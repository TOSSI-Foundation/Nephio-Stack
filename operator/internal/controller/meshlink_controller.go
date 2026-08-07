/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sdcorev1alpha1 "github.com/your-org/sdcore-operator/api/v1alpha1"
)

// meshFinalizer gates deletion so the remote (peer-cluster) N2/N4 Services + EndpointSlice — which carry NO
// owner-ref (they live in OTHER clusters, unreachable by mgmt-cluster owner-GC) — are torn down before the
// MeshLink object is removed. Without it, deleting a MeshLink orphaned upf-<edge> Service/EndpointSlice +
// amf-cp Services on the CP cluster forever.
const meshFinalizer = "sdcore.nephio.io/meshlink-cleanup"

// MeshLinkReconciler is the KRM face of the cross-cluster fabric — the pure-controller replacement
// for mesh-connect.sh + the fabric bash loop. It reconciles a MeshLink (edge -> controlPlane) into:
//   - N2: a global `amf-cp` Service exported by the CP cluster + consumed on the edge (SCTP/NGAP)
//   - N4: a CP-side `upf-<edge>` Service whose EndpointSlice tracks the LIVE edge UPF pod IP (PFCP)
// The one-time Cilium ClusterMesh bring-up (identity+SCTP via helm, shared CA, `clustermesh connect`)
// is delegated to a privileged bootstrap Job (Cilium ClusterMesh needs host helm/cilium — the one
// piece Nephio's declarative model can't express; a controller owning it is the idiomatic answer).
type MeshLinkReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=meshlinks,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=meshlinks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

func (r *MeshLinkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var ml sdcorev1alpha1.MeshLink
	if err := r.Get(ctx, req.NamespacedName, &ml); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion: tear down the peer-cluster N2/N4 resources (no owner-ref, so mgmt-cluster GC can't reach them)
	// before releasing the finalizer. Best-effort — a peer cluster already gone must NOT block deletion.
	if !ml.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&ml, meshFinalizer) {
			r.cleanupRemote(ctx, &ml)
			controllerutil.RemoveFinalizer(&ml, meshFinalizer)
			if err := r.Update(ctx, &ml); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}
	if controllerutil.AddFinalizer(&ml, meshFinalizer) {
		if err := r.Update(ctx, &ml); err != nil {
			return ctrl.Result{}, err
		}
	}

	// client for the edge cluster and for the control-plane cluster (mgmt = in-cluster).
	edgeCS, err := r.clusterClient(ctx, ml.Spec.Edge)
	if err != nil {
		return r.fail(ctx, &ml, "edge client", err)
	}
	cpCS, err := r.clusterClient(ctx, ml.Spec.ControlPlane)
	if err != nil {
		return r.fail(ctx, &ml, "control-plane client", err)
	}

	// ClusterMesh — thesis-native: GENERATE the KRM the mesh reduces to (shared cilium-ca + the
	// cilium-clustermesh connection Secrets) and reconcile it onto both clusters. No Job, no cilium CLI.
	// (cluster.id/name + clustermesh.useAPIServer come from the cluster blueprint's cilium helm values.)
	if err := r.ensureClusterMesh(ctx, &ml, edgeCS, cpCS); err != nil {
		return r.fail(ctx, &ml, "clustermesh", err)
	}

	// N2 — amf-cp global service: CP exports (shared true), edge consumes (shared false).
	if err := ensureAmfGlobal(ctx, cpCS, "true"); err != nil {
		return r.fail(ctx, &ml, "amf-cp export", err)
	}
	if err := ensureAmfGlobal(ctx, edgeCS, "false"); err != nil {
		return r.fail(ctx, &ml, "amf-cp consume", err)
	}

	// N4 — upf-<edge>: a CP Service + EndpointSlice pointing at the LIVE edge UPF pod IP over the mesh.
	upfName := ml.Spec.UPFName
	if upfName == "" {
		upfName = "upf-" + ml.Spec.Edge
	}
	upfIP, err := podIP(ctx, edgeCS, "app=upf")
	if err == nil && upfIP != "" {
		if err := ensureN4Endpoint(ctx, cpCS, upfName, upfIP); err != nil {
			return r.fail(ctx, &ml, "N4 endpoint", err)
		}
	}

	ml.Status.Phase = "N4Wired"
	ml.Status.Message = fmt.Sprintf("N2 amf-cp + N4 %s wired (%s <-> %s)", upfName, ml.Spec.Edge, ml.Spec.ControlPlane)
	setCondition(&ml.Status.Conditions, "Ready", metav1.ConditionTrue, "Wired",
		fmt.Sprintf("N2 amf-cp + N4 %s wired between %s and %s", upfName, ml.Spec.Edge, ml.Spec.ControlPlane), ml.Generation)
	if err := r.Status().Update(ctx, &ml); err != nil {
		log.Error(err, "MeshLink: failed to write success status", "edge", ml.Spec.Edge)
		return ctrl.Result{RequeueAfter: requeueMesh}, nil
	}
	log.Info("MeshLink reconciled OK", "edge", ml.Spec.Edge, "controlPlane", ml.Spec.ControlPlane, "n4", upfName)
	return ctrl.Result{RequeueAfter: requeueMesh}, nil
}

