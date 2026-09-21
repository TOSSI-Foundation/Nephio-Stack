# Nephio-Orchestrated Cloud-Native Private 5G with SD-Core, Edge UPF and OCUDU-RAN Across Split 7.2x and Split 8

This repository brings together deployment stacks for building and validating cloud-native private 5G networks. Each stack is documented as a self-contained implementation so additional orchestration, core, RAN and edge combinations can be added without mixing their architecture, deployment workflow or validation status.

---


### SD-Core + OCUDU-RAN + Edge UPF

An intent-driven, GitOps-native deployment of a complete private 5G network comprising the **SD-Core** control plane, **BESS Edge UPF** and **OCUDU-RAN** gNB on bare-metal edge infrastructure. The stack is orchestrated through [Nephio](https://nephio.org/) (Porch and Config Sync) and [Cluster API](https://cluster-api.sigs.k8s.io/) from a single Kubernetes intent object.

A management cluster runs Nephio and a purpose-built operator. You declare an `EdgeSite` (or a `Fleet`) intent; the operator expands it into Nephio `PackageVariants`, Porch renders them, Config Sync delivers them, and Cluster API provisions a fresh Kubernetes cluster on each target server. Addressing, the network slice, subscriber records and the N2/N3/N4/N6 datapath are **derived from the intent** — not hand-configured. Adding, changing or removing a 5G edge is a change to one YAML object.

- **Bootstrap is imperative** (you cannot install Nephio with Nephio) — `make mgmt`, `make publish`.
- **Everything per-site is declarative** — one `EdgeSite`/`Fleet` intent; the operator does the rest.

### Implementation roadmap

| Phase | Stack configuration | Radio integration | Validation scope | Status |
| --- | --- | --- | --- | --- |
| 1 | SD-Core + OCUDU-RAN + Edge UPF | RF simulator | 50 simulated UEs | **Done** |
| 2 | SD-Core + OCUDU-RAN + Edge UPF | 3GPP Split 8 | End-to-end integration and UE validation | **Done** |
| 3 | SD-Core + OCUDU-RAN + Edge UPF | O-RAN Split 7.2 | Open Fronthaul integration and UE validation | **Done** |

> **Current validated baseline:** all three phases are integrated end to end with SD-Core and the Edge UPF. Phase 1 with the RF simulator (50 simulated UEs); Phase 2 with a **3GPP Split-8** gNB driving a USRP; and Phase 3 with an **O-RAN Split-7.2** gNB reaching an O-RU over Open Fronthaul — Phases 2 and 3 each validated with a real UE.

---

### Architecture

<img width="1920" height="1080" alt="43" src="https://github.com/user-attachments/assets/50cc9b8a-9162-4fcb-b624-bdda881211ca" />

 
The platform has two tiers:

- **Management cluster** — runs Nephio (Porch, in-cluster Gitea, Config Sync source), the IPAM pool, the Cluster API stack (BYOH provider + Canonical Kubernetes bootstrap provider), and the custom operator. It *produces and places* configuration; it does not run any network function itself.
- **Edge clusters** — one per bare-metal server, provisioned on demand by Cluster API. Each runs the actual 5G stack: SD-Core control plane, BESS UPF, optional OCUDU gNB, Multus and the datapath actuator.

---

### How it works

A single `kubectl apply` (or `make edge`) becomes a running 5G edge through a continuous reconcile loop:

1. **Declare** — apply an `EdgeSite`, or edit a `Fleet`, on the management cluster.
2. **Reconcile intent** — the operator claims addressing from **IPAM** (UE pool, N3/N4/N6 subnets) and computes the site's derived values (AMF address, PLMN, TAC, slice).
3. **Generate packages** — it emits one **`PackageVariant`** per network function; **Porch** clones the matching blueprint and renders it with those values.
4. **Publish** — an **auto-approver** promotes each rendered revision Draft → Published; packages land in **Gitea**.
5. **Provision** — in parallel, **Cluster API + BYOH** bootstraps **Canonical Kubernetes** (Cilium CNI, SCTP for N2) on the target server.
6. **Deliver** — the new cluster's **Config Sync** pulls its packages and applies the SD-Core control plane, BESS UPF, OCUDU gNB and Multus.
7. **Wire the datapath** — the **datapath actuator** (a DaemonSet) programs the host routes, N6 egress NAT and downlink next-hop MAC so the N3/N6 user plane forwards.
8. **Serve** — the gNB drives its radio; a UE attaches to SD-Core, establishes a PDU session, and traffic egresses via N6.

Because every step is a reconciler, the loop is **self-healing**, and **teardown is symmetric**: deleting the intent garbage-collects the packages, Config Sync prunes the workloads, and Cluster API deprovisions the cluster — no per-host scripts.

---

### Features

- **One object per site** — a complete edge (core + user plane + radio) from a single `EdgeSite`, or a whole estate from one `Fleet`.
- **Automatic specialisation** — UE pool, N3/N4/N6 subnets, AMF address, PLMN, TAC and slice derived from intent + IPAM (no collisions, no hand-assignment).
- **Declarative cluster provisioning** — bare servers become Kubernetes clusters via CAPI/BYOH inside the same reconcile loop.
- **GitOps delivery** — every site's config is version-controlled in Git and continuously reconciled by Config Sync.
- **Two topologies from one API** — self-contained `cp+upf` cores and `upf-only` user-plane edges (steered over Cilium ClusterMesh).
- **Real radios** — OCUDU gNB with USRP `n310` / `b210` / `x310`, or the `zmq` RF simulator for hardware-free runs.
- **Datapath automation to the wire** — the host N3/N6 path is programmed by a controller, not by SSH.
- **Pinned & mirror-able** — all versions pinned in `versions.env`; images mirror-able into a private registry for air-gapped/rot-proof deploys.

---

### Repository layout

```
.
├── operator/              # Custom Go operator (kubebuilder)
│   ├── api/               #   EdgeSite / Fleet / MeshLink / UPFDataPath CRD types
│   ├── internal/controller/ #  reconcilers (edgesite, fleet, meshlink, upfdatapath, network)
│   ├── cmd/  config/  test/
├── blueprints/            # kpt blueprint packages (specialised per site by Porch)
│   ├── sdcore-cp/         #   SD-Core control plane (AMF/SMF/NRF/AUSF/NSSF/PCF/UDM/UDR/WebUI/Mongo)
│   ├── sdcore-upf/        #   BESS UPF (af_packet)
│   ├── multus/            #   Multus + NetworkAttachmentDefinitions (access/core/radio)
│   ├── datapath-actuator/ #   host N2/N3/N4/N6 datapath DaemonSet
│   ├── n2-expose/         #   N2 NGAP LoadBalancer VIP
│   ├── edge-site/         #   per-site umbrella package
│   ├── cluster-ck8s-byoh/ #   CAPI cluster + ByoHost definitions
│   ├── workload-cluster-ck8s/
│   ├── sdcore-operator/   #   operator install package
│   └── intents/           #   sample intents
├── platform/
│   ├── mgmt-bootstrap/    # imperative bootstrap (make mgmt): CK8s+CAPI+BYOH+Nephio+Gitea
│   ├── controllers/       # auto-approver (Draft→Published) and helpers
│   └── registry-mirror/   # mirror images into a private registry (air-gap / anti-rot)
├── intent/                # example intent objects (e.g. mgmtcore.yaml)
├── fabric/                # network fabric configuration
├── images/ocudu-gnb/      # OCUDU gNB container image build
├── fleet.yaml             # the Fleet intent (edit → make sites)
├── versions.env           # pinned versions (single source of truth)
├── install-prereqs.sh     # CLI toolchain installer (make prereqs)
├── Makefile               # all workflow targets
└── LICENSE

```

---

### Prerequisites

- A **management server** + one or more **edge servers** (Ubuntu 22.04 / 24.04), reachable over a LAN, with `sudo` on each.
- **SSH key auth** from the management server to each edge host.
- Optional radio: USRP `n310` / `b210` / `x310`. No hardware? use `device: zmq` (RF simulator).
- Outbound access to the container registries (or a mirror — see [Configuration](https://github.com/Aditya-gairola/Nephio-Stack#configuration)).

```
make prereqs   # installs the pinned toolchain (go, kustomize, clusterctl, kpt, porchctl). make mgmt runs it too.
```

Pinned versions live in [`versions.env`](https://github.com/Aditya-gairola/Nephio-Stack/blob/main/versions.env): Go 1.23.5 · Cluster API v1.9.5 · cert-manager v1.16.2 · Canonical K8s bootstrap provider v0.6.2 · BYOH v0.5.0 · CK8s channel `1.32-classic/stable`.

---

### Quick start

```
# 1. Bring up the management plane (once)
make mgmt        # Canonical K8s + CAPI + BYOH (built from source) + Nephio (Porch) + in-cluster Gitea
make publish     # build+load the operator image, push blueprints to Gitea, deploy operator + auto-approver + IPAM

# 2. (If any site uses a radio) build the OCUDU gNB image once
make gnb-image OCUDU_SRC=<path-to-ocudu-clone>

# 3. Deploy a site
make edge HOST=192.168.1.111 NAME=edge-01 ROLE=cp+upf SSH_USER=ubuntu

# …or the whole fleet at once
make up          # = make mgmt → make publish → make mgmt-core → make sites
```

Everything after `make publish` is `kubectl apply` of intent.

---

### Deployment scenarios

#### UPF-only edge (steered by the management control plane)

The UPF runs on the edge; N2 (to the mgmt AMF) and N4 are carried over Cilium ClusterMesh by the operator.

```
make edge HOST=<edge-ip> NAME=<name> ROLE=upf-only SSH_USER=<login>
```

Equivalent intent:

```
apiVersion: sdcore.nephio.io/v1alpha1
kind: EdgeSite
metadata: { name: <name>, namespace: default }
spec:
  server: <edge-ip>
  role: upf-only          # controlPlane defaults to "mgmt"
```

#### Self-contained core (CP + UPF, no radio)

A complete SD-Core control plane + UPF on the edge server. N2/N4 are local.

```
make edge HOST=<edge-ip> NAME=<name> ROLE=cp+upf SSH_USER=<login>
```

#### Full CP + UPF + OCUDU gNB (core + radio)

`make edge` does not add a `ran:` block, so use the **Fleet** path (or a hand-written `EdgeSite` that includes `ran:`). Build the gNB image first (`make gnb-image`). Add the site to [`fleet.yaml`](https://github.com/Aditya-gairola/Nephio-Stack/blob/main/fleet.yaml):

```
apiVersion: sdcore.nephio.io/v1alpha1
kind: Fleet
metadata: { name: fleet, namespace: default }
spec:
  sites:
    - server: <edge-ip>
      user: <login>
      role: cp+upf
      ran:
        split: "8"               # "8" = SDR driven directly | "7.2" = Open Fronthaul to an O-RU
        device: n310             # n310 | b210 | x310 | zmq
        radioNic: <sdr-data-nic> # host NIC on the radio's data L2 (networked SDRs, e.g. N310)
        radioAddr: <sdr-ip>      # the SDR's own address
```

```
make sites            # registers host(s) + reconciles the whole fleet declaratively
```

**Split-7.2 (Open Fronthaul to an O-RU).** The example above is Split-8 (an SDR driven directly). For an O-RU over eCPRI, set `split: "7.2"` and add an `ofh:` block. The operator does not own the fronthaul physics — the host needs the fronthaul VF bound to `vfio-pci`, 1G huge pages, **PTP** (`ptp4l`+`phc2sys`) locked to the RU clock, and the VF on the O-RU's fronthaul VLAN:

```
# host fronthaul prep (values below are placeholders — substitute your own):
sudo dpdk-devbind.py --bind=vfio-pci <fronthaul-vf-pci>              # bind the fronthaul VF to vfio-pci
sudo ip link set <fronthaul-nic> vf <vf-index> vlan <fronthaul-vlan> # put the VF on the O-RU's fronthaul VLAN
sudo ptp4l -i <fronthaul-nic> -f <ptp.cfg> ; sudo phc2sys ...        # lock PTP to the RU clock
```

```
      # the site's ran: block, for Split-7.2
      ran:
        split: "7.2"
        device: <o-ru-model>
        cell: { band: <band>, dlArfcn: <arfcn>, bandwidthMHz: <bw>, commonScs: <scs>, pci: <pci> }
        ofh:
          interface: "<fronthaul-vf-pci>"   # PCI address of the DPDK fronthaul VF (vfio-pci)
          ruMac:  "<o-ru-mac>"              # the O-RU's MAC
          duMac:  "<du-vf-mac>"             # the DU (fronthaul VF) MAC
          vlanTag: <fronthaul-vlan>         # the O-RU's fronthaul VLAN (must match the host VF)
```

#### Colocated core on the management server

Run a `cp+upf` core (and optionally a gNB) on the management VM itself — see [`intent/mgmtcore.yaml`](https://github.com/Aditya-gairola/Nephio-Stack/blob/main/intent/mgmtcore.yaml):

```
make mgmt-core
```

#### The whole fleet, one command

```
make up                       # mgmt → publish → mgmt-core → sites
make sites FLEET=fleet.yaml   # after bootstrap, just reconcile the fleet
```

Add / change / remove a site = edit one line in `fleet.yaml` → `make sites`. Removing an entry garbage-collects its `EdgeSite` and tears the site down.

---

### Intent reference

#### `EdgeSite` (`sdcore.nephio.io/v1alpha1`)

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `server`                         | string | ✔ | The edge host (must be a registered ByoHost).                          |
| `role`                           | string | ✔ | `upf-only` (UPF steered by mgmt CP) or `cp+upf` (self-contained core). |
| `user`                           | string |   | SSH login for the host (defaults per fleet / `ubuntu`).                |
| `controlPlane`                   | string |   | Which CP an `upf-only` site attaches to. Default `mgmt`.               |
| `colocated`                      | bool   |   | `true` only for the mgmt VM itself (colocated core).                   |
| `sliceSD`                        | string |   | Override the slice SD (else auto-derived).                             |
| `ranSubnet`                      | string |   | Override the N3/RAN subnet (else from IPAM).                           |
| `ranNic`                         | string |   | Host NIC used for the N3/RAN network.                                  |
| `ran`                            | object |   | Adds an OCUDU gNB (see below). `cp+upf` only.                          |

#### `ran` (RAN spec)

| Field | Type | Description |
| --- | --- | --- |
| `split`                  | string | `"8"` (SDR driven directly) or `"7.2"` (Open Fronthaul to an O-RU). |
| `device`                 | string | `zmq` (RF simulator) · `b210` · `x310` · `n310`.                    |
| `radioNic`               | string | Host NIC on the radio's data L2 (networked SDRs, e.g. N310).        |
| `radioAddr`              | string | The SDR's own IP address.                                           |
| `cell`                   | object | `band`, `dlArfcn`, `bandwidthMHz`, `commonScs`, `pci`.              |
| `ofh`                    | object | Split `"7.2"` only: `interface`, `ruMac`, `duMac`, `vlanTag`.       |

#### `Fleet` (`sdcore.nephio.io/v1alpha1`)

`spec.sites[]` — each an object of `{ server, user?, role?, ran? }`. Each entry expands into one owned `EdgeSite`; add/remove entries to scale.

---

### Provision subscribers (SIMs)

Subscribers are declarative CRs bound to a slice. Apply/edit and the operator POSTs them to the core:

```
kubectl apply -f intent/subscribers.yaml    # each: plmnID, opc, key, seq, sliceRef
kubectl get subscribers.sdcore.nephio.io -n default
```

---

### Verify a UE

With the gNB attached to the core, follow the attach end to end from the gNB and core logs:

```
kubectl logs -f deploy/<gnb>                     # NG Setup → UE Registration → PDU session establishment
kubectl get subscribers.sdcore.nephio.io -n default
```

Then, from the UE (a real phone on a `ran:` site), browse or run a ping / speed test — traffic egresses to the internet via N6.

---

### Teardown

Retracting intent is all it takes. Deleting an `EdgeSite`/`Fleet` garbage-collects its PackageVariants / MeshLink / NetworkSlice / IPClaims → Porch removes the packages → Config Sync prunes → **Cluster API deprovisions the cluster(s)**. No SSH, no per-host teardown script.

```
kubectl delete edgesite <name>     # one site  (make edge-delete NAME=<name> also waits for CAPI)
kubectl delete -f fleet.yaml       # whole fleet (make sites-delete also waits for CAPI)
```

**Delete only the edge sites — the management plane stays up.** `make sites-delete` deletes the Fleet so Cluster API deprovisions every edge cluster; `make edge-delete NAME=<name>` removes a single site. The mgmt plane keeps running:

```
make sites-delete            # all edge sites removed, management plane untouched
make edge-delete NAME=<name> # remove just one edge site
```

**Tear down everything, including the management plane.** `make down` runs `sites-delete` → `hosts-wipe` → `clean`, leaving every host pristine:

```
make down     # edge sites (CAPI-deprovisioned) + edge hosts wiped + the management plane removed
```

- `sites-delete` — delete the Fleet so Cluster API deprovisions the edge clusters.
- `hosts-wipe` — wipe each edge host (byoh-agent, CK8s snap, bridges, containerd, leftover datapath rules).
- `clean` — wipe the **management** host the same way and remove its Kubernetes snap.

---

### Make target reference

| Target | Purpose |
| --- | --- |
| `make prereqs`                           | Install the pinned CLI toolchain.                                                           |
| `make mgmt`                              | Bootstrap the mgmt plane (CK8s + CAPI + BYOH + Nephio + Gitea).                             |
| `make publish`                           | Build+load the operator image, push blueprints to Gitea, deploy operator + approver + IPAM. |
| `make gnb-image OCUDU_SRC=<p>`           | Build the OCUDU gNB image (needed for any `ran:` site).                                     |
| `make add-server HOST=<ip> SSH_USER=<u>` | Register a bare host as a ByoHost.                                                          |
| `make host-serve`                        | Serve a self-registration endpoint for many hosts.                                          |
| `make edge HOST=<ip> NAME=<n> ROLE=<r>`  | One site (`upf-only` / `cp+upf`), no RAN.                                                   |
| `make sites [FLEET=fleet.yaml]`          | Reconcile the whole fleet (RAN supported).                                                  |
| `make mgmt-core`                         | Colocate a `cp+upf` core (+ gNB) on the mgmt VM itself.                                     |
| `make up`                                | Whole-fabric bring-up (`mgmt → publish → mgmt-core → sites`).                               |
| `make down`                              | Whole-fabric teardown (`sites-delete → clean`).                                             |
| `make edge-delete NAME=<n>`              | Delete one EdgeSite + wait for CAPI to deprovision.                                         |
| `make sites-delete`                      | Delete **only** the edge sites (management plane stays up) + wait for CAPI to deprovision.  |
| `make reset`                             | Delete all EdgeSites/workloads, keep the mgmt platform.                                     |
| `make clean`                             | Remove the mgmt Kubernetes snap.                                                            |

---

### Configuration

All versions are pinned in [`versions.env`](https://github.com/Aditya-gairola/Nephio-Stack/blob/main/versions.env) — the single source of truth. Notable settings:

| Variable | Purpose |
| --- | --- |
| `MGMT_LAN_IP`                                  | Address remote edge hosts use to reach the mgmt API. Empty → auto-detected. |
| `CK8S_CHANNEL`                                 | Canonical Kubernetes snap channel (default `1.32-classic/stable`).          |
| `CAPI_VERSION`, `CK8S_VERSION`, `BYOH_VERSION` | Cluster API + provider versions.                                            |
| `WORKLOAD_K8S_VERSION`                         | Workload cluster Kubernetes version.                                        |

**Image pulls / air-gap.** Every server's containerd caches images and (with `imagePullPolicy: IfNotPresent`) never re-pulls. For slow or air-gapped sites, mirror all pinned images into a private registry once — see [`platform/registry-mirror/`](https://github.com/Aditya-gairola/Nephio-Stack/blob/main/platform/registry-mirror).

---

### Topologies

| Topology | Intent | Notes |
| --- | --- | --- |
| **Self-contained core** | `role: cp+upf`    | SD-Core CP + UPF (+ optional gNB) on one edge; N2/N4 local.           |
| **User-plane edge**     | `role: upf-only`  | UPF steered by the mgmt CP; N2/N4 over Cilium ClusterMesh (MeshLink). |
| **Colocated core**      | `colocated: true` | A `cp+upf` core on the management VM itself.                          |

---

### Design notes

- **You cannot install Nephio with Nephio.** The bootstrap layer (`make mgmt`, host onboarding via `register-host.sh`) is inherently imperative: a bare host has no KRM owner to act on it. Everything *above* the cluster boundary is declarative KRM.
- **Specialisation, not editing.** Blueprints are rendered per site by Porch; they are never hand-edited. Moving to a newer SD-Core / OCUDU release is a blueprint revision, not a re-derivation.
- **The datapath is the one thing below Kubernetes.** Host routes, N6 NAT and the downlink next-hop MAC can't be expressed as pod manifests, so the datapath actuator (a DaemonSet driven by a `UPFDataPath` CR) programs them — extending the automation to the wire.
- **Anti supply-chain-rot.** Some upstreams (BYOH's registry, others) are dead; the bootstrap builds them from source and everything is mirror-able into a private registry.

---

### Troubleshooting

| Symptom | Likely cause or corrective action |
| --- | --- |
| Edge node never becomes `Ready`         | CK8s is still pulling the CNI image on a slow link. `make sites` resumes safely; or seed a local mirror (`platform/registry-mirror/`).                                   |
| gNB up but no `radio0` / `n3` interface | Multus attach was lost during a pod crash-loop. Recreate the gNB pod so multus re-attaches.                                                                              |
| 5G attaches but no internet             | Downlink datapath: confirm the UPF can ARP the gNB N3 address and that the `UPFDataPath` route + next-hop MAC are programmed; check N6 NAT egresses the real uplink NIC. |
| gNB `DL task queue is full` / late TTI  | The gNB pod needs isolated/pinned CPUs (real-time). Pin it to `isolcpus` cores.                                                                                          |

---

### Contributing

Issues and pull requests are welcome. Please keep blueprints upstream-clean (specialise via Porch, do not fork network functions), pin new dependencies in `versions.env`, and run the operator build/tests before submitting.
