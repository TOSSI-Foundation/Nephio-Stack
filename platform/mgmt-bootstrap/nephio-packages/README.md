# Vendored Nephio core packages (pinned)

These are the upstream Nephio management components (`porch`, `nephio-operator`,
`workload-crds`, `resource-backend`), fetched ONCE from the catalog at the version
pinned in `../versions.env` and committed here.

Why vendored: the product must install with **no runtime dependency on upstream GitHub**
(Nephio ships no install-YAMLs, and upstream assets rot). `install-nephio.sh` reconciles
these with `kpt live apply` — idempotent, prunable, air-gap capable.

To bump versions: bump `NEPHIO_BRANCH` in versions.env, delete the package dir, re-run
`make nephio` (it re-vendors), then commit the refreshed dir.
