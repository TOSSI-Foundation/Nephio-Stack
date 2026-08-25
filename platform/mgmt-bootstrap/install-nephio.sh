#!/usr/bin/env bash
# Install the Nephio management plane onto the current (mgmt) cluster — PRODUCT-GRADE:
#
#   * PINNED    — exact package versions from versions.env, not "latest".
#   * VENDORED  — Nephio's core kpt packages are copied ONCE into ./nephio-packages/ (committed to
#                 this repo). After that the install has ZERO runtime dependency on upstream GitHub
#                 (which ships no install-YAMLs and whose assets rot). Air-gap capable.
#   * IDEMPOTENT— install == reconcile. `kpt live apply` prunes removed objects and is safe to re-run.
#
# This replaces the old "curl | kubectl apply <release-url>" approach (those URLs don't exist).
set -euo pipefail
: "${NEPHIO_CATALOG_REPO:?}" "${NEPHIO_BRANCH:?}"

HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib.sh"
VENDOR="$HERE/nephio-packages"          # committed, pinned copies live here
CAT="${NEPHIO_CATALOG_REPO%.git}.git"
mkdir -p "$VENDOR"

# pkg-path-in-catalog : target-namespace  (apply order = dependency order)
CORE_PKGS=(
  "core/porch:porch-system"             # Porch (package orchestration API + controllers)
  "core/workload-crds:default"          # Interface/DataNetwork/NFDeployment CRDs our blueprints use
  "core/nephio-operator:nephio-system"  # PackageVariant/PVS reconcilers + specializers
  "core/configsync:config-management-system"   # Config Sync — applies what Porch writes to deploy repos
  "optional/resource-backend:backend-system"  # IPAM/VLAN allocation backend (our UPF blueprints need it)
)

vendor_once() {                         # fetch a pinned copy into the repo iff we don't have one
  local sub="$1" name="${1##*/}"
  if [ ! -f "$VENDOR/$name/Kptfile" ]; then
    say "vendoring nephio/$sub @ $NEPHIO_BRANCH (one-time; commit ./nephio-packages/$name)"
    kpt pkg get "$CAT/nephio/$sub@$NEPHIO_BRANCH" "$VENDOR/$name"
  fi
}

reconcile() {                           # idempotent apply of a vendored package
  local name="$1" ns="$2"
  ( cd "$VENDOR/$name"
    kpt fn render . >/dev/null
    grep -q '^ *inventory:' Kptfile || kpt live init --namespace "$ns" >/dev/null 2>&1 || true
    kpt live apply . --reconcile-timeout=5m --output=table )
}

phase "Nephio management plane (Porch + controllers + CRDs + IPAM backend)"
say "catalog=$CAT@$NEPHIO_BRANCH, vendored + pinned"
for entry in "${CORE_PKGS[@]}"; do vendor_once "${entry%%:*}"; done

for entry in "${CORE_PKGS[@]}"; do
  sub="${entry%%:*}"; ns="${entry##*:}"; name="${sub##*/}"
  run "reconcile $name -> ns/$ns" reconcile "$name" "$ns"
  [ "$name" = "porch" ] && run "wait porch-server ready" kubectl -n porch-system rollout status deploy/porch-server --timeout=300s
done
ok "Nephio management plane ready"
say "next: 'make gitea' deploys the in-cluster git backend + registers the Porch repos (appliance model)"
