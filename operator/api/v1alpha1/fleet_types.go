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

// FleetSite is the MINIMAL intent per server — just an IP and a role. Everything else (the site name, its
// cluster, the per-site addressing, all the packages) is DERIVED by the controllers. This is the whole
// point of Nephio here: you declare the least you possibly can.
type FleetSite struct {
	// Server is the bare server's host IP (a BYOH-registered host).
	// +required
	Server string `json:"server"`
	// User is the SSH login the mgmt node uses to onboard this server (push the BYOH agent + bootstrap the
	// edge platform). Empty = the mgmt node's own login. Set it when the target's login differs (e.g. "one").
	// +optional
	User string `json:"user,omitempty"`
	// Role: "cp+upf" = a self-contained 5G core on this server; "upf-only" = an edge UPF steered by the
	// mgmt control plane. Defaults to upf-only.
	// +kubebuilder:validation:Enum=cp+upf;upf-only
	// +kubebuilder:default=upf-only
	// +optional
	Role string `json:"role,omitempty"`
	// Name optionally pins the site/cluster name; if empty it is derived stably from the server IP.
	// +optional
	Name string `json:"name,omitempty"`
	// RAN (optional): deploy an OCUDU gNB on this site, auto-connected to its SD-Core (cp+upf sites only).
	// +optional
	RAN *RANSpec `json:"ran,omitempty"`
}

// FleetSpec is the WHOLE fleet as a single intent: a list of {server, role}. The Fleet controller expands
// each entry into an EdgeSite (which the EdgeSite controller then expands into cluster/CP/UPF packages).
// Add an entry -> a site is onboarded; remove an entry -> that site's EdgeSite is deleted (owner-GC tears
// the whole site down). One tiny declaration; Nephio does the fan-out.
type FleetSpec struct {
	// +required
	Sites []FleetSite `json:"sites"`
}

// FleetStatus reports how many sites are declared and the last reconcile outcome.
type FleetStatus struct {
	// +optional
	Sites int `json:"sites,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Sites",type=integer,JSONPath=`.status.sites`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`

// Fleet is the top-level fleet intent: one object listing every server as {server, role}. It is the single
// source of truth for "which servers run what", and the exact artifact GitOps (Argo CD / Config Sync)
// reconciles.
type Fleet struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`
	// +required
	Spec FleetSpec `json:"spec"`
	// +optional
	Status FleetStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// FleetList contains a list of Fleet.
type FleetList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitzero"`
	Items []Fleet `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Fleet{}, &FleetList{})
		return nil
	})
}
