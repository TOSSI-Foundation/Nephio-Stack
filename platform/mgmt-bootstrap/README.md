# Management-plane bootstrap (M1)

Stands up the **management cluster** with everything Nephio needs to provision **prod-grade
Canonical Kubernetes on remote servers** by intent. Run **once** per management plane — this is
ops IaC. After it exists, the product is 100% declarative (cluster + 5G intent YAML).

## What it installs
| Step | Target | What |
|---|---|---|
| `mgmt-up` | mgmt k8s (LAN-reachable API, proper cert SANs) | control plane for CAPI + Nephio |
| `byoh-image` | build BYOH controller + agent from source | upstream image DEAD — we build + mirror |
| `providers` | `clusterctl init` | CAPI core + **Canonical-K8s** (bootstrap+control-plane) + **BYOH** (infra) |
| `byoh-fix` | patch | repoint BYOH at our image + fix dead kube-rbac-proxy |
| `nephio` | Porch + specializers + Gitea | the intent/GitOps engine |

## Two axes you configure (`clusterctl-config.yaml`)
- **Distro** = bootstrap+control-plane provider → `canonical-kubernetes` (your preference). Swap for `kubeadm`.
- **Where machines come from** = infrastructure provider → `byoh` (existing hosts). Swap for `metal3`/`MAAS` (bare metal, zero-touch) or `capa/capz/capg` (cloud). **Only this line changes** to target different hardware.

## Run
```
make all        # or step-by-step: make mgmt-up byoh-image providers byoh-fix nephio
```

## Production notes
- Replace KIND mgmt (`kind-mgmt.yaml`) with a real HA cluster.
- Replace all upstream URLs/images with your **private mirror** (`../registry-mirror/`) — upstream rots
  (BYOH VMware registry, kube-rbac-proxy gcr, aether charts are all already dead).
- `install-nephio.sh` is a thin, pinned wrapper over the Nephio installer (TODO: harden / package).
