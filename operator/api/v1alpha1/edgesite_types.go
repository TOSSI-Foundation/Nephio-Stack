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

// EdgeSiteSpec is the ONE high-level intent per 5G site. The EdgeSite controller expands it into
// Nephio KRM (a cluster PackageVariant, CP/UPF NFDeployment PackageVariants, IPClaims for the
// per-site addressing, and a MeshLink) — no imperative scripts. Porch + specializers + Config Sync
// then materialize the cluster + control plane and/or UPF onto the server.
type EdgeSiteSpec struct {
	// Server is the host IP (a BYOH-registered bare server) the site's cluster runs on.
	// +required
	Server string `json:"server"`

	// User is the SSH login the mgmt node uses to onboard this server. Empty = the mgmt node's own login.
	// +optional
	User string `json:"user,omitempty"`

	// Role: "cp+upf" = a standalone SD-Core control plane + a local UPF on this server;
	// "upf-only" = an edge UPF that attaches to ControlPlane's CP over the ClusterMesh.
	// +kubebuilder:validation:Enum=cp+upf;upf-only
	// +kubebuilder:default=upf-only
	Role string `json:"role"`

	// ControlPlane (upf-only only): the name of the cp+upf EdgeSite whose CP this edge attaches to.
	// +optional
	ControlPlane string `json:"controlPlane,omitempty"`

	// Colocated: deploy onto the EXISTING mgmt cluster instead of provisioning a new one (for a
	// cp+upf on the management VM itself — N4 is local, no cluster PackageVariant, no MeshLink).
	// +optional
	Colocated bool `json:"colocated,omitempty"`

	// The addressing fields below are OPTIONAL: if omitted the IPAM specializer allocates
	// non-overlapping values from the site pool (the Nephio-native replacement for hand-picked CIDRs).
	// +optional
	PodCIDR string `json:"podCIDR,omitempty"`
	// +optional
	SliceSD string `json:"sliceSD,omitempty"`
	// +optional
	UEPool string `json:"uePool,omitempty"`
	// N3/N6 datapath addressing — all auto-derived from the site index if omitted (the UPF blueprint's
	// apply-replacements + the emitted UPFDataPath consume them).
	// +optional
	AccessCIDR string `json:"accessCIDR,omitempty"`
	// +optional
	CoreCIDR string `json:"coreCIDR,omitempty"`
	// +optional
	AccessGw string `json:"accessGw,omitempty"`
	// +optional
	CoreGw string `json:"coreGw,omitempty"`
	// +optional
	CoreNextHop string `json:"coreNextHop,omitempty"`
	// +optional
	GnbIP string `json:"gnbIP,omitempty"`

	// RanSubnet is the SHARED RAN L2 pool CIDR (same on every site); the n2-expose LB pool draws from it.
	// Auto-defaulted if omitted.
	// +optional
	RanSubnet string `json:"ranSubnet,omitempty"`
	// N2VIP is this site's DISTINCT NGAP VIP (a bare IP within RanSubnet), pinned on the amf-ngap Service so
	// multiple CP-bearing sites on one RAN L2 never collide. A mid-range IP from the shared pool (not a /32
	// pool) — sidesteps Cilium LB-IPAM's first/last-IP exclusion. Auto-allocated (lowest free) if omitted.
	// +optional
	N2VIP string `json:"n2VIP,omitempty"`
	// RanNic is the site node's RAN-facing NIC (the node-agent announces the NGAP VIP on it).
	// +optional
	RanNic string `json:"ranNic,omitempty"`

	// EgressNic is the site node's internet-facing NIC for N6 breakout.
	// +kubebuilder:default=eth0
	// +optional
	EgressNic string `json:"egressNic,omitempty"`

	// RAN (optional): deploy an OCUDU gNB on this site, AUTO-CONNECTED to this site's SD-Core. You declare
	// only the radio bits; the AMF, PLMN, TAC, SD and N3 are derived from the site's core. Only meaningful
	// on a cp+upf site (the gNB needs a local AMF/UPF to attach to).
	// +optional
	RAN *RANSpec `json:"ran,omitempty"`
}

