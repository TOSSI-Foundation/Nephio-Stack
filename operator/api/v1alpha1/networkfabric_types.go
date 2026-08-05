/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// NetworkFabricSpec is the declarative intent for one cross-site network fabric (N3/N4/N6). It is the
// address plan the Nephio SPECIALIZER draws from: the NetworkReconciler expands it into the low-level
// ipam NetworkInstance + vlan VLANIndex the interface-fn's IPClaims/VLANClaims allocate out of. A simple,
// purpose-built domain intent (unlike upstream infra.nephio.org/Network, whose full topology model we
// don't need) — same pattern as EdgeSite/MeshLink: KRM intent reconciled by a Nephio domain controller.
type NetworkFabricSpec struct {
	// Prefixes are the IP pools this fabric hands out. Each is optionally labeled (e.g.
	// nephio.org/network-name: n3|n4|n6|pool) so the specializer picks the right pool per interface request.
	// +required
	Prefixes []FabricPrefix `json:"prefixes"`
}

// FabricPrefix is one labeled CIDR the fabric allocates from.
type FabricPrefix struct {
	// +required
	Prefix string `json:"prefix"`
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// NetworkFabricStatus is the observed state.
type NetworkFabricStatus struct {
	// +optional
	LastReconcile string `json:"lastReconcile,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=netfabric

// NetworkFabric is the Schema for the networkfabrics API.
type NetworkFabric struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`
	// +required
	Spec NetworkFabricSpec `json:"spec"`
	// +optional
	Status NetworkFabricStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// NetworkFabricList contains a list of NetworkFabric.
type NetworkFabricList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NetworkFabric `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &NetworkFabric{}, &NetworkFabricList{})
		return nil
	})
}
