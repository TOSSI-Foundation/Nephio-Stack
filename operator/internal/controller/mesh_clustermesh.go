/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	sdcorev1alpha1 "github.com/your-org/sdcore-operator/api/v1alpha1"
)

// ensureClusterMesh reconciles the controller-owned HALF of the Cilium ClusterMesh: the SHARED cilium-ca
// Secret, delivered thesis-pure (controller PRODUCES the KRM, Git is the source of truth, Config Sync
// applies it). One CA root is what lets every cluster's clustermesh-apiserver certs chain together — it
// MUST land on a fresh edge BEFORE its apiserver certs are minted, which holds because Config Sync applies
// the deploy repo before `make edge` [1b/6] stands the apiserver up.
//
// The CONNECTION half (peer endpoints + hostAliases + the cilium-clustermesh/kvstoremesh secrets) is
// DELIBERATELY NOT done here. It lives in the bootstrap helm layer (`make edge` [1b/6]+[1c/6], via the
// chart's clustermesh.config, peers derived from EdgeSite intent) because on CK8s there is no other owner
// that survives: the apiserver certs carry only *.mesh.cilium.io SANs (direct-IP endpoints fail TLS, so
// hostAliases on the chart-owned apiserver Deployment are required), and CK8s's helm reconciler REVERTS
// every non-chart mutation of chart-owned objects — a controller-written secret or patch here silently
// loses that fight (proven live: per-edge secrets also OVERWROTE each other, leaving only the last edge
// meshed). Cilium hot-reloads clustermesh config, so chart-rendered peers apply without restarts.
func (r *MeshLinkReconciler) ensureClusterMesh(ctx context.Context, ml *sdcorev1alpha1.MeshLink, edgeCS, cpCS *kubernetes.Clientset) error {
	if ml.Spec.Edge == "" || ml.Spec.Edge == "mgmt" {
		return nil
	}
	cp := ml.Spec.ControlPlane
	if cp == "" {
		cp = "mgmt"
	}

	// SHARED CA -> the edge's deploy repo. Re-delivering the same CA is a Config-Sync no-op.
	caCP, err := cpCS.CoreV1().Secrets("kube-system").Get(ctx, "cilium-ca", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read control-plane cilium-ca: %w", err)
	}
	if err := r.deliverViaGit(ctx, deployRepoFor(ml.Spec.Edge), "clustermesh/cilium-ca.yaml",
		secretYAML("cilium-ca", "kube-system", caCP.Data)); err != nil {
		return fmt.Errorf("deliver shared cilium-ca to edge repo: %w", err)
	}

	// Retire the legacy controller-written cilium-clustermesh Secrets: the chart renders that Secret now
	// (helm would refuse to adopt an existing non-chart copy, and Config Sync would fight it forever).
	// Removing the file from the deploy repo makes Config Sync PRUNE the live Secret; helm then owns it.
	for _, repo := range []string{deployRepoFor(cp), deployRepoFor(ml.Spec.Edge)} {
		if err := r.removeFromGit(ctx, repo, "clustermesh/cilium-clustermesh.yaml"); err != nil {
			return fmt.Errorf("retire legacy cilium-clustermesh from %s: %w", repo, err)
		}
	}
	_ = edgeCS // edge is reconciled via its Git deploy repo + Config Sync, not touched directly
	return nil
}
