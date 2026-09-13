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
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sdcorev1alpha1 "github.com/your-org/sdcore-operator/api/v1alpha1"
)

// EdgeSiteReconciler expands ONE EdgeSite intent into Nephio KRM — the pure-CaD replacement for the
// old deploy-server.sh / deploy-edge.sh / mesh-connect.sh scripts. It emits (all owned by the
// EdgeSite, so they GC with it):
//   - a cluster PackageVariant  (clone cluster-ck8s-byoh -> mgmt repo) -> CAPI+BYOH provision the cluster
//   - IPClaims                  (per-site pod/access/core/UE prefixes) -> the IPAM specializer allocates
//   - a control-plane PackageVariant (role cp+upf)  -> SD-Core CP onto the site's repo
//   - a UPF PackageVariant                          -> BESS-UPF onto the site's repo
//   - a MeshLink (role upf-only) -> the mesh controller wires N2/N4 ClusterMesh to the CP
// Porch renders the variants, specializers fill IPAM/interfaces, Config Sync delivers to the cluster.
type EdgeSiteReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	BlueprintRepo string
	MgmtRepo      string
}

// subscriberCount = how many starter SIMs each self-contained core provisions (consecutive IMSIs).
const subscriberCount = 5

// imsiBase = the 11-digit prefix for the starter subscriber block; the last 4 digits are 7488+i.
// This MUST match the real test SIMs' IMSIs (00101 PLMN + MSIN 010000XXXX), else the UDR has no
// subscriber for the IMSI the UE actually presents and every auth 5G-AKA lookup returns 404. A UE
// with IMSI 001010100007488 fails against a provisioned 001010000007488 — one MSIN digit apart.
const imsiBase = "00101010000" // -> 001010100007488 .. 001010100007492

var (
	pvGVK       = schema.GroupVersionKind{Group: "config.porch.kpt.dev", Version: "v1alpha1", Kind: "PackageVariant"}
	ipClaimGVK  = schema.GroupVersionKind{Group: "ipam.resource.nephio.org", Version: "v1alpha1", Kind: "IPClaim"}
	meshLinkGVK = schema.GroupVersionKind{Group: "sdcore.nephio.io", Version: "v1alpha1", Kind: "MeshLink"}
	netSliceGVK = schema.GroupVersionKind{Group: "sdcore.nephio.io", Version: "v1alpha1", Kind: "NetworkSlice"}
)

// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=edgesites,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=edgesites/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=meshlinks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=config.porch.kpt.dev,resources=packagevariants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ipam.resource.nephio.org,resources=ipclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

