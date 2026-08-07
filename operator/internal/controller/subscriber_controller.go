/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sdcorev1alpha1 "github.com/your-org/sdcore-operator/api/v1alpha1"
)

// The SD-Core webconsole (NMS) config REST API. The operator runs in another namespace, so use the FQDN.
const webuiBase = "http://webui.default.svc.cluster.local:5000"

var httpc = &http.Client{Timeout: 8 * time.Second}

// webuiPost POSTs a JSON body to the webconsole, returning a clear error on non-2xx. Idempotent upsert.
func webuiPost(ctx context.Context, path string, body interface{}) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webuiBase+path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("build request %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpc.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s (is the webui reachable?): %w", path, err)
	}
	defer resp.Body.Close()
	// 409 = already exists: the webconsole POST is an upsert-by-intent, so treat it as idempotent success.
	if resp.StatusCode == http.StatusConflict {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("POST %s -> HTTP %d: %s", path, resp.StatusCode, string(msg))
	}
	return nil
}

// ---------------------------------------------------------------------------------------------------
// SubscriberReconciler — reconciles a Subscriber CR into the webconsole/UDR (per-SIM auth). Pure-Nephio
// replacement for the sdcore-provisioner bash subscriber loop.
// ---------------------------------------------------------------------------------------------------

type SubscriberReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=subscribers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=subscribers/status,verbs=get;update;patch

func (r *SubscriberReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var sub sdcorev1alpha1.Subscriber
	if err := r.Get(ctx, req.NamespacedName, &sub); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if len(sub.Spec.IMSI) < 5 {
		return r.subFail(ctx, &sub, fmt.Errorf("imsi %q too short (need >=5 digits for the PLMN)", sub.Spec.IMSI))
	}
	seq := sub.Spec.SequenceNumber
	if seq == "" {
		seq = "16f3b3f70fc2"
	}
	body := map[string]interface{}{
		"plmnID":         sub.Spec.IMSI[:5],
		"opc":            sub.Spec.Opc,
		"key":            sub.Spec.Key,
		"sequenceNumber": seq,
	}
	if err := webuiPost(ctx, "/api/subscriber/imsi-"+sub.Spec.IMSI, body); err != nil {
		return r.subFail(ctx, &sub, err)
	}
	sub.Status.Phase = "Provisioned"
	sub.Status.Message = fmt.Sprintf("subscriber imsi-%s provisioned into UDR (slice %s)", sub.Spec.IMSI, sub.Spec.SliceRef)
	setCondition(&sub.Status.Conditions, "Ready", metav1.ConditionTrue, "Provisioned", sub.Status.Message, sub.Generation)
	if err := r.Status().Update(ctx, &sub); err != nil {
		log.Error(err, "Subscriber: status update", "imsi", sub.Spec.IMSI)
	}
	log.Info("Subscriber provisioned", "imsi", sub.Spec.IMSI, "slice", sub.Spec.SliceRef)
	// refresh periodically (idempotent) so a webconsole/mongo wipe self-heals.
	return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
}

func (r *SubscriberReconciler) subFail(ctx context.Context, sub *sdcorev1alpha1.Subscriber, err error) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Error(err, "Subscriber provisioning FAILED", "imsi", sub.Spec.IMSI)
	sub.Status.Phase = "Failed"
	sub.Status.Message = err.Error()
	setCondition(&sub.Status.Conditions, "Ready", metav1.ConditionFalse, "ProvisionFailed", err.Error(), sub.Generation)
	if uerr := r.Status().Update(ctx, sub); uerr != nil {
		log.Error(uerr, "Subscriber: also failed status update", "imsi", sub.Spec.IMSI)
	}
	return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
}

func (r *SubscriberReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&sdcorev1alpha1.Subscriber{}).Complete(r)
}

// ---------------------------------------------------------------------------------------------------
// NetworkSliceReconciler — reconciles a NetworkSlice CR into the webconsole: a device-group (its
// subscribers' IMSIs + UE IP pool) + a network-slice (sst/sd + site-info PLMN/gNB+TAC/UPF). The
// site-info is what makes the AMF advertise the served TAI, so the gNB's NG Setup succeeds. Pure-Nephio
// replacement for the sdcore-provisioner bash slice loop.
// ---------------------------------------------------------------------------------------------------

type NetworkSliceReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Config    *rest.Config
	clientset *kubernetes.Clientset
}

// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=networkslices,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=networkslices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create

