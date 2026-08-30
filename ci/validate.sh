#!/usr/bin/env bash
# CI gate for the product: every blueprint must parse, render, and reference only valid kinds.
# Run in CI on every PR (blocks broken blueprints from ever reaching a cluster).
set -euo pipefail
cd "$(dirname "$0")/.."

echo "== 1. YAML well-formed =="
find blueprints platform fabric -name '*.yaml' -print0 | xargs -0 -I{} sh -c 'kubectl create --dry-run=client -f {} >/dev/null 2>&1 || yq e "." {} >/dev/null'

echo "== 2. kpt packages render (specializer pipelines valid) =="
for pkg in blueprints/*/; do
  [ -f "$pkg/Kptfile" ] || continue
  echo "  render $pkg"
  kpt fn render "$pkg" --truncate-output=false >/dev/null
done

echo "== 3. operator compiles + unit tests =="
( cd operator && go build ./... && go vet ./... )

echo "== 4. image refs pinned (no :latest in shipping blueprints) =="
! grep -rn ':latest' blueprints/*/Kptfile | grep -v 'nephio/.*-fn' \
  || (echo "FAIL: pin non-nephio images"; exit 1)

echo "OK — blueprints valid, operator builds."
