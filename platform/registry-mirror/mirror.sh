#!/usr/bin/env bash
# Mirror every image in images.txt into your private registry. Run once (and on version bumps).
# Usage: DEST_REGISTRY=registry.example.com/5g ./mirror.sh
set -euo pipefail
: "${DEST_REGISTRY:?set DEST_REGISTRY, e.g. registry.example.com/5g}"

grep -vE '^\s*#|^\s*$' images.txt | while read -r src _; do
  # strip the source registry host, keep repo:tag under DEST_REGISTRY
  path="${src#*/}"
  dst="$DEST_REGISTRY/$path"
  echo ">> $src  ->  $dst"
  docker pull "$src"
  docker tag "$src" "$dst"
  docker push "$dst"
done
echo ">> done. Point versions.env / blueprint image refs at \$DEST_REGISTRY for air-gapped installs."
