#!/usr/bin/env bash
# Install the pinned CLI toolchain that `make mgmt` / `make publish` need on a FRESH host.
# Idempotent: a tool already at the pinned version is left alone. Versions come from versions.env
# (single source of truth). This is the prerequisite step so any bare server is reproducible —
# NOT something to hand-copy per box.
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck disable=SC1091
source versions.env
: "${GO_VERSION:=1.23.5}"
: "${KUSTOMIZE_VERSION:=v5.4.3}"
: "${CLUSTERCTL_VERSION:=v1.14.0}"
: "${KPT_VERSION:=v1.0.0-beta.57}"
: "${PORCH_VERSION:=v1.5.7}"

ARCH=amd64
BIN=/usr/local/bin
export PATH="/usr/local/go/bin:$BIN:$PATH"
have() { command -v "$1" >/dev/null 2>&1; }

echo "▸ prereqs: pinned CLI toolchain (go ${GO_VERSION}, kustomize ${KUSTOMIZE_VERSION}, clusterctl ${CLUSTERCTL_VERSION}, kpt ${KPT_VERSION}, porchctl ${PORCH_VERSION})"

# --- host tools we don't manage: fail early with a clear hint ---
for t in curl tar; do have "$t" || { echo "  ✗ '$t' missing — apt-get install -y $t"; exit 1; }; done
have docker || { echo "  ✗ docker missing — needed to build/import images (https://docs.docker.com/engine/install/)"; exit 1; }
have snap   || { echo "  ✗ snapd missing — needed for Canonical Kubernetes (apt-get install -y snapd)"; exit 1; }
if ! have helm; then
  echo "  installing helm"
  curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | sudo bash >/dev/null
fi

# --- go (tarball) ---
if ! go version 2>/dev/null | grep -q "go${GO_VERSION} "; then
  echo "  installing go ${GO_VERSION}"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -o /tmp/go.tgz
  sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz
fi

# --- clusterctl (release binary) ---
if ! clusterctl version 2>/dev/null | grep -q "$CLUSTERCTL_VERSION"; then
  echo "  installing clusterctl ${CLUSTERCTL_VERSION}"
  sudo curl -fsSL "https://github.com/kubernetes-sigs/cluster-api/releases/download/${CLUSTERCTL_VERSION}/clusterctl-linux-${ARCH}" -o "$BIN/clusterctl"
  sudo chmod +x "$BIN/clusterctl"
fi

# --- kpt (release binary) ---
if ! kpt version 2>/dev/null | grep -q "${KPT_VERSION#v}"; then
  echo "  installing kpt ${KPT_VERSION}"
  sudo curl -fsSL "https://github.com/kptdev/kpt/releases/download/${KPT_VERSION}/kpt_linux_${ARCH}" -o "$BIN/kpt"
  sudo chmod +x "$BIN/kpt"
fi

# --- kustomize + porchctl (go install, pinned — built with the go we just installed) ---
GOFLAGS_ENV=(env "PATH=/usr/local/go/bin:$PATH" "GOBIN=$BIN" "GOCACHE=/tmp/gocache" "GOPATH=/tmp/gopath")
if ! kustomize version 2>/dev/null | grep -q "$KUSTOMIZE_VERSION"; then
  echo "  installing kustomize ${KUSTOMIZE_VERSION}"
  sudo "${GOFLAGS_ENV[@]}" /usr/local/go/bin/go install "sigs.k8s.io/kustomize/kustomize/v5@${KUSTOMIZE_VERSION}"
fi
if ! porchctl version 2>/dev/null | grep -q "${PORCH_VERSION}"; then
  echo "  installing porchctl ${PORCH_VERSION}"
  sudo "${GOFLAGS_ENV[@]}" /usr/local/go/bin/go install "github.com/nephio-project/porch/cmd/porchctl@${PORCH_VERSION}"
fi

echo "  ✓ prereqs ready:"
for t in go kustomize porchctl clusterctl kpt helm docker; do
  printf "    %-11s %s\n" "$t" "$($t version 2>&1 | head -1 | cut -c1-48 || echo MISSING)"
done
