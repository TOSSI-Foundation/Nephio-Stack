/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	osexec "os/exec"
	"strings"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sdcorev1alpha1 "github.com/your-org/sdcore-operator/api/v1alpha1"
)

// UPFDataPathReconciler is the node-agent (DaemonSet mode): a Go controller — NOT a bash reconcile loop —
// that drives the local host + BESS datapath to match each UPFDataPath CR. It runs with hostNetwork, so
// netlink programs the host's own namespace directly (no nsenter). This is the pure-Nephio "distributed
// actuation" pillar: a Nephio-specific controller encoding the N3/N6 infra logic in code.
type UPFDataPathReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Config    *rest.Config
	NodeName  string
	clientset *kubernetes.Clientset
}

// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=upfdatapaths,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=upfdatapaths/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch

func (r *UPFDataPathReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var dp sdcorev1alpha1.UPFDataPath
	if err := r.Get(ctx, req.NamespacedName, &dp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	s := dp.Spec
	if s.UEPool == "" {
		return ctrl.Result{}, nil
	}
	accBr, coreBr := orDefault(s.AccessBridge, "sdcore-access"), orDefault(s.CoreBridge, "sdcore-core")
	nic := hostEgressNic(s.EgressNic)

	steps := []struct {
		name string
		fn   func() error
	}{
		{"bridges", func() error { return ensureBridge(accBr, coreBr) }},
		{"gateways", func() error { return ensureBridgeAddr(map[string]string{coreBr: s.CoreGateway, accBr: s.AccessGateway}) }},
		{"forwarding", func() error { return ensureForwarding(coreBr, accBr) }},
		{"n6-route", func() error { return ensureRoute(s.UEPool, s.CoreNextHop, coreBr) }},
		{"n6-nat", func() error { return ensureN6Nat(s.UEPool, nic, accBr) }},
		{"n2-vip", func() error { return r.ensureN2VIP(ctx, nic) }},
		{"upf-heal", func() error { return r.ensureUpfHealthy(ctx, orDefault(s.UPFLabel, "app=upf")) }},
		{"upf-harden", func() error { return r.ensureUpfPodHardening(ctx, orDefault(s.UPFLabel, "app=upf")) }},
		{"access-route", func() error { return r.ensureBessAccessRoute(ctx, s.GnbIP, orDefault(s.UPFLabel, "app=upf")) }},
		{"pfcp-heal", func() error { return r.ensurePfcpHealthy(ctx, orDefault(s.UPFLabel, "app=upf")) }},
		{"arp", func() error { return refreshNeigh(map[string]string{s.GnbIP: accBr, s.CoreNextHop: coreBr}) }},
	}
	// Run EVERY step, every reconcile — never abort at the first failure. The steps are independent, and
	// several are HEALS (upf-heal, pfcp-heal): aborting early starves them behind any persistently-failing
	// earlier step (e.g. a BESS exec hiccup), which leaves a broken UPF unrecovered while the loop spins.
	// Failures are collected; the first one names the Ready condition, all are logged.
	var failed []string
	for _, st := range steps {
		if err := st.fn(); err != nil {
			log.Error(err, "UPFDataPath step failed", "datapath", dp.Name, "node", r.NodeName, "step", st.name)
			failed = append(failed, fmt.Sprintf("%s: %v", st.name, err))
		}
	}
	dp.Status.Node = r.NodeName
	if len(failed) > 0 {
		setCondition(&dp.Status.Conditions, "Ready", metav1.ConditionFalse, "StepsFailed",
			strings.Join(failed, "; "), dp.Generation)
		_ = r.Status().Update(ctx, &dp)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	dp.Status.LastReconcile = time.Now().UTC().Format(time.RFC3339)
	setCondition(&dp.Status.Conditions, "Ready", metav1.ConditionTrue, "Reconciled",
		fmt.Sprintf("host+BESS datapath programmed (uePool=%s gnb=%s) on %s", s.UEPool, s.GnbIP, r.NodeName), dp.Generation)
	if err := r.Status().Update(ctx, &dp); err != nil {
		log.Error(err, "UPFDataPath status update", "datapath", dp.Name)
	}
	// self-healing: re-run periodically (survives UPF restarts / ARP staleness / pipeline reprogramming).
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// hostEgressNic resolves the internet-facing NIC host-agnostically. It honors an explicitly-configured
// interface ONLY if it actually exists on THIS node; otherwise it auto-detects the default-route
// interface. Never assume "eth0" — real servers use eno1/enp*s*/ens* etc. A wrong NIC here means the N6
// MASQUERADE is programmed on a non-existent link, so the UE gets a PDU session + IP but no internet.
func hostEgressNic(preferred string) string {
	if preferred != "" {
		if _, err := netlink.LinkByName(preferred); err == nil {
			return preferred
		}
	}
	// Most robust: ask the kernel which interface actually REACHES the internet. A default-route scan is
	// fragile — some hosts represent the default as 0.0.0.0/0 (not a nil Dst) or keep it in a non-main table
	// (cilium), so a `Dst == nil` scan silently misses it and falls through to eth0 (the exact bug that made
	// n6-nat/n2-vip target a dead link on a real server).
	if routes, err := netlink.RouteGet(net.ParseIP("8.8.8.8")); err == nil {
		for _, rt := range routes {
			if rt.LinkIndex > 0 {
				if link, err := netlink.LinkByIndex(rt.LinkIndex); err == nil {
					return link.Attrs().Name
				}
			}
		}
	}
	// fallback: scan for a default route, matching BOTH a nil Dst and an explicit 0.0.0.0/0 Dst.
	if routes, err := netlink.RouteList(nil, netlink.FAMILY_V4); err == nil {
		for _, rt := range routes {
			isDefault := rt.Dst == nil || rt.Dst.IP.Equal(net.IPv4zero)
			if isDefault && rt.LinkIndex > 0 {
				if link, err := netlink.LinkByIndex(rt.LinkIndex); err == nil {
					return link.Attrs().Name
				}
			}
		}
	}
	// No detectable egress NIC. Do NOT guess "eth0" — a wrong NIC silently lands the N6 MASQUERADE on a dead
	// link (UE gets an IP but no internet, and the step still reports success). Return "" so the datapath
	// steps that need it FAIL loudly and surface a real error instead of a broken-but-Ready datapath.
	return ""
}

func orDefault(v, d string) string {
	if v == "" || v == "null" {
		return d
	}
	return v
}

// orInt returns d when v is the zero value (an unset optional int field), else v.
func orInt(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

// indent prefixes every non-empty line of s with pad (for embedding a YAML doc under a ConfigMap key).
func indent(s, pad string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(pad + line + "\n")
	}
	return b.String()
}

// ensureBridge creates the N3/N6 Linux bridges if absent and brings them up (netlink; idempotent).
func ensureBridge(names ...string) error {
	for _, n := range names {
		if n == "" || n == "null" {
			continue
		}
		l, err := netlink.LinkByName(n)
		if err != nil {
			br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: n}}
			if err := netlink.LinkAdd(br); err != nil {
				return fmt.Errorf("add bridge %s: %w", n, err)
			}
			l = br
		}
		if err := netlink.LinkSetUp(l); err != nil {
			return fmt.Errorf("set up %s: %w", n, err)
		}
	}
	return nil
}

// ensureBridgeAddr puts the N3/N6 gateway IPs (/24) on their bridges so the host is a node on both L2s.
func ensureBridgeAddr(brToGw map[string]string) error {
	for br, gw := range brToGw {
		if gw == "" || gw == "null" {
			continue
		}
		l, err := netlink.LinkByName(br)
		if err != nil {
			return fmt.Errorf("link %s: %w", br, err)
		}
		addr, err := netlink.ParseAddr(gw + "/24")
		if err != nil {
			return fmt.Errorf("parse %s: %w", gw, err)
		}
		existing, _ := netlink.AddrList(l, netlink.FAMILY_V4)
		found := false
		for _, a := range existing {
			if a.IP.Equal(addr.IP) {
				found = true
				break
			}
		}
		if !found {
			if err := netlink.AddrAdd(l, addr); err != nil && !strings.Contains(err.Error(), "exists") {
				return fmt.Errorf("addr add %s on %s: %w", gw, br, err)
			}
		}
	}
	return nil
}

// ensureForwarding turns on ip_forward and disables rp_filter on the datapath ifaces (sysctl via /proc), and
// disables checksum offload on the host datapath bridges. Offload matters because forwarded packets the host
// MODIFIES (un-NAT of the N6 downlink, MSS clamp) can otherwise leave the veth in a PARTIAL/placeholder
// checksum state; BESS reads that raw frame via AF_PACKET and forwards it to the gNB with a bad inner TCP
// checksum, so the UE silently drops it — DNS/UDP works but TCP/HTTPS stalls ("5G + uplink, no web"). ethtool
// is best-effort: if the agent image lacks it the reconcile still converges.
func ensureForwarding(brs ...string) error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644); err != nil {
		return fmt.Errorf("ip_forward: %w", err)
	}
	for _, k := range append([]string{"all"}, brs...) {
		_ = os.WriteFile("/proc/sys/net/ipv4/conf/"+k+"/rp_filter", []byte("0"), 0644)
	}
	for _, br := range brs {
		_ = osexec.Command("ethtool", "-K", br, "tx", "off", "rx", "off", "tso", "off", "gso", "off", "gro", "off").Run()
	}
	return nil
}

