/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sdcorev1alpha1 "github.com/your-org/sdcore-operator/api/v1alpha1"
)

// FleetReconciler expands a Fleet (a list of {server, role}) into one EdgeSite per entry. It is the
// top of the Nephio intent hierarchy here: Fleet -> EdgeSite -> (cluster + CP/UPF PackageVariants) ->
// Porch/Config Sync -> running clusters. The user declares only server + role; every EdgeSite (name,
// labels, colocated, addressing) is derived. Each EdgeSite is OWNED by the Fleet, and any managed
// EdgeSite whose entry was removed from the list is deleted (owner-GC then tears that whole site down) —
// so add-a-line onboards a site and remove-a-line decommissions it, purely declaratively.
type FleetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const fleetLabel = "fleet.sdcore.nephio.io/fleet"

// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=fleets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=fleets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=edgesites,verbs=get;list;watch;create;update;patch;delete

func (r *FleetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var fleet sdcorev1alpha1.Fleet
	if err := r.Get(ctx, req.NamespacedName, &fleet); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 1) desired EdgeSites, one per fleet entry (name derived from the IP if not pinned).
	desired := map[string]sdcorev1alpha1.FleetSite{}
	for _, s := range fleet.Spec.Sites {
		if s.Server == "" {
			continue
		}
		name := s.Name
		if name == "" {
			name = "site-" + strings.ReplaceAll(s.Server, ".", "-")
		}
		desired[name] = s
	}

	// 2) create-or-update each desired EdgeSite, owned by this Fleet. Iterate in a STABLE, sorted order and
	//    CONTINUE past a failing site (collect errors) instead of aborting on the first. A Go map has random
	//    iteration order, so the old "range desired + return on first error" made WHICH sites got reconciled
	//    nondeterministic run-to-run — and combined with per-site N2-VIP allocation that reads other sites'
	//    already-persisted VIPs, ordering must be stable for the collision-free guarantee to hold.
	names := make([]string, 0, len(desired))
	for name := range desired {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []string
	for _, name := range names {
		s := desired[name]
		role := s.Role
		if role == "" {
			role = "upf-only"
		}
		es := &sdcorev1alpha1.EdgeSite{}
		es.Name = name
		es.Namespace = fleet.Namespace
		if err := r.applyEdgeSite(ctx, &fleet, es, s.Server, s.User, role, s.RAN); err != nil {
			log.Error(err, "Fleet: EdgeSite reconcile failed", "fleet", fleet.Name, "edgesite", name)
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
		}
	}

	// 3) delete managed EdgeSites whose entry was removed from the fleet (declarative decommission). Runs
	//    regardless of (2) errors so a decommission is never blocked by an unrelated create failure.
	var owned sdcorev1alpha1.EdgeSiteList
	if err := r.List(ctx, &owned, client.InNamespace(fleet.Namespace),
		client.MatchingLabels{fleetLabel: fleet.Name}); err != nil {
		return r.fail(ctx, &fleet, fmt.Errorf("list managed EdgeSites: %w", err))
	}
	for i := range owned.Items {
		es := &owned.Items[i]
		if _, keep := desired[es.Name]; keep || es.DeletionTimestamp != nil {
			continue
		}
		log.Info("Fleet: entry removed -> decommissioning site", "fleet", fleet.Name, "edgesite", es.Name)
		if err := r.Delete(ctx, es); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "Fleet: EdgeSite delete failed", "fleet", fleet.Name, "edgesite", es.Name)
			errs = append(errs, fmt.Sprintf("delete %s: %v", es.Name, err))
		}
	}

	fleet.Status.Sites = len(desired)
	if len(errs) > 0 {
		msg := fmt.Sprintf("%d/%d site(s) reconciled; errors: %s", len(desired)-len(errs), len(desired), strings.Join(errs, "; "))
		fleet.Status.Message = msg
		setCondition(&fleet.Status.Conditions, "Ready", metav1.ConditionFalse, "PartialFailure", msg, fleet.Generation)
		_ = r.Status().Update(ctx, &fleet)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	fleet.Status.Message = fmt.Sprintf("%d site(s) reconciled into EdgeSites", len(desired))
	setCondition(&fleet.Status.Conditions, "Ready", metav1.ConditionTrue, "Reconciled", fleet.Status.Message, fleet.Generation)
	if err := r.Status().Update(ctx, &fleet); err != nil {
		log.Error(err, "Fleet: status update", "fleet", fleet.Name)
	}
	return ctrl.Result{}, nil
}

// applyEdgeSite create-or-updates an EdgeSite for one fleet entry, owned+labeled by the Fleet. Only the
// user-declared fields (server, role) are set; the EdgeSite controller derives everything else.
func (r *FleetReconciler) applyEdgeSite(ctx context.Context, fleet *sdcorev1alpha1.Fleet, es *sdcorev1alpha1.EdgeSite, server, user, role string, ran *sdcorev1alpha1.RANSpec) error {
	t := true
	var cur sdcorev1alpha1.EdgeSite
	err := r.Get(ctx, client.ObjectKeyFromObject(es), &cur)
	if apierrors.IsNotFound(err) {
		es.Labels = map[string]string{fleetLabel: fleet.Name, "fleet.sdcore.nephio.io/managed": "true"}
		es.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: sdcorev1alpha1.SchemeGroupVersion.String(), Kind: "Fleet",
			Name: fleet.Name, UID: fleet.UID, Controller: &t, BlockOwnerDeletion: &t,
		}}
		es.Spec.Server = server
		es.Spec.User = user
		es.Spec.Role = role
		es.Spec.Colocated = false
		es.Spec.RAN = ran
		return r.Create(ctx, es)
	}
	if err != nil {
		return err
	}
	// update in place: keep the derived/persisted addressing, only reassert the declared intent.
	if cur.Labels == nil {
		cur.Labels = map[string]string{}
	}
	cur.Labels[fleetLabel] = fleet.Name
	cur.Labels["fleet.sdcore.nephio.io/managed"] = "true"
	cur.Spec.Server = server
	cur.Spec.User = user
	cur.Spec.Role = role
	cur.Spec.RAN = ran
	return r.Update(ctx, &cur)
}

func (r *FleetReconciler) fail(ctx context.Context, fleet *sdcorev1alpha1.Fleet, err error) (ctrl.Result, error) {
	fleet.Status.Message = err.Error()
	setCondition(&fleet.Status.Conditions, "Ready", metav1.ConditionFalse, "Failed", err.Error(), fleet.Generation)
	_ = r.Status().Update(ctx, fleet)
	return ctrl.Result{}, err
}

func (r *FleetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sdcorev1alpha1.Fleet{}).
		Owns(&sdcorev1alpha1.EdgeSite{}).
		Complete(r)
}