func (r *EdgeSiteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var es sdcorev1alpha1.EdgeSite
	if err := r.Get(ctx, req.NamespacedName, &es); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	cluster := "cluster-" + es.Name

	// Auto-derive the per-site addressing so the user only writes server + role (+ controlPlane) in the
	// EdgeSite — no hand-picked CIDRs. Non-overlapping by a stable index from the name. If the user
	// DID set a field we keep it. Persisted back to the spec so the injected copy (apply-replacements)
	// and everyone else see the same values. (When the IPAM specializer's allocation path works, the
	// IPClaims below replace this; until then this keeps the render valid.)
	if err := r.step(ctx, &es, "DeriveAddressing", func() error { return r.deriveAddressing(ctx, &es) }); err != nil {
		return ctrl.Result{}, err
	}

	// Where CP/UPF land: a colocated site deploys onto the mgmt cluster's repo (no new cluster);
	// otherwise onto the new cluster's own repo (cluster-<name>).
	deployRepo := cluster
	if es.Spec.Colocated {
		deployRepo = r.MgmtRepo
	}

	// Every step is run through step(): logs what it's doing, and on failure sets a CLEAR status
	// condition (Ready=False, reason=<Step>Failed, message="<step>: <err>") + wraps the error naming
	// site + step, so both `kubectl get edgesite` and the logs pinpoint exactly what broke.
	// Colocated cp+upf runs on the EXISTING mgmt cluster -> skip provisioning a new one.
	if !es.Spec.Colocated {
		if err := r.step(ctx, &es, "ClusterPackageVariant",
			func() error { return r.pkgVariant(ctx, &es, cluster, "cluster-ck8s-byoh", r.MgmtRepo, cluster, true) }); err != nil {
			return ctrl.Result{}, err
		}
	}
	for _, n := range []struct{ name, kind string }{{"pod", "network"}, {"access", "network"}, {"core", "network"}, {"uepool", "pool"}} {
		claim := n // capture
		if err := r.step(ctx, &es, "IPClaim/"+claim.name,
			func() error { return r.ipClaim(ctx, &es, es.Name+"-"+claim.name, claim.kind) }); err != nil {
			return ctrl.Result{}, err
		}
	}
	if es.Spec.Role == "cp+upf" {
		if err := r.step(ctx, &es, "ControlPlanePackageVariant",
			func() error { return r.pkgVariant(ctx, &es, "cp-"+es.Name, "sdcore-cp", deployRepo, "cp-"+es.Name, true) }); err != nil {
			return ctrl.Result{}, err
		}
		// N2 exposure — where the CP (AMF) runs, expose NGAP on a RAN-reachable LB VIP so a real gNB can
		// attach. Delivered as FULLY-RENDERED KRM straight to the deploy repo (GitOps), with this site's
		// distinct VIP substituted in Go — NOT via a PackageVariant + apply-replacements, because Nephio's
		// config-injection does not propagate newly-added EdgeSite fields (n2VIP) into the injected copy, so
		// the kpt render fails "fieldPath spec.n2VIP is missing". Controller-produces-KRM + Git-delivers is
		// the same robust path deliverCoreSlice uses.
		if err := r.step(ctx, &es, "N2Expose",
			func() error { return r.deliverN2Expose(ctx, &es, deployRepo) }); err != nil {
			return ctrl.Result{}, err
		}
		// A self-contained core provisions its OWN webui. Deliver a NetworkSlice + a starter SIM to the
		// SITE's deploy repo (GitOps) so the site's operator (cluster-relative webui URL) provisions them
		// into THIS core's webconsole. Runs for colocated too (delivered to the mgmt repo) so the mgmt
		// core's AMF serves its gNB's TAI (the gNB TAC and slice TAC both derive from siteIndex -> match).
		if err := r.step(ctx, &es, "CoreSliceAndSubscriber",
			func() error { return r.deliverCoreSlice(ctx, &es, deployRepo) }); err != nil {
			return ctrl.Result{}, err
		}
		// RAN (optional): if the site declares a `ran` block, deploy an OCUDU gNB auto-connected to this
		// core (edge or colocated mgmt core). Same GitOps delivery; skipped only when `ran` is unset.
		if es.Spec.RAN != nil {
			if err := r.step(ctx, &es, "RANgNB", func() error { return r.deliverRAN(ctx, &es, deployRepo) }); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	// Multus CNI — cluster infra the UPF's N3/N6 NADs need. Delivered per cluster the pure-Nephio way
	// (Config Sync), NOT the Makefile — the same way real Nephio ships pv-multus in its cluster bundle.
	// Named by the deploy repo (one per cluster; colocated sites on mgmt share multus-mgmt). Emitted
	// BEFORE the UPF so the CNI is present first. inject=false: cluster-scoped infra, no per-site params.
	if err := r.step(ctx, &es, "MultusPackageVariant",
		func() error {
			return r.pkgVariant(ctx, &es, "multus-"+deployRepo, "multus", deployRepo, "multus-"+deployRepo, false)
		}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.step(ctx, &es, "UPFPackageVariant",
		func() error { return r.pkgVariant(ctx, &es, "upf-"+es.Name, "edge-site", deployRepo, "upf-"+es.Name, true) }); err != nil {
		return ctrl.Result{}, err
	}
	// Datapath actuator (UPFDataPath CRD + per-node sdcore-datapath DaemonSet) — delivered per cluster
	// the pure-Nephio way (Config Sync), NOT by the Makefile. Named by the deploy repo so it's exactly
	// one per cluster (colocated sites on mgmt share datapath-mgmt). inject=false: cluster-scoped infra.
	if err := r.step(ctx, &es, "DatapathActuatorPackageVariant",
		func() error {
			return r.pkgVariant(ctx, &es, "datapath-"+deployRepo, "datapath-actuator", deployRepo, "datapath-"+deployRepo, false)
		}); err != nil {
		return ctrl.Result{}, err
	}
	if es.Spec.Role == "upf-only" {
		cp := es.Spec.ControlPlane
		if cp == "" {
			cp = "mgmt"
		}
		if err := r.step(ctx, &es, "MeshLink", func() error { return r.meshLink(ctx, &es, cp) }); err != nil {
			return ctrl.Result{}, err
		}
		// Edge NetworkSlice: so the central CP actually routes UE traffic to THIS edge UPF. The
		// NetworkSlice controller provisions it into the mgmt webconsole (slice site-info: edge UPF + edge
		// TAC + UE pool), so the AMF serves the edge TAI and the SMF maps the DNN to the edge UPF over N4.
		// The per-SIM Subscribers (sliceRef=<edge>) are the one bit of user data still applied separately.
		if err := r.step(ctx, &es, "EdgeNetworkSlice", func() error { return r.edgeSlice(ctx, &es) }); err != nil {
			return ctrl.Result{}, err
		}
	}

	// all steps succeeded — clear, positive status.
	es.Status.Phase = "Deploying"
	es.Status.Cluster = cluster
	es.Status.Message = "cluster + NF PackageVariants + IPClaims emitted; Porch/Config-Sync actuating"
	es.Status.ObservedGeneration = es.Generation
	setCondition(&es.Status.Conditions, "Ready", metav1.ConditionTrue, "Emitted",
		"all KRM emitted (cluster/CP/UPF PackageVariants + IPClaims"+meshNote(es.Spec.Role)+")", es.Generation)
	if err := r.Status().Update(ctx, &es); err != nil {
		log.Error(err, "EdgeSite: failed to write success status", "site", es.Name)
		return ctrl.Result{}, fmt.Errorf("EdgeSite %s: status update: %w", es.Name, err)
	}
	log.Info("EdgeSite reconciled OK", "site", es.Name, "role", es.Spec.Role, "cluster", cluster)
	return ctrl.Result{}, nil
}

// step runs one reconcile step with uniform, explicit error handling: it logs the step, and on error
// records a precise status Condition + Phase + Message and returns a wrapped error naming site+step.
func (r *EdgeSiteReconciler) step(ctx context.Context, es *sdcorev1alpha1.EdgeSite, name string, fn func() error) error {
	log := logf.FromContext(ctx)
	log.Info("EdgeSite step", "site", es.Name, "step", name)
	if err := fn(); err != nil {
		log.Error(err, "EdgeSite step FAILED", "site", es.Name, "step", name)
		es.Status.Phase = "Failed"
		es.Status.Message = fmt.Sprintf("step %q: %v", name, err)
		setCondition(&es.Status.Conditions, "Ready", metav1.ConditionFalse, name+"Failed",
			fmt.Sprintf("%s: %v", name, err), es.Generation)
		if uerr := r.Status().Update(ctx, es); uerr != nil {
			log.Error(uerr, "EdgeSite: also failed to write failure status", "site", es.Name, "step", name)
		}
		return fmt.Errorf("EdgeSite %q step %q: %w", es.Name, name, err)
	}
	return nil
}

func meshNote(role string) string {
	if role == "upf-only" {
		return " + MeshLink"
	}
	return ""
}

// pkgVariant emits a Porch PackageVariant cloning blueprint upstreamPkg into downRepo/downPkg. When
// inject is true the EdgeSite is config-injected so the blueprint's apply-replacements can source
// spec.* (cluster/CP/UPF need per-site addressing); cluster-scoped infra (datapath-actuator) sets
// inject=false — it has no injection point and needs no per-site params.
func (r *EdgeSiteReconciler) pkgVariant(ctx context.Context, es *sdcorev1alpha1.EdgeSite, name, upstreamPkg, downRepo, downPkg string, inject bool) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(pvGVK)
	u.SetNamespace(es.Namespace)
	u.SetName(name)
	spec := map[string]interface{}{
		"upstream":   map[string]interface{}{"repo": r.BlueprintRepo, "package": upstreamPkg, "workspaceName": "main"},
		"downstream": map[string]interface{}{"repo": downRepo, "package": downPkg},
	}
	if inject {
		spec["injectors"] = []interface{}{map[string]interface{}{"group": "sdcore.nephio.io", "version": "v1alpha1", "kind": "EdgeSite", "name": es.Name}}
	}
	_ = unstructured.SetNestedMap(u.Object, spec, "spec")
	return r.applyOwned(ctx, es, u)
}

func (r *EdgeSiteReconciler) ipClaim(ctx context.Context, es *sdcorev1alpha1.EdgeSite, name, kind string) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ipClaimGVK)
	u.SetNamespace(es.Namespace)
	u.SetName(name)
	prefixLen := int64(24)
	if kind == "pool" || name == es.Name+"-pod" {
		prefixLen = 16
	}
	_ = unstructured.SetNestedMap(u.Object, map[string]interface{}{
		"kind":            kind,
		"networkInstance": map[string]interface{}{"name": "sdcore-vpc"},
		"prefixLength":    prefixLen,
		// createPrefix carves a NEW dynamic prefix of prefixLength out of the VPC pool. Newer
		// nephio resource-backend REQUIRES this for any claim with prefixLength != 0 ("dynamic prefix
		// claim with prefixlength <> 0 need to set create prefix"); older backends accept it too.
		"createPrefix": true,
	}, "spec")
	return r.applyOwned(ctx, es, u)
}

func (r *EdgeSiteReconciler) meshLink(ctx context.Context, es *sdcorev1alpha1.EdgeSite, cp string) error {
	ml := &sdcorev1alpha1.MeshLink{}
	ml.SetGroupVersionKind(meshLinkGVK)
	ml.Namespace = es.Namespace
	ml.Name = es.Name
	ml.Spec = sdcorev1alpha1.MeshLinkSpec{Edge: es.Name, ControlPlane: cp, UPFName: "upf-" + es.Name}
	return r.applyOwned(ctx, es, ml)
}

// edgeSlice emits a NetworkSlice so the central CP steers this edge's UE traffic to the edge UPF. sd/uePool
// come from the same derived addressing the UPF/datapath use; upfName is the cross-cluster N4 service the
// MeshLink creates (upf-<edge>); the gNB/TAC default per-site (a real radio overrides via its own config).
func (r *EdgeSiteReconciler) edgeSlice(ctx context.Context, es *sdcorev1alpha1.EdgeSite) error {
	ns := &sdcorev1alpha1.NetworkSlice{}
	ns.SetGroupVersionKind(netSliceGVK)
	ns.Namespace = es.Namespace
	ns.Name = es.Name
	ns.Spec = sdcorev1alpha1.NetworkSliceSpec{
		Sst:     "1",
		Sd:      es.Spec.SliceSD,
		Dnn:     "internet",
		UePool:  es.Spec.UEPool,
		UpfName: "upf-" + es.Name,
		Plmn:    sdcorev1alpha1.PlmnId{Mcc: "001", Mnc: "01"},
		Gnbs:    []sdcorev1alpha1.GnbRef{{Name: "gnb-" + es.Name, Tac: 1}},
	}
	return r.applyOwned(ctx, es, ns)
}