// ensureRoute programs the downlink host route: UE pool via the UPF's N6 next-hop on the core bridge.
func ensureRoute(uePool, coreNH, coreBr string) error {
	if coreNH == "" || coreNH == "null" {
		return nil
	}
	_, dst, err := net.ParseCIDR(uePool)
	if err != nil {
		return fmt.Errorf("uePool %s: %w", uePool, err)
	}
	l, err := netlink.LinkByName(coreBr)
	if err != nil {
		return fmt.Errorf("link %s: %w", coreBr, err)
	}
	return netlink.RouteReplace(&netlink.Route{Dst: dst, Gw: net.ParseIP(coreNH), LinkIndex: l.Attrs().Index})
}

// ensureN6Nat installs N6 breakout: MASQUERADE the UE pool out the egress NIC, FORWARD accept the pool
// both ways, plus the access hairpin. Programmed in BOTH iptables and iptables-legacy (CK8s may use either).
func ensureN6Nat(uePool, nic, accBr string) error {
	if nic == "" {
		return fmt.Errorf("no egress NIC detected (auto-detect failed and spec.egressNic unset/invalid) — refusing to program N6 NAT on a guessed link; set spec.egressNic to the internet-facing interface")
	}
	applied := 0
	var errs []string
	for _, backend := range []string{"iptables", "iptables-legacy"} {
		ipt, err := iptables.New(iptables.IPFamily(iptables.ProtocolIPv4), iptables.Path(backend))
		if err != nil {
			continue // backend binary not present in the image — try the other
		}
		// Use INSERT, not AppendUnique, for the MASQUERADE: on nft hosts where kube-proxy/cilium own the nat
		// POSTROUTING chain, AppendUnique's `-C` existence check misreports the rule as present and the
		// APPEND never lands — the reconcile reports success while the UE has no internet (observed live on a
		// cp+upf core; the filter/FORWARD Inserts below landed but the nat MASQUERADE did not). Insert at
		// position 1 provably works there; delete-first keeps it idempotent (exactly one rule per reconcile).
		masq := []string{"-s", uePool, "-o", nic, "-j", "MASQUERADE"}
		_ = ipt.DeleteIfExists("nat", "POSTROUTING", masq...)
		if err := ipt.Insert("nat", "POSTROUTING", 1, masq...); err != nil {
			errs = append(errs, fmt.Sprintf("%s nat: %v", backend, err))
		} else {
			applied++
		}
		// The UE-pool FORWARD accepts MUST sit ABOVE Docker's DROP chains (DOCKER-USER/DOCKER-FORWARD): a node
		// that also runs docker inserts those, and they silently drop the UE<->egress traffic (the UE gets 5G
		// + an IP but no internet). INSERT at the TOP of FORWARD (not Append, which lands below docker), and
		// punch DOCKER-USER too when present. Delete-first keeps exactly one idempotent rule per reconcile.
		ins := func(table, chain string, rule ...string) {
			_ = ipt.DeleteIfExists(table, chain, rule...)
			_ = ipt.Insert(table, chain, 1, rule...)
		}
		ins("filter", "FORWARD", "-s", uePool, "-j", "ACCEPT")
		ins("filter", "FORWARD", "-d", uePool, "-j", "ACCEPT")
		_ = ipt.AppendUnique("filter", "FORWARD", "-i", accBr, "-o", accBr, "-j", "ACCEPT")
		if ok, _ := ipt.ChainExists("filter", "DOCKER-USER"); ok {
			ins("filter", "DOCKER-USER", "-s", uePool, "-j", "ACCEPT")
			ins("filter", "DOCKER-USER", "-d", uePool, "-j", "ACCEPT")
		}
		// GTP-U shrinks the UE's path MTU; clamp TCP MSS so HTTPS/large flows + speed tests don't stall on
		// oversized segments that can't fragment through the tunnel (SYN both directions). 1360 is proven.
		_ = ipt.AppendUnique("mangle", "FORWARD", "-p", "tcp", "-s", uePool, "--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--set-mss", "1360")
		_ = ipt.AppendUnique("mangle", "FORWARD", "-p", "tcp", "-d", uePool, "--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--set-mss", "1360")
	}
	// Success needs the MASQUERADE in at LEAST one backend (both nft+legacy are honored by the kernel). Only
	// fail if EVERY backend failed — so a host with a single working backend still converges.
	if applied == 0 && len(errs) > 0 {
		return fmt.Errorf("n6 UE-pool MASQUERADE not applied in any backend: %s", strings.Join(errs, "; "))
	}
	return nil
}