// clusterClient returns a kubernetes clientset for a named cluster: "mgmt" (or empty) = in-cluster;
// anything else = the CAPI kubeconfig secret cluster-<name>-kubeconfig on the mgmt cluster.
func (r *MeshLinkReconciler) clusterClient(ctx context.Context, name string) (*kubernetes.Clientset, error) {
	if name == "" || name == "mgmt" {
		cfg, err := ctrl.GetConfig()
		if err != nil {
			return nil, err
		}
		return kubernetes.NewForConfig(cfg)
	}
	var sec corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "cluster-" + name + "-kubeconfig"}, &sec); err != nil {
		return nil, err
	}
	raw, ok := sec.Data["value"]
	if !ok {
		return nil, fmt.Errorf("kubeconfig secret for %s has no 'value'", name)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

func ensureAmfGlobal(ctx context.Context, cs *kubernetes.Clientset, shared string) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "amf-cp", Namespace: "default",
			Annotations: map[string]string{"service.cilium.io/global": "true", "service.cilium.io/shared": shared},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "amf"},
			Ports:    []corev1.ServicePort{{Name: "n2", Protocol: corev1.ProtocolSCTP, Port: 38412, TargetPort: intstr38412()}},
		},
	}
	return apply(ctx, cs, svc)
}

func ensureN4Endpoint(ctx context.Context, cs *kubernetes.Clientset, name, ip string) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "pfcp", Protocol: corev1.ProtocolUDP, Port: 8805, TargetPort: intstr8805()}}},
	}
	if err := apply(ctx, cs, svc); err != nil {
		return err
	}
	ready := true
	port := int32(8805)
	pName := "pfcp"
	proto := corev1.ProtocolUDP
	eps := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: name + "-x", Namespace: "default", Labels: map[string]string{"kubernetes.io/service-name": name}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{ip}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}},
		Ports:       []discoveryv1.EndpointPort{{Name: &pName, Protocol: &proto, Port: &port}},
	}
	_, err := cs.DiscoveryV1().EndpointSlices("default").Get(ctx, eps.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cs.DiscoveryV1().EndpointSlices("default").Create(ctx, eps, metav1.CreateOptions{})
		return err
	}
	_, err = cs.DiscoveryV1().EndpointSlices("default").Update(ctx, eps, metav1.UpdateOptions{})
	return err
}

// cleanupRemote best-effort deletes the peer-cluster resources this MeshLink created: the CP-side
// upf-<edge> Service + its hand-made EndpointSlice + the amf-cp export Service, and the edge-side amf-cp
// consume Service. Errors are swallowed (a NotFound or an unreachable/torn-down peer cluster must not block
// finalizer removal — the whole point is that deleting the MeshLink never wedges).
func (r *MeshLinkReconciler) cleanupRemote(ctx context.Context, ml *sdcorev1alpha1.MeshLink) {
	log := logf.FromContext(ctx)
	upfName := ml.Spec.UPFName
	if upfName == "" {
		upfName = "upf-" + ml.Spec.Edge
	}
	if cpCS, err := r.clusterClient(ctx, ml.Spec.ControlPlane); err == nil {
		_ = cpCS.CoreV1().Services("default").Delete(ctx, upfName, metav1.DeleteOptions{})
		_ = cpCS.DiscoveryV1().EndpointSlices("default").Delete(ctx, upfName+"-x", metav1.DeleteOptions{})
		_ = cpCS.CoreV1().Services("default").Delete(ctx, "amf-cp", metav1.DeleteOptions{})
	} else {
		log.Info("MeshLink cleanup: CP cluster unreachable, skipping remote delete", "cp", ml.Spec.ControlPlane)
	}
	if edgeCS, err := r.clusterClient(ctx, ml.Spec.Edge); err == nil {
		_ = edgeCS.CoreV1().Services("default").Delete(ctx, "amf-cp", metav1.DeleteOptions{})
	}
}

func podIP(ctx context.Context, cs *kubernetes.Clientset, selector string) (string, error) {
	pods, err := cs.CoreV1().Pods("default").List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil || len(pods.Items) == 0 {
		return "", err
	}
	return pods.Items[0].Status.PodIP, nil
}

func apply(ctx context.Context, cs *kubernetes.Clientset, svc *corev1.Service) error {
	_, err := cs.CoreV1().Services("default").Get(ctx, svc.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cs.CoreV1().Services("default").Create(ctx, svc, metav1.CreateOptions{})
		return err
	}
	// keep ClusterIP stable: fetch, patch spec fields we own
	cur, err := cs.CoreV1().Services("default").Get(ctx, svc.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	cur.Annotations = svc.Annotations
	cur.Spec.Selector = svc.Spec.Selector
	cur.Spec.Ports = svc.Spec.Ports
	_, err = cs.CoreV1().Services("default").Update(ctx, cur, metav1.UpdateOptions{})
	return err
}

// fail records a precise MeshLink failure: logs the exact step, sets Phase=Failed + a Ready=False
// condition naming the step and error, and requeues (mesh state is retried, not fatal).
func (r *MeshLinkReconciler) fail(ctx context.Context, ml *sdcorev1alpha1.MeshLink, what string, err error) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Error(err, "MeshLink step FAILED", "edge", ml.Spec.Edge, "controlPlane", ml.Spec.ControlPlane, "step", what)
	ml.Status.Phase = "Failed"
	ml.Status.Message = fmt.Sprintf("%s: %v", what, err)
	setCondition(&ml.Status.Conditions, "Ready", metav1.ConditionFalse, what+"Failed",
		fmt.Sprintf("%s: %v", what, err), ml.Generation)
	if uerr := r.Status().Update(ctx, ml); uerr != nil {
		log.Error(uerr, "MeshLink: also failed to write failure status", "edge", ml.Spec.Edge)
	}
	return ctrl.Result{RequeueAfter: requeueMesh}, nil
}

func (r *MeshLinkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&sdcorev1alpha1.MeshLink{}).Complete(r)
}