// deliverN2Expose delivers the AMF NGAP exposure (a serviceSelector-scoped single-IP Cilium LB pool +
// the amf-ngap LoadBalancer Service) as fully-rendered KRM to the CP's deploy repo, with this site's
// DISTINCT VIP baked in. Scoped pool so cilium-ingress can't steal the VIP; single-IP start==stop range
// (not a /32 CIDR) so it always allocates; the node-agent announces it on the RAN NIC.
func (r *EdgeSiteReconciler) deliverN2Expose(ctx context.Context, es *sdcorev1alpha1.EdgeSite, deployRepo string) error {
	vip := es.Spec.N2VIP
	if vip == "" {
		vip = r.ranBase(ctx, es) + ".240"
	}
	y := fmt.Sprintf(`apiVersion: cilium.io/v2alpha1
kind: CiliumLoadBalancerIPPool
metadata:
  name: ran-ngap-pool
spec:
  serviceSelector:
    matchLabels:
      sdcore.nephio.io/expose: n2
  blocks:
    - start: "%s"
      stop: "%s"
---
apiVersion: v1
kind: Service
metadata:
  name: amf-ngap
  namespace: default
  labels:
    app: amf-ngap
    sdcore.nephio.io/expose: n2
spec:
  type: LoadBalancer
  selector:
    app: amf
  ports:
    - name: ngap
      protocol: SCTP
      port: 38412
      targetPort: 38412
`, vip, vip)
	return gitPut(ctx, r.Client, deployRepo, "n2-expose/amf-ngap.yaml", y)
}

// deliverCoreSlice delivers a self-contained core's NetworkSlice + a starter Subscriber to the SITE's
// deploy repo (GitOps). The site's Config Sync applies them, and the site's operator — whose webui URL is
// cluster-relative — provisions them into THIS core's own webconsole, so a UE can attach locally with no
// dependency on the mgmt cluster. upfName is the site's local UPF Service ("upf", from the edge-site
// blueprint); sd/uePool are the site's derived addressing; the gNB/TAC default per-site (a real radio
// overrides via its own config). One starter SIM (the proven test key) so the core is UE-ready out of the
// box; the user adds real SIMs as more Subscriber CRs on the site cluster.
func (r *EdgeSiteReconciler) deliverCoreSlice(ctx context.Context, es *sdcorev1alpha1.EdgeSite, deployRepo string) error {
	tac := 1 // uniform TAC across the whole fleet (one tracking area)
	slice := fmt.Sprintf(`apiVersion: sdcore.nephio.io/v1alpha1
kind: NetworkSlice
metadata:
  name: %s
  namespace: default
spec:
  sst: "1"
  sd: "%s"
  dnn: internet
  uePool: "%s"
  upfName: upf
  plmn: {mcc: "001", mnc: "01"}
  gnbs:
  - {name: gnb-%s, tac: %d}
`, es.Name, es.Spec.SliceSD, es.Spec.UEPool, es.Name, tac)
	if err := gitPut(ctx, r.Client, deployRepo, "core-slice/networkslice.yaml", slice); err != nil {
		return err
	}
	// Provision a starter block of subscribers (5 consecutive IMSIs from imsiBase+7488) with the proven
	// test key, so the core is UE-ready out of the box for multiple UEs. The subscriber_controller gathers
	// every Subscriber CR referencing this slice into the slice's imsi list. Add/replace with real SIMs by
	// editing these Subscriber CRs on the site cluster.
	var sub strings.Builder
	for i := 0; i < subscriberCount; i++ {
		imsi := fmt.Sprintf("%s%04d", imsiBase, 7488+i) // 001010100007488 .. 7492
		sub.WriteString(fmt.Sprintf(`apiVersion: sdcore.nephio.io/v1alpha1
kind: Subscriber
metadata:
  name: sub-%s
  namespace: default
spec:
  imsi: "%s"
  key: "5122250214c33e723a5dd523fc145fc0"
  opc: "981d464c7c52eb6e5036234984ad0bcf"
  sliceRef: %s
---
`, imsi, imsi, es.Name))
	}
	return gitPut(ctx, r.Client, deployRepo, "core-slice/subscriber.yaml", sub.String())
}

// allocateRanVIP picks a COLLISION-FREE N2 VIP for a standalone core: the lowest free /32 in the RAN pool
// (.241-.254), scanning the VIPs every OTHER EdgeSite already holds. The result is persisted by the caller
// (set-if-empty), so it is STABLE — it never shifts when another site is added or deleted. Collision-free
// for any number of cp-bearing sites up to the pool size; .240 stays the mgmt CP. The 192.168.4 base is
// this lab's RAN L2 — override spec.ranSubnet per site for a different RAN. (A pool bigger than a /28, or
// resource-backend IPAM allocation, is the path when a single L2 must hold more than ~14 cores.)
func (r *EdgeSiteReconciler) allocateRanVIP(ctx context.Context, es *sdcorev1alpha1.EdgeSite, base string) string {
	// Once allocated the VIP is STABLE. If this site already carries a valid pool VIP, return it verbatim so a
	// re-derive never reshuffles a running site's NGAP LoadBalancer IP.
	if es.Spec.N2VIP != "" {
		var a, b, c, od int
		if n, _ := fmt.Sscanf(es.Spec.N2VIP, "%d.%d.%d.%d", &a, &b, &c, &od); n == 4 && od >= 241 && od <= 254 {
			return es.Spec.N2VIP
		}
	}
	taken := map[int]bool{240: true} // .240 reserved for the mgmt CP
	var list sdcorev1alpha1.EdgeSiteList
	if err := r.List(ctx, &list); err == nil {
		for i := range list.Items {
			o := &list.Items[i]
			if o.Name == es.Name || o.Spec.N2VIP == "" {
				continue
			}
			var a, b, c, d int
			if n, _ := fmt.Sscanf(o.Spec.N2VIP, "%d.%d.%d.%d", &a, &b, &c, &d); n == 4 {
				taken[d] = true
			}
		}
	}
	// Start the search at a DETERMINISTIC per-identity offset (stable hash of the site name) instead of always
	// at .241, then linear-probe forward past any taken slot. Two sites allocating concurrently (or under a
	// multi-replica operator) then gravitate to DIFFERENT VIPs rather than racing on the same "lowest free".
	start := siteIndex(es.Name) % 14
	for k := 0; k < 14; k++ {
		i := 241 + (start+k)%14
		if !taken[i] {
			return fmt.Sprintf("%s.%d", base, i)
		}
	}
	return base + ".254" // pool exhausted — enlarge the RAN subnet for more cores on one L2
}

// ranBase returns the first three octets (e.g. "192.168.1") of the L2 the RAN/N2 VIP lives on, DERIVED
// from the site's real network so the product works on ANY environment — no hardcoded lab subnet.
// Precedence: an explicit spec.ranSubnet, else the site IP (the mgmt node's InternalIP for a colocated
// core, or spec.server for an edge). Falls back to 192.168.4 only if nothing is parseable.
func (r *EdgeSiteReconciler) ranBase(ctx context.Context, es *sdcorev1alpha1.EdgeSite) string {
	if b := netBase(es.Spec.RanSubnet); b != "" {
		return b
	}
	ip := es.Spec.Server
	if es.Spec.Colocated {
		if n := r.mgmtNodeIP(ctx); n != "" {
			ip = n
		}
	}
	if b := netBase(ip); b != "" {
		return b
	}
	return "192.168.4"
}

// netBase parses "A.B.C" from "A.B.C.D" or "A.B.C.D/xx"; "" if not parseable.
func netBase(s string) string {
	var a, b, c, d int
	if n, _ := fmt.Sscanf(s, "%d.%d.%d.%d", &a, &b, &c, &d); n >= 3 {
		return fmt.Sprintf("%d.%d.%d", a, b, c)
	}
	return ""
}

// mgmtNodeIP returns a cluster node's InternalIP (control-plane preferred) — the mgmt cluster's own
// address, used to derive the RAN L2 for a colocated core without any hardcoded/lab-specific value.
func (r *EdgeSiteReconciler) mgmtNodeIP(ctx context.Context) string {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return ""
	}
	pick := ""
	for i := range nodes.Items {
		n := &nodes.Items[i]
		ip := ""
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				ip = a.Address
				break
			}
		}
		if ip == "" {
			continue
		}
		if _, ok := n.Labels["node-role.kubernetes.io/control-plane"]; ok {
			return ip
		}
		pick = ip
	}
	return pick
}