// refreshNeigh deletes stale/permanent neighbor entries (a leftover MAC after a gNB/UPF restart silently
// blackholes the downlink) so the kernel re-learns them. netlink NeighDel removes PERMANENT (flush won't).
func refreshNeigh(ipToDev map[string]string) error {
	for ip, dev := range ipToDev {
		if ip == "" || ip == "null" {
			continue
		}
		l, err := netlink.LinkByName(dev)
		if err != nil {
			continue
		}
		nip := net.ParseIP(ip)
		neighs, _ := netlink.NeighList(l.Attrs().Index, netlink.FAMILY_V4)
		for _, n := range neighs {
			if n.IP.Equal(nip) && n.State != netlink.NUD_REACHABLE {
				_ = netlink.NeighDel(&n)
			}
		}
	}
	return nil
}

// ensureBessAccessRoute adds the downlink accessRoute for the gNB into the UPF's BESS pipeline. BESS is
// driven over its CLI inside the UPF pod (there's no KRM/host path to BESS), so the node-agent execs it —
// the one unavoidable pod-exec, scoped to the UPF's own datapath. Idempotent (BESS de-dups the prefix).
func (r *UPFDataPathReconciler) ensureBessAccessRoute(ctx context.Context, gnbIP, upfLabel string) error {
	if gnbIP == "" || gnbIP == "null" || r.clientset == nil {
		return nil
	}
	pods, err := r.clientset.CoreV1().Pods("default").List(ctx, metav1.ListOptions{LabelSelector: upfLabel})
	if err != nil || len(pods.Items) == 0 {
		return nil // UPF not up yet; retried next reconcile
	}
	// Only exec once a UPF pod is Running AND its bessd container has STARTED. Otherwise the exec races the
	// UPF's own init (bessd not created yet) and errors "container not found (bessd)". Treating that as a
	// hard failure aborts the WHOLE reconcile — the host datapath (bridges/routes/NAT already programmed by
	// the earlier steps) never completes and Ready never latches. So SKIP gracefully and let the 15s requeue
	// add the accessRoute the moment bessd is live. (Chicken-and-egg: bessd needs the bridges we just set up;
	// the accessRoute needs bessd — decouple them so neither blocks the other.)
	var target string
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == "bessd" && cs.Started != nil && *cs.Started {
				target = p.Name
			}
		}
		if target != "" {
			break
		}
	}
	if target == "" {
		return nil // no UPF with a live bessd yet — retry next reconcile, don't fail the datapath
	}
	arg := fmt.Sprintf(`{"prefix":"%s","prefix_len":32,"gate":0}`, gnbIP)
	if err := r.podExec(ctx, target, "bessd",
		[]string{"bessctl", "command", "module", "accessRoutes", "add", "IPLookupCommandAddArg", arg}); err != nil {
		return err
	}
	// The route above sends downlink to accessRoutes gate0; its L2 rewrite (the accessDstMAC Update module wired
	// to that gate) MUST carry the REAL MAC of the next hop toward the gNB. route_control derives it from ARP and
	// routinely latches the WRONG MAC — the access gateway's, a pre-restart random gNB MAC, or (for a host/routed
	// gNB) a SYNTHETIC pinned MAC that exists nowhere on the wire — so the host kernel L2-drops (PACKET_OTHERHOST)
	// EVERY downlink packet: the UE gets 5G + uplink but no internet, with no drop counter to show for it. Resolve
	// the actual neighbour MAC live in the UPF netns and stamp it. Correct for ANY gNB topology, self-heals across
	// restarts, and reads the same MAC a same-L2 pod gNB is pinned to:
	//   - gNB on the UPF's access L2 (operator-deployed pod): next hop = the gNB itself.
	//   - gNB on the host / a different subnet (OAI, OCUDU host-mode, any routed N3): next hop = access gateway.
	pyScript := fmt.Sprintf(`import re,subprocess
from pybess.bess import BESS
def sh(c): return subprocess.run(c,shell=True,capture_output=True,text=True)
gnb=%q
am=re.search(r'(\d+\.\d+\.\d+\.\d+)/\d+',sh("ip -o -4 addr show dev access").stdout)
gw=re.sub(r'\.\d+$','.1',am.group(1)) if am else ""
if sh("ip route get "+gnb).returncode!=0 and gw:
    sh("ip route replace "+gnb+"/32 via "+gw+" dev access")
m=re.search(r'via (\d+\.\d+\.\d+\.\d+)',sh("ip route get "+gnb).stdout)
nh=m.group(1) if m else gnb
sh("ping -c1 -W1 "+nh)
mm=re.search(r'(([0-9a-f]{2}:){5}[0-9a-f]{2})',sh("ip neigh show "+nh+" dev access").stdout)
if not mm:
    print("no-neigh-mac-yet",nh); raise SystemExit(0)
val=int(mm.group(1).replace(":",""),16)
b=BESS(); b.connect(grpc_url="localhost:10514"); b.pause_all()
try:
    info=b.get_module_info("accessRoutes")
    dst=None
    for g in info.ogates:
        if g.ogate==0: dst=g.name
    if dst:
        try: b.run_module_command(dst,"clear","EmptyArg",{})
        except Exception: pass
        b.run_module_command(dst,"add","UpdateArg",{"fields":[{"offset":0,"size":6,"value":val}]})
        print("dstmac-set",dst,mm.group(1))
finally:
    b.resume_all()
`, gnbIP)
	// best-effort: the neighbour or the gate module may not be resolvable on the first pass; 15s requeue retries.
	_ = r.podExec(ctx, target, "routectl", []string{"python3", "-c", pyScript})
	return nil
}