// RANSpec is the minimal RAN intent — the gNB software stack, the O-RAN split, the radio device, and the
// tweakable NR cell parameters. Everything that ties the gNB to the core is derived, not declared.
type RANSpec struct {
	// Stack: the gNB software. "ocudu" = the official OCUDU project (srsRAN's LF successor).
	// +kubebuilder:validation:Enum=ocudu
	// +kubebuilder:default=ocudu
	// +optional
	Stack string `json:"stack,omitempty"`
	// Split: the O-RAN functional split. "8" = the SDR is driven directly (device below); "7.2" = Open
	// Fronthaul to an external O-RU.
	// +kubebuilder:validation:Enum="8";"7.2"
	// +kubebuilder:default="8"
	// +optional
	Split string `json:"split,omitempty"`
	// Device: the radio for split 8. "zmq" = RF simulator (NO hardware — runs on a VM, provable end-to-end);
	// "n310"/"b210"/"x310" = a USRP SDR (needs the device + host real-time tuning). A non-zmq device makes
	// the gNB run hostNetwork + privileged with the host's UHD/DPDK/MKL libs mounted (it drives real RF).
	// +kubebuilder:validation:Enum=zmq;n310;b210;x310
	// +kubebuilder:default=zmq
	// +optional
	Device string `json:"device,omitempty"`
	// Config: a full OCUDU gnb.yaml for a REAL radio (the ru_sdr + cell_cfg + everything). RF is hardware-
	// and deployment-specific, so it is NOT baked into the operator — you provide your proven config here.
	// The operator deploys it as-is with only the AMF resolved: set `amf.addr: AMF_ADDR` (auto-substituted
	// with the core's AMF), and tac/sd/plmn to the fleet values (tac 1, sd 102030, plmn 00101). When empty,
	// the operator generates a simple self-consistent config from `cell` (used for the zmq simulator).
	// +optional
	Config string `json:"config,omitempty"`
	// LdLibraryPath: for a real-radio (UHD) gNB, the LD_LIBRARY_PATH the host binary needs (UHD/DPDK/MKL).
	// Defaults to the standard UHD + Intel oneAPI install layout; override if your host differs.
	// +optional
	LdLibraryPath string `json:"ldLibraryPath,omitempty"`
	// Cpuset: for a real-radio (UHD) gNB, the isolated CPU cores to pin the gNB to (taskset -c). Real-time
	// RF needs the process on isolated cores (host isolcpus) or the L1 jitters (PRACH loss / underflow).
	// Defaults to "0-15". Ignored for zmq.
	// +optional
	Cpuset string `json:"cpuset,omitempty"`
	// RadioAddr: OPTIONAL override for the SDR's streaming address. The detect initContainer auto-discovers
	// the radio (uhd_find_devices) — set this only when it can't be inferred, e.g. an N310 whose 10G data
	// SFP is on a different subnet than its mgmt port. USB radios (b210) need nothing here.
	// +optional
	RadioAddr string `json:"radioAddr,omitempty"`
	// RadioNic: for a NETWORKED SDR (N310/X310), the host NIC on the radio's data L2. The (non-hostNetwork)
	// gNB gets a jumbo macvlan on it so it can stream to the radio at MTU 9000 — a plain cluster-CNI pod
	// (1500 MTU, masqueraded) can't carry the radio traffic. USB (b210) / fronthaul (O-RU) leave this empty.
	// +optional
	RadioNic string `json:"radioNic,omitempty"`
	// Cell: NR cell radio parameters you tweak by hand (defaults suit band n78, 100 MHz, 30 kHz SCS).
	// Used only when `config` is empty (the generated/zmq path).
	// +optional
	Cell RANCell `json:"cell,omitempty"`
	// Ofh: Open Fronthaul (split 7.2) parameters. Only used when split=="7.2" — points the DU at an
	// external O-RU over a raw-socket/DPDK fronthaul NIC. Requires real hardware + PTP (Phase 3).
	// +optional
	Ofh *RANOfh `json:"ofh,omitempty"`
}