// deliverRAN deploys an OCUDU gNB on a cp+upf site, AUTO-CONNECTED to that site's SD-Core, delivered as
// KRM to the site's deploy repo (GitOps) — the same controller-produces-KRM path as deliverCoreSlice. The
// user declared only the radio bits (RANSpec); everything core-facing is derived here:
//   - AMF: an initContainer resolves the local `amf` Service ClusterIP and stamps it into gnb.yaml (the CP
//     is on this same cluster; no cross-cluster wiring for a self-contained core).
//   - PLMN 001/01 + TAC (siteIndex) + SD (from the site's slice) = the exact TAI the AMF serves.
//   - N3: a multus interface on the site's access L2 at the derived gnbIP (which the datapath actuator
//     already ARP-programs + accessRoutes), so N3 GTP-U to the local UPF works.
// Phase 1 = ru_sdr device_driver: zmq (RF simulator, no radio) — provable on a VM. b210/x310 (real SDR) and
// 7.2 ru_ofh are the same delivery with the ru block + host tuning swapped.
func (r *EdgeSiteReconciler) deliverRAN(ctx context.Context, es *sdcorev1alpha1.EdgeSite, deployRepo string) error {
	ran := es.Spec.RAN
	if ran == nil {
		return nil
	}
	ranReplicas := 1
	if ran.Enabled != nil && !*ran.Enabled {
		ranReplicas = 0 // ran.enabled:false -> stop the gNB (spare the O-RU) without deleting intent
	}
	tac := 1 // uniform TAC across the whole fleet (one tracking area)
	sd, _ := strconv.ParseInt(strings.TrimPrefix(orDefault(es.Spec.SliceSD, "102030"), "0x"), 16, 64)
	split := orDefault(ran.Split, "8")
	device := orDefault(ran.Device, "zmq")

	// A real SDR (UHD) or O-RU drives actual RF and is bound to the node the radio is attached to — the gNB
	// runs hostNetwork + privileged and mounts the host's UHD/DPDK/MKL libs. ZMQ is an in-cluster simulator.
	isUHD := split == "8" && device != "zmq"
	isOFH := split == "7.2"

	// Config precedence, so a client only writes `device:` for the common case:
	//   1. ran.config      — full user override (advanced).
	//   2. a real radio     — a proven built-in PROFILE per device (n310/b210/x310); the detect initContainer
	//                         fills RADIO_ARGS (auto-detected radio) + AMF_ADDR + the taskset cpuset (isolcpus).
	//   3. zmq/simulator    — a simple self-consistent config from ran.cell.
	var gnb string
	if s := strings.TrimSpace(ran.Config); s != "" {
		gnb = s
	} else if isUHD {
		gnb = deviceProfile(device, tac, sd)
	} else if isOFH {
		gnb = ofhProfile(ran.Ofh, ran.Cell, tac, sd)
	} else {
		pci := orInt(ran.Cell.Pci, 1)
		band := strings.TrimPrefix(orDefault(ran.Cell.Band, "n78"), "n")
		arfcn := orInt(ran.Cell.DlArfcn, 632628)
		bw := orInt(ran.Cell.BandwidthMHz, 100)
		scs := orInt(ran.Cell.CommonScs, 30)
		prachIdx := 0
		if device == "zmq" && band == "78" && arfcn == 632628 && bw == 100 && scs == 30 {
			band, arfcn, bw, scs = "3", 368500, 10, 15
			prachIdx = 1
		}
		radio := ranRadioBlock(split, device, srateForBw(bw), ran.Ofh)
		prach := ""
		if prachIdx > 0 {
			prach = fmt.Sprintf("\n  prach:\n    prach_config_index: %d\n    zero_correlation_zone: 0", prachIdx)
		}
		gnb = fmt.Sprintf(`cu_cp:
  amf:
    addr: AMF_ADDR
    port: 38412
    bind_addr: 0.0.0.0
    supported_tracking_areas:
      - tac: %d
        plmn_list:
          - plmn: "00101"
            tai_slice_support_list:
              - sst: 1
                sd: %d
%s
cell_cfg:
  dl_arfcn: %d
  band: %s
  channel_bandwidth_MHz: %d
  common_scs: %d
  plmn: "00101"
  tac: %d
  pci: %d%s
log:
  filename: stdout
  all_level: info
`, tac, sd, radio, arfcn, band, bw, scs, tac, pci, prach)
	}

	// N3/NG-U downlink bind - universal across ALL gNB modes (OFH, UHD/SDR, ZMQ): pin the NG-U (N3
	// GTP-U) socket to the gNB real n3 IP (GnbIP). Without it OCUDU binds/advertises 0.0.0.0, the UPF
	// encaps downlink to 0.0.0.0, and the UE gets 5G + IP but NO internet (downlink black-holed at
	// accessbad_route). Skip if the config already declares cu_up.
	if es.Spec.GnbIP != "" && !strings.Contains(gnb, "cu_up:") {
		gnb += fmt.Sprintf("\ncu_up:\n  ngu:\n    socket:\n      - bind_addr: %s\n", es.Spec.GnbIP)
	}
	cm := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: ocudu-gnb-config
  namespace: default
data:
  gnb.yaml: |
%s`, indent(gnb, "    "))
	if err := gitPut(ctx, r.Client, deployRepo, "ran/configmap.yaml", cm); err != nil {
		return err
	}

	// The gNB Deployment. An initContainer resolves `amf` -> ClusterIP into the config either way.
	//  - UHD real radio: hostNetwork + privileged, image ocudu/gnb:uhd, mounts the host's UHD/DPDK/MKL libs
	//    (the binary links them and the radio is physically on the node) + UHD images. No multus (hostNet).
	//  - ZMQ simulator: pod-network N3 via the access-net multus NAD, image ocudu/gnb:zmq.
	initC := `      initContainers:
        - name: resolve-amf
          image: busybox:1.36
          command: ["sh","-c","until AMF=$(nslookup amf-headless.default.svc.cluster.local 2>/dev/null | awk '/^Address: /{print $2; exit}'); [ -n \"$AMF\" ]; do echo waiting for amf DNS; sleep 2; done; sed \"s/AMF_ADDR/$AMF/\" /in/gnb.yaml > /out/gnb.yaml; echo resolved AMF=$AMF"]
          volumeMounts:
            - { name: cfg-in, mountPath: /in }
            - { name: cfg-out, mountPath: /out }`
	var podSpec string
	if isOFH {
		// Split 7.2 Open Fronthaul: a DPDK O-RU gNB (integrated CU+DU). Non-hostNetwork (N2 via the pod's
		// primary CNI IP; N3 via the access-net multus iface). The fronthaul VF is driven by DPDK over vfio
		// (hostPath /dev/vfio + 1G hugepages). HOST PREREQS the operator does NOT manage: the fronthaul VF
		// bound to vfio-pci, hugepages reserved, and PTP (phc2sys) running — hardware/host concerns.
		ofhInit := `set -e
until AMF=$(getent hosts amf-headless.default.svc.cluster.local 2>/dev/null | awk '{print $1; exit}'); [ -n "$AMF" ]; do echo waiting for amf DNS; sleep 2; done
CPUS=$(tr ' ' '\n' < /proc/cmdline | grep '^isolcpus=' | sed 's/.*isolcpus=//' | tr ',' '\n' | grep -E '^[0-9]+(-[0-9]+)?$' | head -1)
[ -n "$CPUS" ] || CPUS="0-$(($(nproc)-1))"
A=$(ip -o -4 addr show n3 2>/dev/null | awk '{print $4}' | head -1); [ -n "$A" ] && ip addr change "$A" dev n3 scope link 2>/dev/null || true
sed "s/AMF_ADDR/$AMF/" /in/gnb.yaml > /out/gnb.yaml
# N3/NG-U: pin the gNB downlink GTP-U bind to its real n3 IP, else it advertises 0.0.0.0 and the UPF
# encaps downlink to 0.0.0.0 (uplink works, downlink dropped). $A is the n3 addr (with /prefix).
N3=$(echo "$A" | cut -d/ -f1)
if [ -n "$N3" ] && ! grep -q '^cu_up:' /out/gnb.yaml; then printf '\ncu_up:\n  ngu:\n    socket:\n      - bind_addr: %s\n' "$N3" >> /out/gnb.yaml; fi
printf '%s' "$CPUS" > /out/cpuset
echo "ofh-init: cpuset=[$CPUS] amf=$AMF"
`
		detectCM := fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: ocudu-gnb-detect\n  namespace: default\ndata:\n  detect.sh: |\n%s", indent(ofhInit, "    "))
		if err := gitPut(ctx, r.Client, deployRepo, "ran/detect.yaml", detectCM); err != nil {
			return err
		}
		podSpec = `      dnsPolicy: ClusterFirst
      initContainers:
        - name: ofh-init
          image: ocudu/gnb:split72
          imagePullPolicy: IfNotPresent
          command: ["sh","/scripts/detect.sh"]
          securityContext: { privileged: true }
          volumeMounts:
            - { name: cfg-in, mountPath: /in }
            - { name: scripts, mountPath: /scripts }
            - { name: cfg-out, mountPath: /out }
      containers:
        - name: gnb
          image: ocudu/gnb:split72
          imagePullPolicy: IfNotPresent
          command: ["sh","-c","exec taskset -c \"$(cat /out/cpuset)\" /usr/local/bin/gnb_split_7_2 -c /out/gnb.yaml"]
          securityContext: { privileged: true }
          resources:
            limits: { hugepages-1Gi: 2Gi, memory: 6Gi }
            requests: { hugepages-1Gi: 2Gi, memory: 2Gi, cpu: "4" }
          volumeMounts:
            - { name: cfg-out, mountPath: /out }
            - { name: vfio, mountPath: /dev/vfio }
            - { name: hugepages, mountPath: /dev/hugepages }
            - { name: sysbus, mountPath: /sys/bus/pci }
            - { name: sysdev, mountPath: /sys/devices }
      volumes:
        - { name: cfg-in, configMap: { name: ocudu-gnb-config } }
        - { name: scripts, configMap: { name: ocudu-gnb-detect } }
        - { name: cfg-out, emptyDir: {} }
        - { name: vfio, hostPath: { path: /dev/vfio } }
        - { name: hugepages, emptyDir: { medium: HugePages } }
        - { name: sysbus, hostPath: { path: /sys/bus/pci } }
        - { name: sysdev, hostPath: { path: /sys/devices } }`
	} else if isUHD {
		// SELF-CONTAINED image (UHD + libs + FPGA images baked, built from source) — NO host mounts, so it
		// runs on ANY host/OS. A `detect` initContainer auto-discovers the radio (uhd_find_devices), the
		// isolated cores (isolcpus), resolves the AMF, and (for a networked N310) sets the data-NIC MTU —
		// stamping RADIO_ARGS/AMF_ADDR + the taskset cpuset. So the client declares only `device:`.
		// The radio type is DECLARED by intent (ran.device), mapped to its UHD type here. The detect script
		// only fills in what it CANNOT know statically: a networked radio's discovered addr, the AMF ClusterIP,
		// and the isolcpus set. It must NOT re-pick the type from uhd_find_devices — on a host that can SEE a
		// shared networked USRP (e.g. a lab N310 on the LAN), blind auto-detect would grab that busy device
		// instead of the intent-declared local one (the "Someone tried to claim this device again" crash).
		wantType := map[string]string{"n310": "n3xx", "b210": "b200", "x310": "x300"}[device]
		if wantType == "" {
			wantType = device
		}
		// AMF N2 endpoint = the AMF Service ClusterIP (resolved in-cluster via DNS). The gNB binds SCTP to
		// 0.0.0.0: OCUDU HANGS at startup if asked to bind a specific host IP (a cilium_host bind spins; a LAN
		// IP won't reach the ClusterIP through cilium socket-LB). 0.0.0.0 is the only address that both starts
		// and connects. (A stale long-lived gNB pod can drift into an N2 re-connect loop — a clean pod restart,
		// which the operator does on config change, clears it; there is no per-host single-homing to do here.)
		detectScript := fmt.Sprintf(`set -e
until AMF=$(getent hosts amf-headless.default.svc.cluster.local 2>/dev/null | awk '{print $1; exit}'); [ -n "$AMF" ]; do echo "waiting for amf DNS"; sleep 2; done
WANT=%q
# Honor the intent-declared radio: bind ONLY the requested type, never auto-grab a different/shared one.
# USB b200 needs no addr; networked n3xx/x300 discover THEIR OWN addr (filtered by type so a co-visible
# radio of another type can't be selected). RADIO_ADDR (intent override) always wins for the data plane.
case "$WANT" in
  b200) TYPE=b200; ARGS="type=b200" ;;
  n3xx) TYPE=n3xx; MGMT=$(timeout 20 uhd_find_devices --args "type=n3xx" 2>/dev/null | sed -n 's/^[[:space:]]*addr:[[:space:]]*//p' | head -1)
        DATA=${RADIO_ADDR:-$MGMT}; MGMT=${MGMT:-$RADIO_ADDR}; ARGS="type=n3xx${DATA:+,addr=$DATA}${MGMT:+,mgmt_addr=$MGMT}" ;;
  x300) TYPE=x300; MGMT=$(timeout 20 uhd_find_devices --args "type=x300" 2>/dev/null | sed -n 's/^[[:space:]]*addr:[[:space:]]*//p' | head -1)
        DATA=${RADIO_ADDR:-$MGMT}; ARGS="type=x300${DATA:+,addr=$DATA}" ;;
  *)    TYPE="$WANT"; [ -n "$RADIO_ADDR" ] && ARGS="addr=$RADIO_ADDR" || ARGS="type=$WANT" ;;
