# Cross-site fabric (the datapath layer)

Nephio IPAM allocates the N3/N4/N6 IPs — but those IPs must be **reachable across clusters/sites**,
or the central SMF can't reach an edge UPF and the datapath is dead. That reachability is the
**fabric**, and it is the one thing Nephio does NOT build for you (it's infra, not orchestration).

## How the `vpc-*` fabrics span sites — pick one per deployment
| Environment | Fabric mechanism |
|---|---|
| **Production, recommended** | **Cilium ClusterMesh** — a multi-cluster CNI mesh. Pods/services in any cluster reach any other. Canonical K8s ships Cilium, so it's a config toggle (`cilium-values.yaml` + `cilium clustermesh connect`). |
| Production, existing L3 network | Routed underlay / SD-WAN — the `vpc-*` prefixes are advertised (BGP) across sites. |
| Sandbox / single-host | containerlab (what the Nephio sandbox uses) |
| PoC across 2 VMs | VXLAN overlay between the hosts carrying the `vpc-*` VLANs |

## Recommended: Cilium ClusterMesh
Every workload cluster runs Cilium with a unique `cluster-id` + `cluster-name` and a mesh API
server; the mgmt (or a hub) connects them. Then a UPF pod on edge-A and the SMF pod on regional
are mutually reachable by pod IP — the specialized N4 "just works" across sites.

```
   regional (SMF)  ─┐
   edge-1  (UPF)  ─┼─  Cilium ClusterMesh  ─  every pod reachable from every cluster
   edge-N  (UPF)  ─┘
```

## Install (per workload cluster) — baked into the workload-cluster blueprint
Add a `pv-cilium-clustermesh` PackageVariant (alongside pv-multus) that applies `cilium-values.yaml`
and registers the cluster into the mesh. Then the fabric scales automatically: a new edge joins
the mesh the moment it's provisioned. (TODO: wire the PackageVariant once ClusterMesh is validated.)
