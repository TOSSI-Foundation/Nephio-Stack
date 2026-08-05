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

// UPFDataPathSpec is the declarative intent for one edge UPF's node-level N3/N6 datapath. A per-node
// controller (the node-agent, DaemonSet mode of this operator) reconciles it into the host + BESS
// datapath — the pure-Nephio "distributed actuation" (a Go controller, NOT a bash reconcile loop).
type UPFDataPathSpec struct {
	// +required
	UEPool string `json:"uePool"`
	// AccessGateway/CoreGateway: the host's IPs on the N3/N6 bridges (so the host is a first-class node on
	// both L2s: uplink egresses N6, and route_control can resolve the gNB for the downlink).
	// +optional
	AccessGateway string `json:"accessGateway,omitempty"`
	// +optional
	CoreGateway string `json:"coreGateway,omitempty"`
	// CoreNextHop: the UPF's N6 (core) IP the host routes the UE pool toward for downlink.
	// +optional
	CoreNextHop string `json:"coreNextHop,omitempty"`
	// +kubebuilder:default="sdcore-access"
	// +optional
	AccessBridge string `json:"accessBridge,omitempty"`
	// +kubebuilder:default="sdcore-core"
	// +optional
	CoreBridge string `json:"coreBridge,omitempty"`
	// GnbIP: the gNB's N3 IP (the BESS downlink accessRoute target).
	// +optional
	GnbIP string `json:"gnbIP,omitempty"`
	// +kubebuilder:default="eth0"
	// +optional
	EgressNic string `json:"egressNic,omitempty"`
	// +kubebuilder:default="app=upf"
	// +optional
	UPFLabel string `json:"upfLabel,omitempty"`
}

// UPFDataPathStatus is the observed state.
type UPFDataPathStatus struct {
	// LastReconcile is the RFC3339 time the node-agent last drove this datapath to reality.
	// +optional
	LastReconcile string `json:"lastReconcile,omitempty"`
	// Node is the node whose host datapath this reflects.
	// +optional
	Node string `json:"node,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=updp
// +kubebuilder:printcolumn:name="UEPool",type=string,JSONPath=`.spec.uePool`
// +kubebuilder:printcolumn:name="GNB",type=string,JSONPath=`.spec.gnbIP`
// +kubebuilder:printcolumn:name="LastReconcile",type=string,JSONPath=`.status.lastReconcile`

// UPFDataPath is the Schema for the upfdatapaths API.
type UPFDataPath struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`
	// +required
	Spec UPFDataPathSpec `json:"spec"`
	// +optional
	Status UPFDataPathStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// UPFDataPathList contains a list of UPFDataPath.
type UPFDataPathList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []UPFDataPath `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &UPFDataPath{}, &UPFDataPathList{})
		return nil
	})
}
