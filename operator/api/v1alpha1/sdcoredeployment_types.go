/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SDCoreDeploymentSpec defines the desired state of SDCoreDeployment.
type SDCoreDeploymentSpec struct {
	// Component selects which SD-Core chart this deployment renders.
	// +kubebuilder:validation:Enum=controlplane;upf;ransim;subscribers
	// +kubebuilder:default=controlplane
	Component string `json:"component"`

	// Values are freeform Helm values overrides, merged on top of the chart
	// defaults (e.g. PLMN, slices, which NFs to deploy, mongo URL).
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Values *runtime.RawExtension `json:"values,omitempty"`

	// TargetNamespace is where the rendered objects are deployed.
	// Defaults to the SDCoreDeployment's own namespace.
	// +optional
	TargetNamespace string `json:"targetNamespace,omitempty"`
}

// SDCoreDeploymentStatus defines the observed state of SDCoreDeployment.
type SDCoreDeploymentStatus struct {
	// Phase is a coarse lifecycle summary: Pending, Deployed, Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message carries the latest human-readable detail (e.g. helm error).
	// +optional
	Message string `json:"message,omitempty"`

	// ReleaseRevision is the helm release revision last applied.
	// +optional
	ReleaseRevision int `json:"releaseRevision,omitempty"`

	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current state of the SDCoreDeployment resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Component",type=string,JSONPath=`.spec.component`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SDCoreDeployment is the Schema for the sdcoredeployments API
type SDCoreDeployment struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SDCoreDeployment
	// +required
	Spec SDCoreDeploymentSpec `json:"spec"`

	// status defines the observed state of SDCoreDeployment
	// +optional
	Status SDCoreDeploymentStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SDCoreDeploymentList contains a list of SDCoreDeployment
type SDCoreDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SDCoreDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SDCoreDeployment{}, &SDCoreDeploymentList{})
		return nil
	})
}