// ensureUpfPodHardening applies the two UPF-netns settings the af_packet datapath needs but the image ships
// wrong, each of which otherwise silently breaks real-UE internet (5G attaches, uplink works, no web):
//   - ip_forward=0: the un-NATed N6 downlink (dst = the UE IP) lands on the UPF's core iface; BESS consumes it
//     via AF_PACKET, but with ip_forward=1 the pod KERNEL ALSO forwards its copy back out access — a host<->pod
//     routing LOOP that TTL-expires every downlink packet (seen live as "ICMP time exceeded" from the UPF pod).
//   - tx/rx checksum offload OFF on access+core: AF_PACKET hands BESS raw frames; with offload the inner TCP
//     checksum is left PARTIAL and the UE drops the segment (DNS/UDP fine, TCP/HTTPS stalls). Off = correct.
// Idempotent, re-applied every reconcile so it survives UPF pod restarts. Best-effort: a missing sysctl/ethtool
// never fails the datapath (the earlier host steps have already converged).
func (r *UPFDataPathReconciler) ensureUpfPodHardening(ctx context.Context, upfLabel string) error {
	if r.clientset == nil {
		return nil
	}
	pods, err := r.clientset.CoreV1().Pods("default").List(ctx, metav1.ListOptions{LabelSelector: upfLabel})
	if err != nil || len(pods.Items) == 0 {
		return nil
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		started := false
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == "bessd" && cs.Started != nil && *cs.Started {
				started = true
			}
		}
		if !started {
			continue
		}
		sh := `echo 0 > /proc/sys/net/ipv4/ip_forward 2>/dev/null || sysctl -w net.ipv4.ip_forward=0 >/dev/null 2>&1 || true
for i in access core; do ethtool -K "$i" tx off rx off tso off gso off gro off >/dev/null 2>&1 || true; done`
		_ = r.podExec(ctx, p.Name, "bessd", []string{"sh", "-c", sh})
		return nil
	}
	return nil
}