esac
CPUS=$(tr ' ' '\n' < /proc/cmdline | grep '^isolcpus=' | sed 's/isolcpus=//' | tr ',' '\n' | grep -E '^[0-9]+(-[0-9]+)?$' | head -1)
[ -n "$CPUS" ] || CPUS="0-$(($(nproc)-1))"
# N3 (access-net multus iface "n3") only needs L2 reachability to the UPF; set it link-scope so OCUDU's
# 0.0.0.0-bound N2 SCTP does NOT advertise it — leaving the pod's primary CNI IP as the ONLY N2 address
# (single-homed => stable N2 on any host). Same for any other non-primary global-scope iface the pod has.
for IFACE in n3 radio0; do
  A=$(ip -o -4 addr show "$IFACE" 2>/dev/null | awk '{print $4}' | head -1)
  [ -n "$A" ] && ip addr change "$A" dev "$IFACE" scope link 2>/dev/null || true
done
sed -e "s/AMF_ADDR/$AMF/" -e "s|RADIO_ARGS|$ARGS|" /in/gnb.yaml > /out/gnb.yaml
printf '%%s' "$CPUS" > /out/cpuset
echo "detect: want=$WANT type=$TYPE data=$DATA args=[$ARGS] cpuset=[$CPUS] amf=$AMF"
`, wantType)
		detectCM := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: ocudu-gnb-detect
  namespace: default
data:
  detect.sh: |
%s`, indent(detectScript, "    "))
		if err := gitPut(ctx, r.Client, deployRepo, "ran/detect.yaml", detectCM); err != nil {
			return err
		}
		// env is block-style: a flow "[ ]" can't hold a block "- {...}" item (that's the KNV2001 parse
		// error). Empty when no radioAddr override.
		envBlock := ""
		if ran.RadioAddr != "" {
			envBlock = fmt.Sprintf("\n          env:\n            - { name: RADIO_ADDR, value: \"%s\" }", ran.RadioAddr)
		}
		// NON-hostNetwork: the gNB runs in its OWN netns, so OCUDU's N2 SCTP (which can ONLY bind 0.0.0.0 —
		// a specific bind_addr hangs it) advertises just the pod's primary-CNI IP instead of every host
		// interface. On a hostNetwork gNB a multi-homed host (cilium + LAN + UPF bridges + radio net + docker
		// + VPN + Tailscale + …) makes the AMF pick an unreachable SCTP path and reset N2 every ~30s; a single
		// pod IP is single-homed and stable on ANY host. N3 rides a dedicated `access-net` multus interface
		// whose IP detect sets link-scope, so SCTP does not advertise it either (see detect.sh).
		podSpec = fmt.Sprintf(`      dnsPolicy: ClusterFirst
      initContainers:
        - name: detect
          image: ocudu/gnb:sdr
          imagePullPolicy: IfNotPresent
          command: ["sh","/scripts/detect.sh"]
          securityContext: { privileged: true }%s
          volumeMounts:
            - { name: cfg-in, mountPath: /in }
            - { name: scripts, mountPath: /scripts }
            - { name: cfg-out, mountPath: /out }
      containers:
        - name: gnb
          image: ocudu/gnb:sdr
          imagePullPolicy: IfNotPresent
          command: ["sh","-c","exec taskset -c \"$(cat /out/cpuset)\" /usr/local/bin/gnb -c /out/gnb.yaml"]
          securityContext: { privileged: true }
          volumeMounts:
            - { name: cfg-out, mountPath: /out }
            - { name: usb, mountPath: /dev/bus/usb }
      volumes:
        - { name: cfg-in, configMap: { name: ocudu-gnb-config } }
        - { name: scripts, configMap: { name: ocudu-gnb-detect } }
        - { name: cfg-out, emptyDir: {} }
        - { name: usb, hostPath: { path: /dev/bus/usb, type: DirectoryOrCreate } }`, envBlock)
	} else {
		podSpec = fmt.Sprintf(`%s
      containers:
        - name: gnb
          image: ocudu/gnb:zmq
          imagePullPolicy: IfNotPresent
          args: ["-c","/out/gnb.yaml"]
          securityContext: { privileged: true }
          volumeMounts:
            - { name: cfg-out, mountPath: /out }
      volumes:
        - { name: cfg-in, configMap: { name: ocudu-gnb-config } }
        - { name: cfg-out, emptyDir: {} }`, initC)
	}
	// Both ZMQ and (now non-hostNetwork) UHD gNBs get a dedicated `access-net` interface `n3` for GTP-U on
	// the UPF's L2 — its IP is set link-scope by detect so SCTP won't advertise it, leaving the pod's primary
	// CNI IP as the ONLY N2 SCTP address (single-homed).
	// PIN a deterministic MAC on `n3`: a pod's macvlan MAC is random per-restart, and the BESS UPF captures
	// the gNB's MAC ONCE into its downlink rewrite — a changed MAC after any gNB restart silently blackholes
	// downlink (UE has 5G + IP but no internet). A stable MAC keeps that binding valid forever. 02: = LAA.
	n3mac := "02:00:00:00:00:01"
	if o := strings.Split(es.Spec.GnbIP, "."); len(o) == 4 {
		n3mac = fmt.Sprintf("02:00:%02x:%02x:%02x:%02x", atoiOr(o[0], 10), atoiOr(o[1], 200), atoiOr(o[2], 193), atoiOr(o[3], 20))
	}
	podAnnot := fmt.Sprintf(`
      annotations:
        k8s.v1.cni.cncf.io/networks: '[{"name":"access-net","interface":"n3","ips":["%s/24"],"mac":"%s"}]'`, es.Spec.GnbIP, n3mac)
	// A NETWORKED SDR (N310/X310) streams at jumbo MTU that a plain cluster-CNI pod can't carry. Give the
	// gNB a `radio0` jumbo macvlan on the host's radio NIC so it reaches the SDR at MTU 9000; detect sets it
	// link-scope so it stays out of N2 SCTP. (USB/O-RU leave radioNic empty and skip this entirely.)
	if isUHD && ran.RadioNic != "" && strings.Contains(ran.RadioAddr, ".") {
		nad := fmt.Sprintf(`apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: radio-net
  namespace: default
spec:
  config: '{"cniVersion":"0.3.1","type":"macvlan","master":"%s","mode":"bridge","mtu":9000,"ipam":{"type":"static"}}'
`, ran.RadioNic)
		if err := gitPut(ctx, r.Client, deployRepo, "ran/radio-net.yaml", nad); err != nil {
			return err
		}
		gnbRadioIP := ran.RadioAddr[:strings.LastIndex(ran.RadioAddr, ".")] + ".10"
		podAnnot = fmt.Sprintf(`
      annotations:
        k8s.v1.cni.cncf.io/networks: '[{"name":"access-net","interface":"n3","ips":["%s/24"]},{"name":"radio-net","interface":"radio0","ips":["%s/24"]}]'`, es.Spec.GnbIP, gnbRadioIP)
	}
	// A hostNetwork (UHD) gNB binds host :2152 (GTP-U); on a RollingUpdate the old+new pods collide on it
	// and crash-loop. Recreate tears the old down first. (ZMQ is pod-network, no host-port clash.)
	strategy := ""
	if isUHD {
		strategy = "\n  strategy: { type: Recreate }"
	}
	dep := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: gnb-%s
  namespace: default
  labels: { app: gnb-%s }