func (r *NetworkSliceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var ns sdcorev1alpha1.NetworkSlice
	if err := r.Get(ctx, req.NamespacedName, &ns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	s := ns.Spec
	if s.Sst == "" {
		s.Sst = "1"
	}
	if s.Dnn == "" {
		s.Dnn = "internet"
	}
	if s.UpfName == "" {
		s.UpfName = "upf"
	}
	if s.Plmn.Mcc == "" {
		s.Plmn.Mcc = "001"
	}
	if s.Plmn.Mnc == "" {
		s.Plmn.Mnc = "01"
	}
	mbrUp := s.Qos.MbrUplink
	if mbrUp == 0 {
		mbrUp = 1000000000
	}
	mbrDn := s.Qos.MbrDownlink
	if mbrDn == 0 {
		mbrDn = 1000000000
	}

	// gather the IMSIs of every Subscriber referencing this slice (across namespaces).
	var subs sdcorev1alpha1.SubscriberList
	if err := r.List(ctx, &subs); err != nil {
		return r.sliceFail(ctx, &ns, fmt.Errorf("list subscribers: %w", err))
	}
	imsis := []string{}
	for _, sub := range subs.Items {
		if sub.Spec.SliceRef == ns.Name {
			imsis = append(imsis, sub.Spec.IMSI)
		}
	}

	dg := "dg-" + ns.Name
	deviceGroup := map[string]interface{}{
		"imsis":          imsis,
		"site-info":      ns.Name,
		"ip-domain-name": "pool-" + ns.Name,
		"ip-domain-expanded": map[string]interface{}{
			"dnn":         s.Dnn,
			"ue-ip-pool":  s.UePool,
			"dns-primary": "8.8.8.8",
			"mtu":         1400,
			"ue-dnn-qos": map[string]interface{}{
				"dnn-mbr-uplink":   mbrUp,
				"dnn-mbr-downlink": mbrDn,
				"traffic-class":    map[string]interface{}{"name": "platinum", "qci": 9, "arp": 1, "pdb": 300, "pelr": 6},
			},
		},
	}
	if err := webuiPost(ctx, "/config/v1/device-group/"+dg, deviceGroup); err != nil {
		return r.sliceFail(ctx, &ns, err)
	}

	gnbs := []map[string]interface{}{}
	for _, g := range s.Gnbs {
		tac := g.Tac
		if tac == 0 {
			tac = 1
		}
		gnbs = append(gnbs, map[string]interface{}{"name": g.Name, "tac": tac})
	}
	networkSlice := map[string]interface{}{
		"slice-id":          map[string]interface{}{"sst": s.Sst, "sd": s.Sd},
		"site-device-group": []string{dg},
		"site-info": map[string]interface{}{
			"site-name": ns.Name,
			"plmn":      map[string]interface{}{"mcc": s.Plmn.Mcc, "mnc": s.Plmn.Mnc},
			"gNodeBs":   gnbs,
			"upf":       map[string]interface{}{"upf-name": s.UpfName, "upf-port": "8805"},
		},
	}
	if err := webuiPost(ctx, "/config/v1/network-slice/"+ns.Name, networkSlice); err != nil {
		return r.sliceFail(ctx, &ns, err)
	}

	// Work around the SD-Core webconsole DNN-drop bug directly in Mongo (see mongo_helpers.go): restore
	// the device-group's ip-domains so the SMF learns the SNSSAI+DNN->UPF mapping (else it rejects every
	// PDU session "not matched DNN Config"), and clone each subscriber's provisioned data if missing.
	if r.clientset != nil {
		if err := ensureDnnConfig(ctx, r.Config, r.clientset, dg, s.Dnn, s.UePool, mbrUp, mbrDn); err != nil {
			return r.sliceFail(ctx, &ns, err)
		}
		for _, imsi := range imsis {
			if err := ensureProvisionedData(ctx, r.Config, r.clientset, imsi, s.Sd); err != nil {
				return r.sliceFail(ctx, &ns, err)
			}
		}
	}

	ns.Status.Phase = "Provisioned"
	ns.Status.Provisioned = time.Now().UTC().Format(time.RFC3339)
	ns.Status.Message = fmt.Sprintf("slice %s (sst=%s sd=%s) provisioned: %d gNB(s), %d subscriber(s), upf=%s", ns.Name, s.Sst, s.Sd, len(gnbs), len(imsis), s.UpfName)
	setCondition(&ns.Status.Conditions, "Ready", metav1.ConditionTrue, "Provisioned", ns.Status.Message, ns.Generation)
	if err := r.Status().Update(ctx, &ns); err != nil {
		log.Error(err, "NetworkSlice: status update", "slice", ns.Name)
	}
	log.Info("NetworkSlice provisioned", "slice", ns.Name, "sd", s.Sd, "gnbs", len(gnbs), "imsis", len(imsis))
	// requeue to pick up newly-added Subscribers into the device-group.
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *NetworkSliceReconciler) sliceFail(ctx context.Context, ns *sdcorev1alpha1.NetworkSlice, err error) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Error(err, "NetworkSlice provisioning FAILED", "slice", ns.Name)
	ns.Status.Phase = "Failed"
	ns.Status.Message = err.Error()
	setCondition(&ns.Status.Conditions, "Ready", metav1.ConditionFalse, "ProvisionFailed", err.Error(), ns.Generation)
	if uerr := r.Status().Update(ctx, ns); uerr != nil {
		log.Error(uerr, "NetworkSlice: also failed status update", "slice", ns.Name)
	}
	return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
}

func (r *NetworkSliceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Config == nil {
		r.Config = mgr.GetConfig()
	}
	cs, err := kubernetes.NewForConfig(r.Config)
	if err != nil {
		return fmt.Errorf("build clientset for mongo exec: %w", err)
	}
	r.clientset = cs
	// Watch Subscribers and re-reconcile the slice each one references. Without this the slice and its
	// subscribers race: a slice reconciled BEFORE its Subscribers exist posts a device-group with
	// "imsis": [] to the webconsole and nothing ever refreshes it — the AMF then rejects every UE on
	// that slice with FIVEG_SERVICES_NOT_ALLOWED even though both CRs say Provisioned.
	return ctrl.NewControllerManagedBy(mgr).
		For(&sdcorev1alpha1.NetworkSlice{}).
		Watches(&sdcorev1alpha1.Subscriber{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, o client.Object) []reconcile.Request {
				sub, ok := o.(*sdcorev1alpha1.Subscriber)
				if !ok || sub.Spec.SliceRef == "" {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Namespace: sub.Namespace, Name: sub.Spec.SliceRef,
				}}}
			})).
		Complete(r)
}
