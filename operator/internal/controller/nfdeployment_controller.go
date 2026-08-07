/*
Copyright 2026.
Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/storage/driver"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	nephiov1alpha1 "github.com/nephio-project/api/workload/v1alpha1"
)

// NFDeploymentReconciler consumes Nephio's NFDeployment CRs (the SAME API free5GC
// uses), reads the interface IPs the Nephio IPAM/VLAN specializers already
// allocated onto the shared VPC fabric, and renders the matching SD-Core chart
// wired to those IPs — so the distributed CP<->edge datapath auto-wires.
type NFDeploymentReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	RestConfig *rest.Config
}

// providerToComponent maps NFDeployment.spec.provider to an embedded chart.
func providerToComponent(provider string) (string, bool) {
	switch {
	case strings.HasPrefix(provider, "upf."):
		return "upf", true
	case strings.HasPrefix(provider, "smf."), strings.HasPrefix(provider, "amf."),
		strings.HasPrefix(provider, "controlplane."):
		return "controlplane", true
	}
	return "", false
}

// +kubebuilder:rbac:groups=workload.nephio.org,resources=nfdeployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=workload.nephio.org,resources=nfdeployments/status,verbs=get;update;patch

func (r *NFDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var nfd nephiov1alpha1.NFDeployment
	if err := r.Get(ctx, req.NamespacedName, &nfd); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	component, ok := providerToComponent(nfd.Spec.Provider)
	if !ok {
		return ctrl.Result{}, nil // not an SD-Core provider
	}

	values, err := buildValuesFromNFDeployment(component, &nfd)
	if err != nil {
		log.Error(err, "building values from NFDeployment")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	ch, err := loadChart(component)
	if err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	ns := nfd.Namespace
	relName := nfd.Name

	cfg := new(action.Configuration)
	getter := newRESTClientGetter(r.RestConfig, ns)
	if err := cfg.Init(getter, ns, "secret", func(f string, a ...interface{}) { log.Info(fmt.Sprintf(f, a...)) }); err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	hist := action.NewHistory(cfg)
	hist.Max = 1
	if _, herr := hist.Run(relName); herr == driver.ErrReleaseNotFound {
		inst := action.NewInstall(cfg)
		inst.ReleaseName = relName
		inst.Namespace = ns
		inst.CreateNamespace = true
		inst.Timeout = 5 * time.Minute
		if _, err := inst.Run(ch, values); err != nil {
			log.Error(err, "helm install")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
	} else {
		upg := action.NewUpgrade(cfg)
		upg.Namespace = ns
		upg.Timeout = 5 * time.Minute
		if _, err := upg.Run(relName, ch, values); err != nil {
			log.Error(err, "helm upgrade")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
	}

	apimeta.SetStatusCondition(&nfd.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Deployed",
		Message: fmt.Sprintf("SD-Core %s wired from specialized interfaces", component),
	})
	_ = r.Status().Update(ctx, &nfd)
	return ctrl.Result{}, nil
}

// buildValuesFromNFDeployment turns the specialized interface IPs into chart values.
func buildValuesFromNFDeployment(component string, nfd *nephiov1alpha1.NFDeployment) (map[string]interface{}, error) {
	switch component {
	case "upf":
		n3, err := firstIfaceIPv4(nfd.Spec.Interfaces, "n3")
		if err != nil {
			return nil, fmt.Errorf("n3: %w", err)
		}
		n6, err := firstIfaceIPv4(nfd.Spec.Interfaces, "n6")
		if err != nil {
			return nil, fmt.Errorf("n6: %w", err)
		}
		pool := firstPool(nfd.Spec, "n6")
		n3gw := firstIfaceGateway(nfd.Spec.Interfaces, "n3") // N3/access next-hop (e.g. 10.202.0.1)
		n6gw := firstIfaceGateway(nfd.Spec.Interfaces, "n6") // N6/core next-hop (e.g. 10.203.0.1)
		// macvlan master = the edge node's data NICs (per-cluster). N3(access) and N6(core) ride
		// separate host interfaces, so read each from an env var set on the operator Deployment;
		// default eth0. Without a master the NAD renders `"master": ,` and the pod can't get its NIC.
		accessMaster := os.Getenv("UPF_ACCESS_MASTER")
		if accessMaster == "" {
			accessMaster = "eth0"
		}
		coreMaster := os.Getenv("UPF_CORE_MASTER")
		if coreMaster == "" {
			coreMaster = "eth0"
		}
		// CNI plugin for the N3/N6 data NADs. Default `bridge`: when the gNB and UPF share the
		// same host L2 (co-located edge), macvlan siblings on a Linux-bridge master are isolated
		// (ARP never resolves) — bridge CNI attaches both to the bridge and the datapath works.
		// For af_packet UPFs the chart's `iface` value carries the master/bridge NAME.
		cniPlugin := os.Getenv("UPF_CNI_PLUGIN")
		if cniPlugin == "" {
			cniPlugin = "bridge"
		}
		vals := map[string]interface{}{
			"config": map[string]interface{}{
				"upf": map[string]interface{}{
					"privileged": true,
					"hugepage":   map[string]interface{}{"enabled": false},
					"sriov":      map[string]interface{}{"enabled": false},
					// chart's NAD template reads `.iface` for the macvlan master / bridge name
					"access":     map[string]interface{}{"cniPlugin": cniPlugin, "ipam": "static", "ip": n3 + "/24", "iface": accessMaster, "gateway": n3gw},
					"core":       map[string]interface{}{"cniPlugin": cniPlugin, "ipam": "static", "ip": n6 + "/24", "iface": coreMaster, "gateway": n6gw},
					"cfgFiles":   map[string]interface{}{"upf.jsonc": map[string]interface{}{"mode": "af_packet"}},
				},
			},
		}
		if pool != "" {
			cu := vals["config"].(map[string]interface{})["upf"].(map[string]interface{})
			cu["enb"] = map[string]interface{}{"subnet": pool}
		}
		return vals, nil
	case "controlplane":
		n4, _ := firstIfaceIPv4(nfd.Spec.Interfaces, "n4")
		return map[string]interface{}{
			"config": map[string]interface{}{
				"smf": map[string]interface{}{"n4": map[string]interface{}{"ip": n4}},
			},
		}, nil
	}
	return map[string]interface{}{}, nil
}

func firstIfaceIPv4(ifaces []nephiov1alpha1.InterfaceConfig, name string) (string, error) {
	for _, ic := range ifaces {
		if ic.Name == name && ic.IPv4 != nil {
			ip, _, err := net.ParseCIDR(ic.IPv4.Address)
			if err != nil {
				return "", err
			}
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("interface %q not found", name)
}

func firstIfaceGateway(ifaces []nephiov1alpha1.InterfaceConfig, name string) string {
	for _, ic := range ifaces {
		if ic.Name == name && ic.IPv4 != nil && ic.IPv4.Gateway != nil {
			return *ic.IPv4.Gateway
		}
	}
	return ""
}

func firstPool(spec nephiov1alpha1.NFDeploymentSpec, ifaceName string) string {
	for _, ni := range spec.NetworkInstances {
		attached := false
		for _, i := range ni.Interfaces {
			if i == ifaceName {
				attached = true
			}
		}
		if attached {
			for _, dn := range ni.DataNetworks {
				for _, p := range dn.Pool {
					return p.Prefix
				}
			}
		}
	}
	return ""
}

func (r *NFDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.RestConfig == nil {
		r.RestConfig = mgr.GetConfig()
	}
	return ctrl.NewControllerManagedBy(mgr).
		// only reconcile on SPEC changes, not our own status writes -> no helm-upgrade loop
		For(&nephiov1alpha1.NFDeployment{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("sdcore-nfdeployment").
		Complete(r)
}