// ensurePfcpHealthy self-heals the SD-Core UPF's well-known PFCP-vs-bessd startup race: the pfcp-agent's
// pfcpiface connects to bessd once at start and, if the BESS pipeline wasn't ready yet, latches "datapath
// down" and rejects EVERY SMF Association Setup forever (SMF then rejects the PDU session
// "UPF not associated"). There's no retry in the image, so the node-agent (the datapath domain controller)
// heals it: if a running UPF's pfcp-agent is still logging "datapath down", restart pfcpiface so it
// re-attaches to the now-ready bessd. Self-limiting — once associated the log clears and this no-ops.
func (r *UPFDataPathReconciler) ensurePfcpHealthy(ctx context.Context, upfLabel string) error {
	if r.clientset == nil {
		return nil
	}
	pods, err := r.clientset.CoreV1().Pods("default").List(ctx, metav1.ListOptions{LabelSelector: upfLabel})
	if err != nil || len(pods.Items) == 0 {
		return nil
	}
	tail := int64(20)
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		stream, err := r.clientset.CoreV1().Pods(p.Namespace).GetLogs(p.Name,
			&corev1.PodLogOptions{Container: "pfcp-agent", TailLines: &tail}).Stream(ctx)
		if err != nil {
			continue // pfcp-agent not up yet; retry next reconcile
		}
		buf := new(bytes.Buffer)
		_, _ = io.Copy(buf, stream)
		_ = stream.Close()
		if strings.Contains(buf.String(), "datapath down") {
			// bounce pfcpiface -> fresh attach to the ready bessd -> association succeeds. Best-effort.
			_ = r.podExec(ctx, p.Name, "pfcp-agent", []string{"pkill", "-TERM", "pfcpiface"})
		}
	}
	return nil
}

