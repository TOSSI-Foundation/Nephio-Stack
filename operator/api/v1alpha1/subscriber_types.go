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

// SubscriberSpec is one SIM's provisioning intent. The SubscriberReconciler reconciles it into the
// SD-Core webconsole (UDR auth) — the pure-Nephio replacement for the old sdcore-provisioner bash.
type SubscriberSpec struct {
	// +required
	IMSI string `json:"imsi"`
	// +required
	Key string `json:"key"`
	// +required
	Opc string `json:"opc"`
	// SliceRef names the NetworkSlice this SIM belongs to (its IMSI joins that slice's device-group).
	// +optional
	SliceRef string `json:"sliceRef,omitempty"`
	// +optional
	SequenceNumber string `json:"sequenceNumber,omitempty"`
}

// SubscriberStatus is the observed state.
type SubscriberStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sub
// +kubebuilder:printcolumn:name="IMSI",type=string,JSONPath=`.spec.imsi`
// +kubebuilder:printcolumn:name="Slice",type=string,JSONPath=`.spec.sliceRef`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// Subscriber is the Schema for the subscribers API.
type Subscriber struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`
	// +required
	Spec SubscriberSpec `json:"spec"`
	// +optional
	Status SubscriberStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SubscriberList contains a list of Subscriber.
type SubscriberList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Subscriber `json:"items"`
}

// PlmnId is a mcc/mnc pair.
type PlmnId struct {
	// +kubebuilder:default="001"
	// +optional
	Mcc string `json:"mcc,omitempty"`
	// +kubebuilder:default="01"
	// +optional
	Mnc string `json:"mnc,omitempty"`
}

// GnbRef names a gNB + its TAC for the slice site-info (the AMF advertises the served TAI from this).
type GnbRef struct {
	// +required
	Name string `json:"name"`
	// +kubebuilder:default=1
	// +optional
	Tac int `json:"tac,omitempty"`
}

// SliceQos caps the data flow (MBR). Defaults to 1 Gbps up/down.
type SliceQos struct {
	// +optional
	MbrUplink int64 `json:"mbrUplink,omitempty"`
	// +optional
	MbrDownlink int64 `json:"mbrDownlink,omitempty"`
}

// NetworkSliceSpec declares one 5G slice. The NetworkSliceReconciler reconciles it into the webconsole
// as a device-group (its subscribers' IMSIs + UE IP pool) + a network-slice (sst/sd + site-info with
// the PLMN, gNB(s)+TAC, and UPF). The site-info is what makes the AMF advertise the served TAI, which
// is why a gNB's NG Setup succeeds. Pure-Nephio replacement for the sdcore-provisioner bash slice path.
type NetworkSliceSpec struct {
	// +kubebuilder:default="1"
	// +optional
	Sst string `json:"sst,omitempty"`
	// +required
	Sd string `json:"sd"`
	// +kubebuilder:default="internet"
	// +optional
	Dnn string `json:"dnn,omitempty"`
	// +required
	UePool string `json:"uePool"`
	// +kubebuilder:default="upf"
	// +optional
	UpfName string `json:"upfName,omitempty"`
	// +optional
	Plmn PlmnId `json:"plmn,omitempty"`
	// +required
	Gnbs []GnbRef `json:"gnbs"`
	// +optional
	Qos SliceQos `json:"qos,omitempty"`
}

// NetworkSliceStatus is the observed state.
type NetworkSliceStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	Provisioned string `json:"provisioned,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=slice
// +kubebuilder:printcolumn:name="SST",type=string,JSONPath=`.spec.sst`
// +kubebuilder:printcolumn:name="SD",type=string,JSONPath=`.spec.sd`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// NetworkSlice is the Schema for the networkslices API.
type NetworkSlice struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`
	// +required
	Spec NetworkSliceSpec `json:"spec"`
	// +optional
	Status NetworkSliceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// NetworkSliceList contains a list of NetworkSlice.
type NetworkSliceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NetworkSlice `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion,
			&Subscriber{}, &SubscriberList{},
			&NetworkSlice{}, &NetworkSliceList{})
		return nil
	})
}