spec:
  replicas: %d
  selector: { matchLabels: { app: gnb-%s } }%s
  template:
    metadata:
      labels: { app: gnb-%s }%s
    spec:
%s
`, es.Name, es.Name, ranReplicas, es.Name, strategy, es.Name, podAnnot, podSpec)
	return gitPut(ctx, r.Client, deployRepo, "ran/deployment.yaml", dep)
}

// deviceProfile returns a proven OCUDU gnb.yaml for a real SDR — sensible DEFAULTS per radio so a client
// writes only `device:`. RADIO_ARGS + AMF_ADDR are placeholders the detect initContainer fills at runtime
// (auto-detected radio address, resolved AMF). TAC/SD/PLMN match the core. Override any of it with ran.config.
func atoiOr(s string, d int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n & 0xff
	}
	return d
}

func deviceProfile(device string, tac int, sd int64) string {
	head := fmt.Sprintf(`cu_cp:
  amf:
    addr: AMF_ADDR
    bind_addr: 0.0.0.0
    supported_tracking_areas:
      - tac: %d
        plmn_list:
          - plmn: "00101"
            tai_slice_support_list:
              - { sst: 1, sd: %d }
`, tac, sd)
	switch device {
	case "b210":
		// USRP B210 over USB — PROVEN working profile: band n78 TDD 20 MHz, 1T1R. Validated end-to-end on a real
		// b210 + COTS phone (registration + PDU session + internet, 22 two-way flows). Each choice was learned the
		// hard way and is load-bearing:
		//   - 1T1R (nof_antennas 1): 2T2R doubles the USB3 + CPU load and the b210 can't keep up -> RF
		//     "underflow/late" that drops the UE mid-attach. One antenna is stable and still attaches fine.
		//   - otw_format sc12 + num_recv/send_frames=64: shrink the USB3 sample bandwidth and add buffering so the
		//     radio doesn't starve on a busy host.
		//   - band 78 @ dl_arfcn 621312 (3319.68 MHz): the frequency the COTS UE actually camps on. The old n3
		//     368500 default never got a single PRACH from the phone.
		//   - max_consecutive_kos huge: don't release the UE over a few transient radio KOs — the robustness that
		//     lets this survive a loaded / less-tuned server (the "any server, any env" requirement).
		return head + fmt.Sprintf(`ru_sdr: { device_driver: uhd, device_args: "RADIO_ARGS,num_recv_frames=64,num_send_frames=64", clock: internal, srate: 23.04, otw_format: sc12, tx_gain: 80, rx_gain: 60 }
cell_cfg:
  dl_arfcn: 621312
  band: 78
  channel_bandwidth_MHz: 20
  common_scs: 30
  nof_antennas_dl: 1
  nof_antennas_ul: 1
  plmn: "00101"
  tac: %d
  pci: 0
  slicing: [ { sst: 1, sd: %d } ]
  pdsch: { max_consecutive_kos: 1000000 }
  pusch: { max_consecutive_kos: 1000000 }
  pucch: { max_consecutive_kos: 1000000 }
log: { filename: stdout, all_level: warning }
`, tac, sd)
	default:
		// USRP N310/X310 — band n78 TDD 40 MHz. Proven aether-ims N310 profile: tx_gain 45 (35 starves DL,
		// 55 over-drives the PA into a TX->RX self-interference PRACH storm), 2 DL antennas (1 halves DL
		// throughput), 2 UL antennas (RX diversity without RLC max-reTx RLF).
		return head + fmt.Sprintf(`ru_sdr: { device_driver: uhd, device_args: "RADIO_ARGS", srate: 61.44, clock: internal, sync: internal, tx_gain: 45, rx_gain: 70 }