// ensureN2VIP owns ARP for the RAN-facing N2 VIP. The n2-expose blueprint gives the AMF NGAP Service a
// LoadBalancer IP from the RAN pool (Cilium LB-IPAM assigns it, and Cilium programs the SCTP frontend in
// BPF even without kube-proxy replacement) — but NOTHING answers ARP for that VIP on CK8s: Cilium's L2
// announcer hard-requires kubeProxyReplacement=true, and CK8s's helm reconciler force-reverts that key
// (it runs kube-proxy), so a real gNB's SCTP INIT dies as an unanswered ARP "who-has <vip>". The
// thesis-native fix is this node-agent owning the VIP like it owns the rest of the host datapath: put
// each exposed Service's LB IP on the RAN NIC as a /32 so the kernel answers ARP and the BPF frontend
// does the rest. Selected by label (sdcore.nephio.io/expose: n2) so it is intent-driven; no-op on
// clusters with no exposed N2 (the edges).
func (r *UPFDataPathReconciler) ensureN2VIP(ctx context.Context, nic string) error {
	if nic == "" {
		return nil // no egress NIC to announce on; ensureN6Nat already surfaces the missing-NIC error
	}
	var svcs corev1.ServiceList
	if err := r.List(ctx, &svcs, client.MatchingLabels{"sdcore.nephio.io/expose": "n2"}); err != nil {
		return nil // CRD/cache not ready is not a datapath failure
	}
	want := map[string]bool{} // the VIPs this node SHOULD announce right now
	for i := range svcs.Items {
		for _, ing := range svcs.Items[i].Status.LoadBalancer.Ingress {
			if ing.IP != "" {
				want[ing.IP] = true
			}
		}
	}
	l, err := netlink.LinkByName(nic)
	if err != nil {
		if len(want) == 0 {
			return nil // no N2 exposure here and no RAN NIC — nothing to do
		}
		return fmt.Errorf("n2-vip link %s: %w", nic, err)
	}
	for ip := range want {
		addr, perr := netlink.ParseAddr(ip + "/32")
		if perr != nil {
			continue
		}
		if err := netlink.AddrReplace(l, addr); err != nil {
			return fmt.Errorf("n2-vip %s on %s: %w", ip, nic, err)
		}
	}
	// Remove STALE VIPs: a /32 on the RAN NIC that this node once announced but is no longer a current
	// expose=n2 LB IP (e.g. the VIP was reassigned .240 -> .241). Leaving it risks a cross-site ARP
	// collision. Only /32 host routes are touched, and only those not currently wanted; the node's real
	// addresses (non-/32, or /32s that ARE wanted) are never removed.
	existing, _ := netlink.AddrList(l, netlink.FAMILY_V4)
	for i := range existing {
		a := existing[i]
		ones, bits := a.Mask.Size()
		if ones == 32 && bits == 32 && !want[a.IP.String()] && isRanVIPCandidate(a.IP.String()) {
			_ = netlink.AddrDel(l, &existing[i])
		}
	}
	return nil
}

