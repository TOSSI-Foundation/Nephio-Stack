/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// requeueMesh is how often the MeshLink reconciler re-checks the live edge UPF pod IP and re-wires N4
// (the edge UPF pod IP changes on restart). Short enough to track churn, long enough to stay quiet.
const requeueMesh = 20 * time.Second

func intstr38412() intstr.IntOrString { return intstr.FromInt(38412) }
func intstr8805() intstr.IntOrString  { return intstr.FromInt(8805) }

// setCondition records a clear, deduplicated status Condition on any of our CRs (shared by the
// EdgeSite + MeshLink reconcilers) so `kubectl get ... -o yaml` always shows precisely what happened
// (Type/Status/Reason/Message + the generation it applies to).
func setCondition(conds *[]metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string, generation int64) {
	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}