cell_cfg:
  dl_arfcn: 621312
  band: 78
  channel_bandwidth_MHz: 40
  common_scs: 30
  nof_antennas_dl: 2
  nof_antennas_ul: 2
  plmn: "00101"
  tac: %d
  pci: 0
  slicing: [ { sst: 1, sd: %d } ]
  tdd_ul_dl_cfg: { dl_ul_tx_period: 10, nof_dl_slots: 7, nof_dl_symbols: 6, nof_ul_slots: 2, nof_ul_symbols: 4 }
  prach: { prach_config_index: 159, prach_root_sequence_index: 1, zero_correlation_zone: 0, prach_frequency_start: 12 }
  pdsch: { max_consecutive_kos: 1000000 }
  pusch: { max_consecutive_kos: 1000000 }
  pucch: { max_consecutive_kos: 1000000, nof_cell_csi_res: 0 }
  csi: { csi_rs_enabled: false }
log: { filename: stdout, all_level: warning }
`, tac, sd)
	}
}

// srateForBw returns the sample rate (MHz) OCUDU should run for a given channel bandwidth: the smallest
// standard NR rate strictly greater than the bandwidth. This keeps ru_sdr.srate (and the ZMQ base_srate)
// self-consistent with the cell so PRACH stays sample-accurate.
func srateForBw(bwMHz int) string {
	switch {
	case bwMHz <= 5:
		return "7.68"
	case bwMHz <= 10:
		return "11.52"
	case bwMHz <= 15:
		return "15.36"
	case bwMHz <= 20:
		return "23.04"
	case bwMHz <= 40:
		return "46.08"
	case bwMHz <= 50:
		return "61.44"
	case bwMHz <= 80:
		return "92.16"
	default:
		return "122.88"
	}
}

// ofhProfile renders a complete, proven OCUDU gnb.yaml for a split-7.2 O-RU (integrated CU+DU) — the DPDK
// Open Fronthaul path (LiteOn-class 4T4R O-RU). Editable knobs come from ran.ofh (VF/PCI, RU/DU MAC, EAL
// args, port maps) + ran.cell (band/arfcn/bw/scs/pci/antennas); the rest are proven O-RAN 7.2 defaults.
// AMF_ADDR is resolved in-cluster. For a radically different O-RU, set ran.config with your own gnb.yaml.
func ofhProfile(ofh *sdcorev1alpha1.RANOfh, cell sdcorev1alpha1.RANCell, tac int, sd int64) string {
	pci, ruMac, duMac := "0000:af:0a.0", "aa:bb:cc:dd:ee:ff", "00:11:22:33:44:55"
	if ofh != nil {
		pci = orDefault(ofh.Interface, pci)
		ruMac = orDefault(ofh.RuMac, ruMac)
		duMac = orDefault(ofh.DuMac, duMac)
	}
	eal := fmt.Sprintf("--lcores (0-1)@(0-3) -a %s --iova-mode=pa", pci)
	if ofh != nil && strings.TrimSpace(ofh.EalArgs) != "" {
		eal = ofh.EalArgs
	}
	prachP, dlP, ulP := ofhPortList(ofh, "prach"), ofhPortList(ofh, "dl"), ofhPortList(ofh, "ul")
	band := strings.TrimPrefix(orDefault(cell.Band, "n78"), "n")
	arfcn := orInt(cell.DlArfcn, 630000)
	bw := orInt(cell.BandwidthMHz, 100)
	scs := orInt(cell.CommonScs, 30)
	pciCell := orInt(cell.Pci, 0)
	ant := orInt(cell.Antennas, 4)
	return fmt.Sprintf(`cu_cp:
  amf:
    addr: AMF_ADDR
    port: 38412
    bind_addr: 0.0.0.0
    supported_tracking_areas:
      - tac: %d
        plmn_list:
          - plmn: "00101"
            tai_slice_support_list:
              - sst: 1
                sd: %d
hal:
  eal_args: "%s"
expert_phy:
  allow_request_on_empty_uplink_slot: true
ru_ofh:
  t1a_max_cp_dl: 350
  t1a_min_cp_dl: 200
  t1a_max_cp_ul: 350
  t1a_min_cp_ul: 200
  t1a_max_up: 300
  t1a_min_up: 0
  ta4_max: 500
  ta4_min: 0
  is_prach_cp_enabled: true
  ignore_ecpri_payload_size: false
  ignore_ecpri_seq_id: true
  compr_method_ul: bfp
  compr_bitwidth_ul: 9
  compr_method_dl: bfp
  compr_bitwidth_dl: 9
  compr_method_prach: bfp
  compr_bitwidth_prach: 9
  enable_ul_static_compr_hdr: true
  enable_dl_static_compr_hdr: true
  iq_scaling: 5.0
  cells:
    - network_interface: %s
      ru_mac_addr: %s
      du_mac_addr: %s
      prach_port_id: %s
      dl_port_id: %s
      ul_port_id: %s
cell_cfg:
  dl_arfcn: %d
  band: %s
  channel_bandwidth_MHz: %d
  common_scs: %d
  plmn: "00101"
  tac: %d
  pci: %d
  nof_antennas_dl: %d
  nof_antennas_ul: %d
  pdcch:
    common:
      coreset0_index: 11
      ss0_index: 0
  pdsch:
    min_ue_mcs: 0
    max_ue_mcs: 28
    nof_harqs: 16
    max_nof_harq_retxs: 4
    mcs_table: qam64
  pusch:
    min_ue_mcs: 0
    max_ue_mcs: 25
    max_nof_harq_retxs: 4
    msg3_delta_preamble: 2
    p0_nominal_with_grant: -96
    mcs_table: qam64
  pucch:
    p0_nominal: -96
    sr_period_ms: 20
  prach:
    prach_config_index: 159
    prach_root_sequence_index: 1
    zero_correlation_zone: 14
    prach_frequency_start: 22
    preamble_rx_target_pw: -80
    preamble_trans_max: 7
    power_ramping_step_db: 4
    nof_cb_preambles_per_ssb: 64
    ra_resp_window: 10
  csi:
    csi_rs_enabled: true
    csi_rs_period: 20
  ssb:
    ssb_period: 20
    ssb_block_power_dbm: 0
  tdd_ul_dl_cfg:
    dl_ul_tx_period: 5
    nof_dl_slots: 3
    nof_dl_symbols: 6
    nof_ul_slots: 1
    nof_ul_symbols: 4
log:
  filename: stdout
  all_level: info
  ofh_level: info
