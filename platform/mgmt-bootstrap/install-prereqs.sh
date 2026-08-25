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

# Ensure this user owns their Go module cache. A prior root-run `go` (or leftover from other tooling) can
# leave root-owned dirs under ~/go, which then breaks the non-root `go build` in byoh-image ("permission
# denied" writing the module cache). Self-heal so the bootstrap is robust on a reused host.
mkdir -p "$HOME/go" "$HOME/.cache/go-build" 2>/dev/null || true
sudo chown -R "$(id -u):$(id -g)" "$HOME/go" "$HOME/.cache/go-build" 2>/dev/null || true

# --- host tools we don't manage: fail early with a clear hint ---
for t in curl tar; do have "$t" || { echo "  ✗ '$t' missing — apt-get install -y $t"; exit 1; }; done
have docker || { echo "  ✗ docker missing — needed to build/import images (https://docs.docker.com/engine/install/)"; exit 1; }
have snap   || { echo "  ✗ snapd missing — needed for Canonical Kubernetes (apt-get install -y snapd)"; exit 1; }
if ! have helm; then
  echo "  installing helm"
  curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | sudo bash >/dev/null
fi

# --- go (tarball) ---
if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go${GO_VERSION} "; then
  echo "  installing go ${GO_VERSION}"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -o /tmp/go.tgz
  sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz
fi
# Put go on the standard PATH (/usr/local/bin is on PATH; /usr/local/go/bin is NOT by default). Without
# this the byoh-image `go build` and other host go steps fail with "go: not found". Always (re)assert.
sudo ln -sf /usr/local/go/bin/go /usr/local/bin/go
sudo ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt

# --- clusterctl (release binary). Download to /tmp then install: `install` overwrites any stale
#     symlink at the destination, which `curl -o` cannot (it follows a dangling link and errors 23). ---
if ! clusterctl version 2>/dev/null | grep -q "$CLUSTERCTL_VERSION"; then
  echo "  installing clusterctl ${CLUSTERCTL_VERSION}"
  curl -fsSL "https://github.com/kubernetes-sigs/cluster-api/releases/download/${CLUSTERCTL_VERSION}/clusterctl-linux-${ARCH}" -o /tmp/clusterctl
  sudo install -m0755 /tmp/clusterctl "$BIN/clusterctl" && rm -f /tmp/clusterctl
fi

# --- kpt (release binary) ---
if ! kpt version 2>/dev/null | grep -q "${KPT_VERSION#v}"; then
  echo "  installing kpt ${KPT_VERSION}"
  curl -fsSL "https://github.com/kptdev/kpt/releases/download/${KPT_VERSION}/kpt_linux_${ARCH}" -o /tmp/kpt
  sudo install -m0755 /tmp/kpt "$BIN/kpt" && rm -f /tmp/kpt
fi

# --- kustomize (go install, pinned — built with the go we just installed) ---
if ! kustomize version 2>/dev/null | grep -q "$KUSTOMIZE_VERSION"; then
  echo "  installing kustomize ${KUSTOMIZE_VERSION}"
  sudo env "PATH=/usr/local/go/bin:$PATH" "GOBIN=$BIN" "GOCACHE=/tmp/gocache" "GOPATH=/tmp/gopath" \
    /usr/local/go/bin/go install "sigs.k8s.io/kustomize/kustomize/v5@${KUSTOMIZE_VERSION}"
fi

# --- porchctl (release tarball — porch's go.mod has replace directives, so `go install` is refused) ---
if ! porchctl version 2>/dev/null | grep -q "${PORCH_VERSION#v}"; then
  echo "  installing porchctl ${PORCH_VERSION}"
  curl -fsSL "https://github.com/kptdev/porch/releases/download/${PORCH_VERSION}/porchctl_${PORCH_VERSION#v}_linux_${ARCH}.tar.gz" -o /tmp/porchctl.tgz
  rm -rf /tmp/porchctl-x && mkdir -p /tmp/porchctl-x && tar -xzf /tmp/porchctl.tgz -C /tmp/porchctl-x
  sudo install -m0755 "$(find /tmp/porchctl-x -name porchctl -type f | head -1)" "$BIN/porchctl"
  rm -rf /tmp/porchctl.tgz /tmp/porchctl-x
fi

echo "  ✓ prereqs ready:"
for t in go kustomize porchctl clusterctl kpt helm docker; do
  printf "    %-11s %s\n" "$t" "$($t version 2>&1 | head -1 | cut -c1-48 || echo MISSING)"
done
