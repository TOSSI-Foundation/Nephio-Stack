#!/usr/bin/env bash
# Generate the BYOH bootstrap kubeconfig that a host agent uses to self-register with the mgmt
# cluster (creates a BootstrapKubeconfig CR; the BYOH controller mints a short-lived, CSR-capable
# kubeconfig in its status). Host-agnostic: apiserver + CA are read from the live mgmt kubeconfig.
set -euo pipefail
source "$(cd "$(dirname "$0")" && pwd)/lib.sh"
: "${MGMT_LAN_IP:?set MGMT_LAN_IP}"
OUT="${1:-/tmp/bootstrap-kubeconfig.conf}"
APISERVER="https://${MGMT_LAN_IP}:6443"
CA_CERT="$(kubectl config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
[ -n "$CA_CERT" ] || die "no certificate-authority-data in the mgmt kubeconfig"

# Force a FRESH token every run: delete any prior BootstrapKubeconfig so the controller re-mints. A plain
# `apply` on an existing CR keeps the OLD status token, which has a short TTL and may have EXPIRED between
# earlier (failed) attempts -> the agent's CSR then gets 401 Unauthorized. Delete = always a live token.
kubectl delete bootstrapkubeconfig byoh-bootstrap -n default --ignore-not-found >/dev/null 2>&1 || true
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: BootstrapKubeconfig
metadata:
  name: byoh-bootstrap
  namespace: default
spec:
  apiserver: "${APISERVER}"
  certificate-authority-data: "${CA_CERT}"
  insecure-skip-tls-verify: false
EOF

say "waiting for the BootstrapKubeconfig controller to mint credentials…"
data=""
for _ in $(seq 1 30); do
  data="$(kubectl get bootstrapkubeconfig byoh-bootstrap -n default \
    -o jsonpath='{.status.bootstrapKubeconfigData}' 2>/dev/null || true)"
  [ -n "$data" ] && break
  sleep 4
done
[ -n "$data" ] || die "BootstrapKubeconfig produced no data (BYOH<->CK8s bootstrap-token issue?)"
printf '%s' "$data" > "$OUT"
ok "bootstrap kubeconfig ready -> $OUT  (apiserver $APISERVER)"
