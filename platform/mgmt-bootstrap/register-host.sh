#!/usr/bin/env bash
# HOST-JOIN — the one command that turns a bare server into a Nephio-managed host. It preps the OS and
# registers a BYOH agent with the management plane; after this, the mgmt cluster can provision Canonical
# K8s on this server purely by intent. This is BOOTSTRAP (you can't use Nephio to onboard a machine that
# isn't a cluster yet) and the ONLY imperative code that runs on a target server — the 5G product on top
# is pure KRM. Runs standalone (`curl .../register | sudo bash`), so helpers are embedded, not sourced.
#   Usage: sudo bash register-host.sh <MGMT_LAN_IP>
set -euo pipefail
MGMT_IP="${1:?usage: register-host.sh <MGMT_LAN_IP>}"
BYOH_AGENT_URL="${BYOH_AGENT_URL:-http://$MGMT_IP:8080/byoh-hostagent}"

# ---- embedded presentation helpers (self-contained; mirrors mgmt-bootstrap/lib.sh) --------------------
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then _B=$'\033[1m'; _D=$'\033[2m'; _G=$'\033[32m'; _R=$'\033[31m'; _C=$'\033[36m'; _0=$'\033[0m'
else _B=''; _D=''; _G=''; _R=''; _C=''; _0=''; fi
phase(){ printf '\n%s▸ %s%s\n' "${_B}${_C}" "$*" "${_0}"; }
say(){   printf '   %s%s%s\n' "${_D}" "$*" "${_0}"; }
ok(){    printf '   %s✓%s %s\n' "${_G}" "${_0}" "$*"; }
run(){ local d="$1"; shift; local l; l="$(mktemp)"; [ -t 1 ] && printf '   %s…%s %s' "${_D}" "${_0}" "$d"
  if "$@" >"$l" 2>&1; then [ -t 1 ] && printf '\r'; printf '   %s✓%s %s\n' "${_G}" "${_0}" "$d"; rm -f "$l"; else [ -t 1 ] && printf '\r'; printf '   %s✗%s %s\n' "${_R}" "${_0}" "$d"; sed 's/^/       /' "$l" >&2; rm -f "$l"; return 1; fi; }

phase "Host prep for Canonical K8s"
run "disable swap" swapoff -a
# A reused host can carry a prior CNI's leftovers that collide with CK8s Cilium: stale interfaces AND stale
# /etc/cni/net.d confs (a *calico*/*canal*/*flannel*/*multus* conf sorts before cilium's and shadows it ->
# every pod stuck ContainerCreating, or "plugin type=\"calico\" not found" pod-sandbox failures).
for i in flannel.1 cni0 vxlan.calico; do ip link delete "$i" 2>/dev/null || true; done
find /etc/cni/net.d -maxdepth 1 -type f \( -iname '*multus*' -o -iname '*calico*' -o -iname '*canal*' -o -iname '*flannel*' -o -iname '*weave*' \) -delete 2>/dev/null || true
# Clear stale CAPI bootstrap material + prior CK8s state: a leftover /etc/kubernetes/pki/ca.crt makes the
# CABPCK bootstrap think the node is ALREADY bootstrapped, so it skips `k8s bootstrap` and hangs forever.
# (`snap remove k8s --purge` does NOT clear /etc/kubernetes.)
rm -rf /capi/etc /capi/run /etc/kubernetes 2>/dev/null || true
# A host that previously belonged to the fabric may carry a stale CK8s snap + datapath bridges. Reset them
# HERE, at (re-)onboard, so the install starts pristine. This is WHY teardown needs no host-wipe script:
# `kubectl delete` releases the host declaratively (CAPI deprovisions the cluster), and the host is made
# bare at its next onboarding instead — 'release' and 'wipe' decoupled, zero SSH-per-host on delete.
if snap list k8s >/dev/null 2>&1; then run "reset stale CK8s from a prior fabric membership" snap remove k8s --purge; fi
for br in sdcore-access sdcore-core; do ip link delete "$br" 2>/dev/null || true; done
rm -rf /var/lib/ck8s-containerd 2>/dev/null || true
printf 'overlay\nbr_netfilter\n' > /etc/modules-load.d/k8s.conf
run "load kernel modules (overlay, br_netfilter)" modprobe overlay br_netfilter
printf 'net.ipv4.ip_forward=1\nnet.bridge.bridge-nf-call-iptables=1\n' > /etc/sysctl.d/k8s.conf
run "apply sysctls" sysctl --system
# docker (if present) forces iptables FORWARD=DROP, killing pod egress — keep it open + persist.
iptables -P FORWARD ACCEPT || true
printf '[Unit]\nDescription=keep FORWARD open for pod egress\nAfter=docker.service\n[Service]\nType=oneshot\nExecStart=/usr/sbin/iptables -P FORWARD ACCEPT\n[Install]\nWantedBy=multi-user.target\n' > /etc/systemd/system/pod-egress.service
systemctl enable pod-egress.service >/dev/null 2>&1 || true
# Wait out any apt lock (unattended-upgrades / concurrent apt on a fresh Ubuntu box), then install deps.
# NB: NOT containerd — CK8s bundles its own; a host-level one would own /run/containerd and break bootstrap.
for i in $(seq 1 60); do
  fuser /var/lib/dpkg/lock-frontend /var/lib/apt/lists/lock /var/cache/apt/archives/lock >/dev/null 2>&1 || break
  [ "$i" = 1 ] && say "waiting for apt/dpkg lock (unattended-upgrades)…"; sleep 5
