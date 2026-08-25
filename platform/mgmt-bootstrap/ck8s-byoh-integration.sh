#!/usr/bin/env bash
# Make Canonical Kubernetes accept BYOH host self-registration. CK8s differs from kubeadm in three
# ways that each break the BYOH  bootstrap-token -> CSR -> ByoHost  flow. Fix all three (idempotent).
# Run after the BYOH provider is installed (clusterctl init). This is THE CK8s<->BYOH interop glue.
set -euo pipefail
source "$(cd "$(dirname "$0")" && pwd)/lib.sh"
API_ARGS=/var/snap/k8s/common/args/kube-apiserver
PKI=/etc/kubernetes/pki
RESTART_API=0
phase "CK8s <-> BYOH interop (bootstrap-token auth + client-CA trust + host RBAC)"

# 1. CK8s does NOT enable bootstrap-token auth by default -> BYOH's bootstrap token is rejected (401).
if ! sudo grep -q enable-bootstrap-token-auth "$API_ARGS"; then
  echo "--enable-bootstrap-token-auth=true" | sudo tee -a "$API_ARGS" >/dev/null
  RESTART_API=1; ok "enabled bootstrap-token auth"
fi

# 2. CK8s authenticates clients against a SEPARATE CA (kubernetes-ca-client) from the CSR-signing CA
#    (kubernetes-ca) -> the host cert minted via CSR isn't trusted (401). Trust the cluster CA for
#    client auth too (kubeadm uses a single CA for both; this makes CK8s behave the same for BYOH).
if ! sudo openssl crl2pkcs7 -nocrl -certfile "$PKI/client-ca.crt" 2>/dev/null \
     | openssl pkcs7 -print_certs -noout 2>/dev/null | grep -qE "CN = kubernetes-ca$"; then
  sudo cp "$PKI/client-ca.crt" "$PKI/client-ca.crt.orig" 2>/dev/null || true
  sudo bash -c "cat $PKI/ca.crt >> $PKI/client-ca.crt"
  RESTART_API=1; ok "trusted cluster CA in the apiserver client-auth bundle"
fi

if [ "$RESTART_API" = 1 ]; then
  run "restart kube-apiserver + wait healthz" bash -c \
    'sudo snap restart k8s.kube-apiserver; for _ in $(seq 1 15); do kubectl get --raw /healthz >/dev/null 2>&1 && exit 0; sleep 2; done; exit 1'
fi

# 3. The bootstrap token's group (system:bootstrappers:byoh) can create CSRs but the BYOH-provided
#    byohost-editor binding only grants group byoh:hosts -> add the bootstrap group so the agent can
#    create its own ByoHost during registration.
if ! kubectl get clusterrolebinding byoh-byohost-editor-clusterrole-binding \
       -o jsonpath='{.subjects[*].name}' | grep -q 'system:bootstrappers:byoh'; then
  kubectl patch clusterrolebinding byoh-byohost-editor-clusterrole-binding --type=json \
    -p='[{"op":"add","path":"/subjects/-","value":{"apiGroup":"rbac.authorization.k8s.io","kind":"Group","name":"system:bootstrappers:byoh"}}]'
  ok "granted host self-registration RBAC"
fi

ok "CK8s <-> BYOH interop ready"