// RANOfh carries the split-7.2 Open Fronthaul parameters for driving an external O-RU (e.g. LiteOn).
type RANOfh struct {
	// Interface: the fronthaul NIC name (raw-socket mode) or PCIe address like "0000:01:00.1" (DPDK mode).
	// +optional
	Interface string `json:"interface,omitempty"`
	// RuMac: the O-RU's MAC address (fronthaul destination).
	// +optional
	RuMac string `json:"ruMac,omitempty"`
	// DuMac: the DU/host fronthaul NIC MAC address (fronthaul source).
	// +optional
	DuMac string `json:"duMac,omitempty"`
	// VlanTag: the C-plane/U-plane VLAN tag on the fronthaul (1..4094).
	// +kubebuilder:default=3
	// +optional
	VlanTag int `json:"vlanTag,omitempty"`
}

// RANCell holds the hand-tweakable NR cell radio parameters (these are the "change some things manually"
// knobs — everything core-facing is derived).
type RANCell struct {
	// +kubebuilder:default=n78
	// +optional
	Band string `json:"band,omitempty"`
	// +kubebuilder:default=632628
	// +optional
	DlArfcn int `json:"dlArfcn,omitempty"`
	// +kubebuilder:default=100
	// +optional
	BandwidthMHz int `json:"bandwidthMHz,omitempty"`
	// +kubebuilder:default=30
	// +optional
	CommonScs int `json:"commonScs,omitempty"`
	// +kubebuilder:default=1
	// +optional
	Pci int `json:"pci,omitempty"`
}

// EdgeSiteStatus is the observed state.
type EdgeSiteStatus struct {
	// Phase: Provisioning, ClusterReady, Deploying, Ready, Failed.
	// +optional
	Phase string `json:"phase,omitempty"`
	// Message carries the latest human-readable detail.
	// +optional
	Message string `json:"message,omitempty"`
	// Cluster is the CAPI/Porch cluster name this site provisioned (cluster-<name>).
	// +optional
	Cluster string `json:"cluster,omitempty"`
	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=edge
// +kubebuilder:printcolumn:name="Server",type=string,JSONPath=`.spec.server`
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.role`
// +kubebuilder:printcolumn:name="CP",type=string,JSONPath=`.spec.controlPlane`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// EdgeSite is the Schema for the edgesites API.
type EdgeSite struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`
	// +required
	Spec EdgeSiteSpec `json:"spec"`
	// +optional
	Status EdgeSiteStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// EdgeSiteList contains a list of EdgeSite.
type EdgeSiteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []EdgeSite `json:"items"`
}

// MeshLinkSpec declares that an EDGE cluster's UPF must reach a CONTROL-PLANE cluster's AMF/SMF over
// Cilium ClusterMesh (N2 + N4). The mesh specializer reconciles this into: cluster identity + SCTP
// (helm), a shared CA, the clustermesh connect, and the amf-cp / upf-<edge> global services. This is
// the KRM face of what used to be mesh-connect.sh + the fabric controller's imperative steps.
type MeshLinkSpec struct {
	// Edge is the edge cluster name (cluster-<edge>).
	// +required
	Edge string `json:"edge"`
	// ControlPlane is the CP cluster name whose AMF/SMF the edge attaches to (cluster-<cp>, or "mgmt").
	// +required
	ControlPlane string `json:"controlPlane"`
	// UPFName is the N4 service name the CP's SMF uses to reach this edge's UPF (upf-<edge>).
	// +optional
	UPFName string `json:"upfName,omitempty"`
}

// MeshLinkStatus is the observed state.
type MeshLinkStatus struct {
	// Phase: Pending, Connected, N4Wired, Ready, Failed.
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
// +kubebuilder:printcolumn:name="Edge",type=string,JSONPath=`.spec.edge`
// +kubebuilder:printcolumn:name="ControlPlane",type=string,JSONPath=`.spec.controlPlane`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// MeshLink is the Schema for the meshlinks API.
type MeshLink struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`
	// +required
	Spec MeshLinkSpec `json:"spec"`
	// +optional
	Status MeshLinkStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// MeshLinkList contains a list of MeshLink.
type MeshLinkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []MeshLink `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion,
			&EdgeSite{}, &EdgeSiteList{},
			&MeshLink{}, &MeshLinkList{})
		return nil
	})
}