done
run "install host deps (socat, ebtables, ethtool, conntrack)" \
  bash -c "apt-get update -qq && apt-get install -y -qq socat ebtables ethtool conntrack"
# A leftover Docker keeps its own containerd up under dockerd, blocking CK8s from claiming /run/containerd.
if systemctl is-active --quiet docker 2>/dev/null || [ -S /run/containerd/containerd.sock ]; then
  systemctl disable --now docker docker.socket containerd 2>/dev/null || true
  systemctl mask docker.socket 2>/dev/null || true
  rm -rf /run/containerd /var/run/containerd 2>/dev/null || true
  ok "removed leftover Docker so CK8s can own containerd"
fi
# A prior FAILED BYOH bootstrap can leave the CK8s `k8s` snap half-installed: its k8sd owns /run/containerd
# but the node was never bootstrapped, so the NEXT `k8s bootstrap` aborts its pre-init check ("/run/containerd
# already exists"). If the snap is present but NOT part of a cluster, purge it so bootstrap starts clean.
if snap list k8s >/dev/null 2>&1 && ! k8s status >/dev/null 2>&1; then
  snap remove k8s --purge >/dev/null 2>&1 || true
  rm -rf /run/containerd /var/run/containerd 2>/dev/null || true
  ok "purged stale unbootstrapped k8s snap (freed /run/containerd)"
fi

phase "Register with the management plane"
# push model (make add-server scp'd the binary) preferred; else self-serve from the mgmt endpoint.
if [ -f /tmp/byoh-hostagent ]; then install -m0755 /tmp/byoh-hostagent /usr/local/bin/byoh-hostagent
else curl -fsSL "$BYOH_AGENT_URL" -o /usr/local/bin/byoh-hostagent && chmod +x /usr/local/bin/byoh-hostagent; fi
ok "installed registration agent"
mkdir -p /root/.byoh
# STOP any agent from a PRIOR registration: on a re-onboarded host (was an edge before the mgmt was
# rebuilt) the old agent keeps running with the OLD cluster CA -> "unknown authority" against the new
# apiserver, and `enable --now` would NOT restart it. Stop it so the fresh creds take effect.
systemctl stop byoh-agent 2>/dev/null || true
# The agent runs its CSR bootstrap only if --bootstrap-kubeconfig is set AND ~/.byoh/config is ABSENT.
rm -f /root/.byoh/config
if [ -f /tmp/bootstrap-kubeconfig.conf ]; then cp /tmp/bootstrap-kubeconfig.conf /root/.byoh/bootstrap.conf
else curl -fsSL "http://$MGMT_IP:8080/bootstrap-kubeconfig.conf" -o /root/.byoh/bootstrap.conf; fi
cat >/etc/systemd/system/byoh-agent.service <<EOF
[Unit]
Description=BYOH host agent
After=network-online.target
StartLimitIntervalSec=0
[Service]
WorkingDirectory=/root
# --bootstrap-kubeconfig -> the agent submits a CSR with the bootstrap token, gets a byoh:hosts cert, and
# registers itself as a ByoHost with that cert.
ExecStart=/usr/local/bin/byoh-hostagent --bootstrap-kubeconfig /root/.byoh/bootstrap.conf --namespace default --skip-installation
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
EOF
run "start BYOH agent (CSR bootstrap -> ByoHost)" \
  bash -c "systemctl daemon-reload && systemctl enable byoh-agent && systemctl restart byoh-agent"
# The agent writes ~/.byoh/config after CSR bootstrap but DROPS the cluster CA-data, so it then can't
# verify the mgmt apiserver cert (x509: unknown authority "kubernetes-ca") and never registers. Re-inject
# the CA (carried in bootstrap.conf) and restart. Baked in so it never needs a manual fix again.
for i in $(seq 1 30); do [ -f /root/.byoh/config ] && break; sleep 2; done
CA=$(grep 'certificate-authority-data:' /root/.byoh/bootstrap.conf 2>/dev/null | awk '{print $2}' | head -1)
if [ -n "$CA" ] && ! grep -q 'certificate-authority-data:' /root/.byoh/config 2>/dev/null; then
  sed -i '/insecure-skip-tls-verify:/d' /root/.byoh/config
  sed -i "\|server: https|a\\    certificate-authority-data: $CA" /root/.byoh/config
  systemctl restart byoh-agent
  ok "re-injected cluster CA into agent config (agent drops it otherwise)"
fi
ok "host registered — declare intent on the mgmt cluster to provision it"