`, tac, sd, eal, pci, ruMac, duMac, prachP, dlP, ulP, arfcn, band, bw, scs, tac, pciCell, ant, ant)
}

// ofhPortList returns the eAxC port map for "prach"/"dl"/"ul" as a YAML flow list, honouring ran.ofh
// overrides and falling back to a 4T4R default.
func ofhPortList(ofh *sdcorev1alpha1.RANOfh, kind string) string {
	def := map[string][]int{"prach": {4, 5, 6, 7}, "dl": {0, 1, 2, 3}, "ul": {0, 1, 2, 3}}[kind]
	v := def
	if ofh != nil {
		switch kind {
		case "prach":
			if len(ofh.PrachPortId) > 0 {
				v = ofh.PrachPortId
			}
		case "dl":
			if len(ofh.DlPortId) > 0 {
				v = ofh.DlPortId
			}
		case "ul":
			if len(ofh.UlPortId) > 0 {
				v = ofh.UlPortId
			}
		}
	}
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.Itoa(n)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// ranRadioBlock renders the split-specific radio section of gnb.yaml. Split 8 drives the SDR directly via
// ru_sdr — device "zmq" (RF simulator; gains omitted so OCUDU auto-forces them to 0 dB as ZMQ requires),
// or "b210"/"x310" (USRP over UHD). Split 7.2 uses Open Fronthaul (ru_ofh) to an external O-RU.
func ranRadioBlock(split, device, srate string, ofh *sdcorev1alpha1.RANOfh) string {
	if split == "7.2" {
		iface, ruMac, duMac, vlan := "0000:01:00.1", "aa:bb:cc:dd:ee:ff", "00:11:22:33:44:55", 3
		if ofh != nil {
			iface = orDefault(ofh.Interface, iface)
			ruMac = orDefault(ofh.RuMac, ruMac)
			duMac = orDefault(ofh.DuMac, duMac)
			vlan = orInt(ofh.VlanTag, vlan)
		}
		// Open Fronthaul (O-RAN 7.2). Needs a DPDK/raw-socket fronthaul NIC + PTP-locked timing (Phase 3).
		return fmt.Sprintf(`ru_ofh:
  ru_bandwidth_MHz: 100
  t1a_max_cp_dl: 500
  t1a_min_cp_dl: 258
  t1a_max_cp_ul: 500
  t1a_min_cp_ul: 285
  t1a_max_up: 300
  t1a_min_up: 85
  ta4_max: 300
  ta4_min: 50
  is_prach_cp_enabled: true
  compr_method_ul: bfp
  compr_bitwidth_ul: 9
  compr_method_dl: bfp
  compr_bitwidth_dl: 9
  compr_method_prach: bfp
  compr_bitwidth_prach: 9
  enable_ul_static_compr_hdr: true
  enable_dl_static_compr_hdr: true
  cells:
    - network_interface: %s
      ru_mac_addr: %s
      du_mac_addr: %s
      vlan_tag_cp: %d
      vlan_tag_up: %d
      prach_port_id: [4, 5]
      dl_port_id: [0, 1]
      ul_port_id: [0, 1]`, iface, ruMac, duMac, vlan, vlan)
	}
	switch device {
	case "b210":
		return fmt.Sprintf(`ru_sdr:
  device_driver: uhd
  device_args: type=b200
  clock: external
  srate: %s
  tx_gain: 80
  rx_gain: 40`, srate)
	case "x310":
		return fmt.Sprintf(`ru_sdr:
  device_driver: uhd
  device_args: type=x300
  clock: internal
  srate: %s
  tx_gain: 50
  rx_gain: 50`, srate)
	default: // zmq — RF simulator. Gains intentionally omitted (OCUDU forces them to 0 dB for ZMQ).
		return fmt.Sprintf(`ru_sdr:
  device_driver: zmq
  device_args: tx_port=tcp://127.0.0.1:2000,rx_port=tcp://127.0.0.1:2001,base_srate=%se6
  srate: %s`, srate, srate)
	}
}

// deriveAddressing fills podCIDR/sliceSD/uePool/egressNic from a stable per-site index if the user
// left them empty, so the EdgeSite intent stays minimal (just server + role). Persisted to the spec
// so the injected copy (apply-replacements) sees the same values. Non-overlapping via the name index.
func (r *EdgeSiteReconciler) deriveAddressing(ctx context.Context, es *sdcorev1alpha1.EdgeSite) error {
	idx := siteIndex(es.Name)
	changed := false
	set := func(p *string, v string) {
		if *p == "" {
			*p = v
			changed = true
		}
	}
	set(&es.Spec.PodCIDR, fmt.Sprintf("10.%d.0.0/16", idx))
	set(&es.Spec.UEPool, fmt.Sprintf("172.%d.0.0/16", idx))
	set(&es.Spec.SliceSD, "102030")
	set(&es.Spec.EgressNic, "eth0")
	// N3/N6 datapath addressing — third-octet indexed so it never overlaps the 10.<idx>.0.0/16 pod net
	// (idx is capped < 200 by siteIndex). These feed the edge-site blueprint's apply-replacements, which
	// stamp the emitted UPFDataPath CR the datapath actuator reconciles (bridges/routes/ARP/accessRoutes).
	// AccessCIDR/CoreCIDR are the UPF's OWN N3/N6 host addresses (.10 in the /24), not the network
	// address (.0 is invalid on an interface). Gateways = .1, gNB = .20, downlink next-hop = the UPF
	// core IP (.10). All within the site's 10.200.<idx>/10.201.<idx> subnets (idx<200, no pod overlap).
	set(&es.Spec.AccessCIDR, fmt.Sprintf("10.200.%d.10/24", idx))
	set(&es.Spec.CoreCIDR, fmt.Sprintf("10.201.%d.10/24", idx))
	set(&es.Spec.AccessGw, fmt.Sprintf("10.200.%d.1", idx))
	set(&es.Spec.CoreGw, fmt.Sprintf("10.201.%d.1", idx))
	set(&es.Spec.CoreNextHop, fmt.Sprintf("10.201.%d.10", idx))
	set(&es.Spec.GnbIP, fmt.Sprintf("10.200.%d.20", idx))
	// RAN-facing N2 exposure (n2-expose blueprint). The LB POOL is the SHARED RAN L2 range (same on every
	// site); each site pins a DISTINCT VIP (n2VIP) on its amf-ngap Service so multiple CP-bearing sites on
	// one L2 never answer ARP for one IP (which silently breaks NGAP for both). The VIP is a mid-range IP
	// from the pool — NOT a /32 pool — which sidesteps Cilium LB-IPAM's first/last-IP exclusion. .240 is the
	// mgmt CP; standalone cores get the lowest free .241-.254, persisted (stable across add/delete).
	// Environment-specific (suits the 192.168.4.0/24 lab); override spec.ranSubnet/n2VIP for another RAN.
	base := r.ranBase(ctx, es)
	set(&es.Spec.RanSubnet, base+".240/28")
	vip := base + ".240" // mgmt CP
	if !es.Spec.Colocated {
		vip = r.allocateRanVIP(ctx, es, base)
	}
	set(&es.Spec.N2VIP, vip)
	set(&es.Spec.RanNic, "eth0")
	if changed {
		return r.Update(ctx, es)
	}
	return nil
}

// siteIndex maps a site name to a stable small int in 2..198 (the non-overlapping addressing octet).
// Capped below 200 so 10.<idx>.0.0/16 (pods/UE) never collides with the 10.200.x/10.201.x N3/N6 nets.
func siteIndex(name string) int {
	h := 0
	for _, c := range name {
		h = (h*31 + int(c)) % 197
	}
	return h + 2
}

// applyOwned sets the EdgeSite as owner and create-or-updates the child (typed or unstructured).
func (r *EdgeSiteReconciler) applyOwned(ctx context.Context, es *sdcorev1alpha1.EdgeSite, obj client.Object) error {
	t := true
	obj.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: sdcorev1alpha1.SchemeGroupVersion.String(), Kind: "EdgeSite",
		Name: es.Name, UID: es.UID, Controller: &t, BlockOwnerDeletion: &t,
	}})
	cur := obj.DeepCopyObject().(client.Object)
	err := r.Get(ctx, client.ObjectKeyFromObject(obj), cur)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	// IDEMPOTENT: only Update when the desired spec actually differs from what's live. Without this the
	// reconciler rewrites the PackageVariant every loop, and Porch cuts a NEW package draft each time
	// (revision churn: packagevariant-1,2,3...). Compare the spec map for unstructured children.
	if u, ok := obj.(*unstructured.Unstructured); ok {
		if curU, ok2 := cur.(*unstructured.Unstructured); ok2 {
			want, _, _ := unstructured.NestedMap(u.Object, "spec")
			have, _, _ := unstructured.NestedMap(curU.Object, "spec")
			if reflect.DeepEqual(want, have) {
				return nil // unchanged — no update, no revision churn
			}
		}
	}
	obj.SetResourceVersion(cur.GetResourceVersion())
	return r.Update(ctx, obj)
}

func (r *EdgeSiteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.BlueprintRepo == "" {
		r.BlueprintRepo = "blueprints"
	}
	if r.MgmtRepo == "" {
		r.MgmtRepo = "mgmt"
	}
	pv := &unstructured.Unstructured{}
	pv.SetGroupVersionKind(pvGVK)
	return ctrl.NewControllerManagedBy(mgr).
		For(&sdcorev1alpha1.EdgeSite{}).
		Owns(&sdcorev1alpha1.MeshLink{}).
		Owns(pv).
		Complete(r)
}