// isRanVIPCandidate reports whether a /32 on the RAN NIC looks like a managed N2 VIP (in the RAN VIP pool)
// rather than the node's own address — so stale-VIP cleanup never removes a legitimate host IP. The VIP
// pool is always the .240-.254 host range of whatever RAN subnet is in use, so match on the last octet —
// env-agnostic (no hardcoded lab subnet).
func isRanVIPCandidate(ip string) bool {
	var a, b, c, d int
	if n, _ := fmt.Sscanf(ip, "%d.%d.%d.%d", &a, &b, &c, &d); n == 4 {
		return d >= 240 && d <= 254
	}
	return false
}

// ensureUpfHealthy self-heals a UPF that fell victim to the multus-ordering race. On a fresh edge cluster
// Config Sync delivers the multus DaemonSet and the UPF with NO ordering guarantee, so the UPF sandbox is
// often created BEFORE multus is the node's CNI. When that happens the UPF gets cilium-only networking and
// its `networks` annotation is never processed — the access/core interfaces (and their IPs) never attach.
// bess-init then fails "ip route via <access-gw>: invalid gateway" and crash-loops forever, because only
// the initContainer restarts (the sandbox is never recreated, so multus never gets a second chance).
//
// The cure is to delete the pod so the StatefulSet recreates it with a FRESH sandbox — by which time multus
// is ready, so access/core attach and the UPF comes up clean. We detect the race two independent ways so it
// is robust across every environment/timing:
//   (a) crash-loop  — bess-init keeps failing (restartCount climbs); a genuine "invalid gateway" loop.
//   (b) net-missing — the pod has existed >45s but multus never recorded access+core in network-status.
// Uses the controller-runtime client (always initialized) for list+delete — NOT the raw clientset (which,
// if it ever failed to init, would silently no-op this heal while the reconcile still reported success).
// Self-limiting: a Ready UPF is never touched, so it converges to one clean recreate and then no-ops.
func (r *UPFDataPathReconciler) ensureUpfHealthy(ctx context.Context, upfLabel string) error {
	sel, err := labels.Parse(upfLabel)
	if err != nil {
		return nil
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, &client.ListOptions{Namespace: "default", LabelSelector: sel}); err != nil {
		return nil
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil || podReady(p) {
			continue // already being replaced, or healthy — never touch a Ready UPF
		}
		var restarts int32
		for _, cs := range p.Status.InitContainerStatuses {
			restarts += cs.RestartCount
		}
		crashLoop := restarts >= 3
		netMissing := time.Since(p.CreationTimestamp.Time) > 45*time.Second && !hasSecondaryNets(p, "access", "core")
		if crashLoop || netMissing {
			_ = r.Delete(ctx, p)
		}
	}
	return nil
}

// podReady reports whether the pod's Ready condition is True.
func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// hasSecondaryNets reports whether multus recorded EVERY named secondary interface in the pod's
// network-status annotation. An empty/absent annotation (the race signature) fails this for any name.
func hasSecondaryNets(p *corev1.Pod, names ...string) bool {
	status := p.Annotations["k8s.v1.cni.cncf.io/network-status"]
	for _, n := range names {
		if !strings.Contains(status, `"interface":"`+n+`"`) && !strings.Contains(status, `"interface": "`+n+`"`) {
			return false
		}
	}
	return true
}

func (r *UPFDataPathReconciler) podExec(ctx context.Context, pod, container string, cmd []string) error {
	req := r.clientset.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace("default").SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: container, Command: cmd, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(r.Config, "POST", req.URL())
	if err != nil {
		return err
	}
	var stdout, stderr bytes.Buffer
	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
}

func (r *UPFDataPathReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Config == nil {
		r.Config = mgr.GetConfig()
	}
	cs, err := kubernetes.NewForConfig(r.Config)
	if err != nil {
		return err
	}
	r.clientset = cs
	if r.NodeName == "" {
		r.NodeName = os.Getenv("NODE_NAME")
	}
	return ctrl.NewControllerManagedBy(mgr).For(&sdcorev1alpha1.UPFDataPath{}).Complete(r)
}
