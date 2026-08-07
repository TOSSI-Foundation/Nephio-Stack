/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sdcorev1alpha1 "github.com/your-org/sdcore-operator/api/v1alpha1"
)

var (
	networkInstanceGVK = schema.GroupVersionKind{Group: "ipam.resource.nephio.org", Version: "v1alpha1", Kind: "NetworkInstance"}
	vlanIndexGVK       = schema.GroupVersionKind{Group: "vlan.resource.nephio.org", Version: "v1alpha1", Kind: "VLANIndex"}
)

// NetworkReconciler turns a NetworkFabric intent into the low-level resource-backend databases the
// specializer allocates from:
//   - an ipam NetworkInstance carrying the fabric's prefixes (+ nephio.org/network-name labels), so the
//     interface-fn's IPClaims resolve instead of failing "db not initialized";
//   - a vlan VLANIndex (the named VLAN pool) the VLANClaims draw from.
// This is the Nephio domain controller that stands in for the missing upstream network controller — the
// fabric stays PURE KRM intent (fabric/network-intent.yaml) reconciled by a controller, with no
// hand-written NetworkInstance/VLANIndex YAML and no imperative kubectl-apply of backend pools.
type NetworkReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=networkfabrics,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=networkfabrics/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ipam.resource.nephio.org,resources=networkinstances,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=vlan.resource.nephio.org,resources=vlanindices,verbs=get;list;watch;create;update;patch

func (r *NetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var nf sdcorev1alpha1.NetworkFabric
	if err := r.Get(ctx, req.NamespacedName, &nf); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ns := nf.Namespace
	if ns == "" {
		ns = "default"
	}

	// [1] the ipam NetworkInstance carrying the fabric's prefixes (+ network-name labels).
	prefixes := make([]interface{}, 0, len(nf.Spec.Prefixes))
	for _, p := range nf.Spec.Prefixes {
		entry := map[string]interface{}{"prefix": p.Prefix}
		if len(p.Labels) > 0 {
			labels := map[string]interface{}{}
			for k, v := range p.Labels {
				labels[k] = v
			}
			entry["labels"] = labels
		}
		prefixes = append(prefixes, entry)
	}
	ni := &unstructured.Unstructured{}
	ni.SetGroupVersionKind(networkInstanceGVK)
	ni.SetName(nf.Name)
	ni.SetNamespace(ns)
	if err := unstructured.SetNestedSlice(ni.Object, prefixes, "spec", "prefixes"); err != nil {
		return ctrl.Result{}, fmt.Errorf("build NetworkInstance %s prefixes: %w", nf.Name, err)
	}
	if err := r.ensure(ctx, ni); err != nil {
		return r.fail(ctx, &nf, fmt.Errorf("reconcile NetworkInstance %s: %w", nf.Name, err))
	}

	// [2] the vlan VLANIndex — the named VLAN database the VLANClaims allocate from.
	vi := &unstructured.Unstructured{}
	vi.SetGroupVersionKind(vlanIndexGVK)
	vi.SetName(nf.Name)
	vi.SetNamespace(ns)
	_ = unstructured.SetNestedMap(vi.Object, map[string]interface{}{}, "spec")
	if err := r.ensure(ctx, vi); err != nil {
		return r.fail(ctx, &nf, fmt.Errorf("reconcile VLANIndex %s: %w", nf.Name, err))
	}

	nf.Status.LastReconcile = time.Now().UTC().Format(time.RFC3339)
	setCondition(&nf.Status.Conditions, "Ready", metav1.ConditionTrue, "Reconciled",
		fmt.Sprintf("backend NetworkInstance + VLANIndex '%s' ready (%d prefixes)", nf.Name, len(prefixes)), nf.Generation)
	if err := r.Status().Update(ctx, &nf); err != nil {
		log.Error(err, "NetworkFabric status update", "fabric", nf.Name)
	}
	log.Info("reconciled network fabric into backend", "fabric", nf.Name, "namespace", ns)
	return ctrl.Result{}, nil
}

func (r *NetworkReconciler) fail(ctx context.Context, nf *sdcorev1alpha1.NetworkFabric, err error) (ctrl.Result, error) {
	setCondition(&nf.Status.Conditions, "Ready", metav1.ConditionFalse, "ReconcileFailed", err.Error(), nf.Generation)
	_ = r.Status().Update(ctx, nf)
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// ensure create-or-updates an unstructured backend resource, writing only when the spec differs
// (idempotent). Mirrors EdgeSiteReconciler.applyOwned's manual pattern, since controllerutil doesn't
// resolve under the pinned controller-runtime.
func (r *NetworkReconciler) ensure(ctx context.Context, obj *unstructured.Unstructured) error {
	cur := obj.DeepCopy()
	err := r.Get(ctx, client.ObjectKeyFromObject(obj), cur)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	want, _, _ := unstructured.NestedMap(obj.Object, "spec")
	have, _, _ := unstructured.NestedMap(cur.Object, "spec")
	if reflect.DeepEqual(want, have) {
		return nil // unchanged
	}
	obj.SetResourceVersion(cur.GetResourceVersion())
	return r.Update(ctx, obj)
}

func (r *NetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&sdcorev1alpha1.NetworkFabric{}).Complete(r)
}
