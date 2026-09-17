#!/usr/bin/env bash
# Install the per-edge PLATFORM onto a freshly-CAPI-provisioned edge cluster, then let the EdgeSite
# intent deliver the UPF/multus/datapath the pure-Nephio way. This is BOOTSTRAP (same paradox as
# `make mgmt`: you can't Nephio-install Nephio's Config-Sync/operator onto a bare edge cluster), so it
# is imperative — but it is the ONLY imperative per-edge step; everything after is pure KRM (the UPF,
# multus, datapath, mesh come from the EdgeSite controller's PackageVariants via Config Sync).
#
#   Usage:  install-edge-platform.sh <edge-name> <edge-server-ip>
#   Proven live on cluster-edge45 / 192.168.4.45 (edge UPF 5/5 Running via Config Sync).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib.sh"
source "$HERE/versions.env"
EDGE="${1:?usage: install-edge-platform.sh <edge-name> <edge-ip> [role]}"
EDGEIP="${2:?usage: install-edge-platform.sh <edge-name> <edge-ip> [role]}"
ROLE="${3:-upf-only}"   # upf-only (mesh back to mgmt CP) | cp+upf (self-contained core, no mesh)
CLUSTER="cluster-$EDGE"
MGMT_IP="$(kubectl get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')"
GITEA_INT="gitea-http.gitea.svc.cluster.local:3000"
KC=/tmp/$CLUSTER.kubeconfig
EK() { KUBECONFIG=$KC kubectl "$@"; }

phase "Edge platform [1/6] — wait for '$CLUSTER' node Ready + grab kubeconfig"
# The reliable "cluster is up" signal is the edge NODE going Ready via its own apiserver — the CK8s
# provider doesn't reliably set cluster.status.controlPlaneReady. Re-grab the kubeconfig each loop (it may
# be regenerated) and TOLERATE "connection refused" while the apiserver is still bootstrapping / while a
# stale kubeconfig secret from a prior attempt points at an apiserver not yet serving. ~10 min budget.
say "provisioning the edge cluster on the registered host (CK8s bootstrap ~2-6 min)…"
READY=0; APISERVER_UP=0
# 20-min hard safety cap. The expected path is a few minutes; this cap only bites when something is
# genuinely slow. We distinguish two very different situations so a slow image pull is NEVER mistaken
# for a broken deploy: (a) the edge apiserver never answers -> the host was not provisioned (fail fast,
# ~6 min); (b) apiserver up but node not Ready -> CK8s bootstrapped and we are only waiting on the
# cilium CNI image pull -> keep waiting with visible progress up to the cap.
for i in $(seq 1 240); do
  kubectl get secret "$CLUSTER-kubeconfig" -n default -o jsonpath='{.data.value}' 2>/dev/null | base64 -d > "$KC" 2>/dev/null || true
  if EK get nodes 2>/dev/null | grep -q ' Ready '; then READY=1; break; fi
  if EK get nodes >/dev/null 2>&1; then
    [ "$APISERVER_UP" = 0 ] && { APISERVER_UP=1; ok "edge apiserver up — CK8s bootstrapped; waiting on the cilium CNI image"; }
    [ $((i % 6)) -eq 0 ] && say "cilium image pulling… pod state: $(EK -n kube-system get pods -l k8s-app=cilium --no-headers 2>/dev/null | awk '{print $3}' | sort | uniq -c | tr -s ' \n' ' ' | sed 's/^ //')"
  elif [ "$i" -ge 72 ] && [ "$APISERVER_UP" = 0 ]; then
    die "$CLUSTER apiserver never came up in ~6m — the host was not provisioned. Check: kubectl get byohost,cluster,machine (an unbound ByoHost means CAPI has no host to provision onto)."
  fi
  sleep 5
done
[ "$READY" = 1 ] || die "$CLUSTER node still not Ready after ~20m: the edge apiserver is up but the cilium CNI image never finished pulling — this host's internet to ghcr.io is too slow. Re-run 'make sites' (it resumes safely from here), or serve the images from a local mirror once (platform/registry-mirror/)."
EK get nodes 2>/dev/null || true
ok "$CLUSTER control plane Ready"

# SCTP on the edge CNI is required by EVERY role: N2/NGAP is SCTP, and CK8s's Cilium ships enable-sctp=false
# (it silently DROPS SCTP), so a gNB's association hangs. Set it via the ck-network HELM release so it
# survives CK8s's helm reconciler (a raw cilium-config patch is reverted). This is the mesh block's job for
# upf-only, but a self-contained cp+upf core ALSO terminates N2 locally — so enable sctp for it here too.
phase "Edge platform [1a/6] — enable SCTP on the edge CNI (N2/NGAP)"
run "cilium enable-sctp on $EDGE" \
  ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 "${EDGE_USER:-ubuntu}@$EDGEIP" \
  "sudo bash -c 'C=\$(ls -d /snap/k8s/current/k8s/manifests/charts/cilium* | head -1); k8s config > /tmp/adm.conf 2>/dev/null; /snap/k8s/current/bin/helm --kubeconfig /tmp/adm.conf -n kube-system upgrade ck-network \$C --reuse-values --set sctp.enabled=true && k8s kubectl -n kube-system rollout status ds/cilium --timeout=150s'" \
  || warn "sctp-enable on $EDGE didn't complete — re-run 'make edge' to retry"

# A SELF-CONTAINED core (cp+upf) has its OWN AMF/SMF on the same cluster as its UPF — N2/N4 are LOCAL,
# there is nothing to mesh. Skip the ClusterMesh bring-up for it; it applies only to upf-only sites whose
# CP lives on the mgmt cluster. (SCTP above is already enabled for both roles.)
if [ "$ROLE" = "upf-only" ]; then

# ClusterMesh mesh-ENABLE (BOOTSTRAP — the accepted imperative exception). CK8s exposes NO declarative
# cilium hook for cluster.id / clustermesh, so per the thesis audit this is a helm value set at CNI level,
# exactly like sctp on the mgmt — there is genuinely no KRM object for it. Gives the edge a DISTINCT
# cluster.id (derived from its name) + its own clustermesh-apiserver so it is BORN meshable. The mesh
# CONNECT (shared CA + cilium-clustermesh Secrets) is emitted THESIS-PURE by the MeshLink controller via
# Git -> Config Sync; this step only makes the edge meshable. Best-effort: re-run make edge to retry.
phase "Edge platform [1b/6] — ClusterMesh enable + connect-to-mgmt (born meshed)"
# ENABLE (distinct cluster.id/name + own clustermesh-apiserver) AND CONNECT (clustermesh.config peer =
# the mgmt) in ONE helm pass. The connect MUST live at this helm layer too: the chart renders the
# cilium-clustermesh/kvstoremesh secrets AND the <peer>.mesh.cilium.io hostAliases (the apiserver certs
# only carry *.mesh.cilium.io SANs, so direct-IP endpoints fail TLS) — and CK8s's helm reconciler REVERTS
# any non-chart mutation of chart-owned objects, so a controller-written secret/patch cannot survive here.
# Cilium hot-reloads clustermesh config, so peers apply without restarts. The shared CA (delivered
# thesis-pure by the MeshLink controller via Config Sync BEFORE the apiserver certs are minted) makes
# every cluster's certs chain to one root — that is what lets the peers trust each other.
CID=$(( $(printf '%s' "$EDGE" | cksum | cut -d' ' -f1) % 200 + 2 ))
run "cilium clustermesh enable+connect on $EDGE (cluster.id=$CID, peer=mgmt@$MGMT_IP)" \
  ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 "${EDGE_USER:-ubuntu}@$EDGEIP" \
  "sudo bash -c 'C=\$(ls -d /snap/k8s/current/k8s/manifests/charts/cilium* | head -1); k8s config > /tmp/adm.conf 2>/dev/null; /snap/k8s/current/bin/helm --kubeconfig /tmp/adm.conf -n kube-system upgrade ck-network \$C --reuse-values --set sctp.enabled=true --set cluster.id=$CID --set cluster.name=$EDGE --set clustermesh.useAPIServer=true --set clustermesh.config.enabled=true --set clustermesh.config.clusters[0].name=mgmt --set clustermesh.config.clusters[0].port=32379 --set clustermesh.config.clusters[0].ips[0]=$MGMT_IP'" \
  || warn "clustermesh enable+connect on $EDGE didn't complete — re-run 'make edge' to retry"
# inter-cluster SNAT breaks SCTP and drops ~50% of cross-cluster traffic; the key is NOT chart-templated,
# so this patch persists across CK8s reconciles (proven). Roll the edge cilium only if the value changed.
if ! ssh -o ConnectTimeout=15 "${EDGE_USER:-ubuntu}@$EDGEIP" "sudo k8s kubectl -n kube-system get cm cilium-config -o jsonpath='{.data.enable-inter-cluster-snat}'" 2>/dev/null | grep -q false; then
  run "disable inter-cluster SNAT on $EDGE" \
    ssh -o ConnectTimeout=15 "${EDGE_USER:-ubuntu}@$EDGEIP" \
    "sudo bash -c 'k8s kubectl -n kube-system patch cm cilium-config --type=merge -p \"{\\\"data\\\":{\\\"enable-inter-cluster-snat\\\":\\\"false\\\"}}\" && k8s kubectl -n kube-system rollout restart ds/cilium'" \
    || warn "SNAT-off on $EDGE didn't complete — re-run 'make edge' to retry"
fi

phase "Edge platform [1c/6] — mgmt learns this edge as a mesh peer"
# The mgmt's peer LIST must cover every edge; helm lists replace (not merge), so rebuild the full list
# from the EdgeSite intent each time — the intent IS the topology source of truth (thesis).
PEERS="--set clustermesh.config.enabled=true"
i=0
while read -r name server; do
  [ -n "$name" ] || continue
  PEERS="$PEERS --set clustermesh.config.clusters[$i].name=$name --set clustermesh.config.clusters[$i].port=32379 --set clustermesh.config.clusters[$i].ips[0]=$server"
  i=$((i+1))
done < <(kubectl get edgesites -o jsonpath='{range .items[?(@.spec.role=="upf-only")]}{.metadata.name} {.spec.server}{"\n"}{end}' 2>/dev/null)
run "mgmt clustermesh peers <- $i edge(s) from EdgeSite intent" \
  sudo bash -c "C=\$(ls -d /snap/k8s/current/k8s/manifests/charts/cilium* | head -1); /snap/k8s/current/bin/helm --kubeconfig /etc/kubernetes/admin.conf -n kube-system upgrade ck-network \$C --reuse-values $PEERS" \
  || warn "mgmt peer update didn't complete — re-run 'make edge' to retry"
if ! kubectl -n kube-system get cm cilium-config -o jsonpath='{.data.enable-inter-cluster-snat}' 2>/dev/null | grep -q false; then
  run "disable inter-cluster SNAT on mgmt" \
    sudo bash -c "kubectl --kubeconfig /etc/kubernetes/admin.conf -n kube-system patch cm cilium-config --type=merge -p '{\"data\":{\"enable-inter-cluster-snat\":\"false\"}}' && kubectl --kubeconfig /etc/kubernetes/admin.conf -n kube-system rollout restart ds/cilium" \
    || warn "SNAT-off on mgmt didn't complete — re-run 'make edge' to retry"
fi

else
  say "role=$ROLE is a self-contained core (local N2/N4) — skipping ClusterMesh bring-up"
fi

phase "Edge platform [2/6] — Config Sync on the edge cluster"
CAT="${NEPHIO_CATALOG_REPO%.git}.git"
rm -rf /tmp/cs-pkg; kpt pkg get "$CAT/nephio/core/configsync@$NEPHIO_BRANCH" /tmp/cs-pkg >/dev/null 2>&1
( cd /tmp/cs-pkg && kpt fn render . >/dev/null 2>&1 || true )
EK apply -f /tmp/cs-pkg/ --recursive >/dev/null 2>&1 || true   # first pass installs the operator + CRDs
# The ConfigManagement CR races its own CRD: an immediate second apply can fail "no matches for kind"
# and — if swallowed — leaves the edge with NO reconciler at all, so Config Sync never deploys anything
# while every step "succeeds" (bit us live on edge63). Re-apply until the CR EXISTS, then GATE on the
# reconciler-manager actually coming up; die loudly instead of shipping a half-installed edge.
for i in $(seq 1 30); do
  EK apply -f /tmp/cs-pkg/ --recursive >/dev/null 2>&1 || true
  EK get configmanagement config-management >/dev/null 2>&1 && break
  sleep 5
done
EK get configmanagement config-management >/dev/null 2>&1 || die "ConfigManagement CR never applied on $CLUSTER — Config Sync would silently deploy NOTHING"
for i in $(seq 1 40); do
  EK -n config-management-system get deploy reconciler-manager -o jsonpath='{.status.availableReplicas}' 2>/dev/null | grep -q 1 && break
  sleep 5
done
EK -n config-management-system get deploy reconciler-manager -o jsonpath='{.status.availableReplicas}' 2>/dev/null | grep -q 1 \
  || die "Config Sync reconciler-manager not Available on $CLUSTER"
ok "Config Sync operator + reconciler-manager up"

phase "Edge platform [3/6] — CRDs the edge packages need"
for crd in nfdeployments.workload.nephio.org interfaces.req.nephio.org datanetworks.req.nephio.org \
           network-attachment-definitions.k8s.cni.cncf.io packagevariants.config.porch.kpt.dev; do
  kubectl get crd "$crd" -o yaml 2>/dev/null | EK apply -f - >/dev/null 2>&1 || true
done
EK apply -f "$HERE/../../operator/config/crd/bases/" >/dev/null 2>&1 || true

phase "Edge platform [4/6] — sdcore-operator (image + deploy)"
sudo docker save "$SDCORE_OPERATOR_IMAGE" -o /tmp/op.tar; sudo chmod 644 /tmp/op.tar
scp -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 /tmp/op.tar "${EDGE_USER:-ubuntu}@$EDGEIP":/tmp/op.tar >/dev/null
ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 "${EDGE_USER:-ubuntu}@$EDGEIP" \
  "sudo /snap/k8s/current/bin/ctr -a /run/containerd/containerd.sock -n k8s.io images import /tmp/op.tar >/dev/null 2>&1"
( cd "$HERE/../../operator" && EK apply -k config/default >/dev/null 2>&1 || true )
EK -n sdcore-operator-system set image deploy/sdcore-operator-controller-manager manager="$SDCORE_OPERATOR_IMAGE" >/dev/null 2>&1
EK -n sdcore-operator-system patch deploy sdcore-operator-controller-manager --type=json \
  -p '[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' >/dev/null 2>&1
EK -n sdcore-operator-system rollout status deploy/sdcore-operator-controller-manager --timeout=120s >/dev/null 2>&1 || true
# The node-agent DaemonSet (sdcore-datapath) is delivered by Config Sync and runs THIS same operator image
# as the datapath actuator. On a re-run it may already be running an older layer; force it to adopt the
# freshly-imported image so its self-heal logic is current. First deploy: it doesn't exist yet (Config Sync
# brings it up on the pinned tag, which the edge containerd now has) — hence best-effort.
EK -n kube-system rollout restart ds/sdcore-datapath >/dev/null 2>&1 || true

# OCUDU gNB image(s) — a cp+upf site's `ran` block makes the controller deliver a gNB Deployment whose image
# is ocudu/gnb:zmq (simulator) or ocudu/gnb:sdr (real radio, UHD+DPDK+MKL). Neither has a public registry
# (built from source by `make gnb-image`), so seed whichever are present into the edge's containerd the same
# save->scp->ctr-import way as the operator image, else the gNB pod ImagePullBackOffs.
if [ "$ROLE" = "cp+upf" ]; then
  for IMG in "${RAN_GNB_IMAGE:-ocudu/gnb:zmq}" "${RAN_GNB_UHD_IMAGE:-ocudu/gnb:sdr}"; do
    sudo docker image inspect "$IMG" >/dev/null 2>&1 || continue
    phase "Edge platform [4b/6] — OCUDU gNB image ($IMG)"
    sudo docker save "$IMG" -o /tmp/gnb.tar; sudo chmod 644 /tmp/gnb.tar
    scp -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 /tmp/gnb.tar "${EDGE_USER:-ubuntu}@$EDGEIP":/tmp/gnb.tar >/dev/null
    ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 "${EDGE_USER:-ubuntu}@$EDGEIP" \
      "sudo /snap/k8s/current/bin/ctr -a /run/containerd/containerd.sock -n k8s.io images import /tmp/gnb.tar >/dev/null 2>&1"
  done
fi

phase "Edge platform [5/6] — edge deploy repo + RootSync"
curl -s -o /dev/null -u "$GITEA_ADMIN_USER:$GITEA_ADMIN_PASS" -X POST "http://$MGMT_IP:32300/api/v1/user/repos" \
  -H 'Content-Type: application/json' -d "{\"name\":\"$CLUSTER\",\"private\":false,\"auto_init\":true,\"default_branch\":\"main\"}" || true
porchctl repo register --namespace default --name "$CLUSTER" --deployment=true \
  --repo-basic-username="$GITEA_ADMIN_USER" --repo-basic-password="$GITEA_ADMIN_PASS" \
  "http://$GITEA_INT/$GITEA_ADMIN_USER/$CLUSTER.git" 2>/dev/null || true
# refresh the PackageVariant controller's repo cache so upf-/multus-/datapath- PVs resolve the new repo
kubectl -n porch-system rollout restart deploy porch-controllers >/dev/null 2>&1 || true
EK create ns config-management-system >/dev/null 2>&1 || true
EK create secret generic git-creds -n config-management-system \
  --from-literal=username="$GITEA_ADMIN_USER" --from-literal=token="$GITEA_ADMIN_PASS" \
  --dry-run=client -o yaml | EK apply -f - >/dev/null 2>&1
cat <<EOF | EK apply -f - >/dev/null 2>&1
apiVersion: configsync.gke.io/v1beta1
kind: RootSync
metadata: { name: edge-sync, namespace: config-management-system }
spec:
  sourceFormat: unstructured
  sourceType: git
  git: { repo: "http://$MGMT_IP:32300/$GITEA_ADMIN_USER/$CLUSTER.git", branch: main, dir: /, auth: token, secretRef: { name: git-creds } }
EOF

phase "Edge platform [6/6] — converge packages to the CURRENT blueprints"
# The PackageVariant controller can only CLONE a downstream package once; it can NOT update an existing
# one (Porch errors: "clone cannot create a new revision ... make subsequent revisions using copy"). So a
# blueprint change (e.g. a new operator image carrying a datapath self-heal fix) NEVER reaches an
# already-onboarded edge on its own — the edge keeps serving the revision it was first cloned at. Converge
# the edge by purging its downstream package revisions so the EdgeSite-owned PVs re-clone the CURRENT
# blueprint into fresh downstreams; Config Sync then rolls the new content out. Idempotent: a first-time
# onboard has nothing to purge (the PVs simply clone for the first time). A brief datapath flap is expected
# while packages re-clone — the node-agent self-heals the UPF afterwards, so the edge lands UE-ready.
say "converging '$CLUSTER' packages to current blueprints (PVs cannot update in place — re-clone)…"
for r in $(kubectl get packagerevisions -o name 2>/dev/null | sed 's#.*/##' | grep "^$CLUSTER\."); do
  porchctl rpkg propose-delete "$r" -n default >/dev/null 2>&1 || true
  porchctl rpkg del "$r" -n default >/dev/null 2>&1 || true
done
# nudge the EdgeSite so its owned PVs re-reconcile and re-clone immediately (don't wait for periodic resync)
kubectl annotate edgesite "$EDGE" sdcore.nephio.io/refresh="$(date +%s)" --overwrite >/dev/null 2>&1 || true

ok "edge platform ready on $EDGE"
say "the EdgeSite controller's upf-/multus-/datapath- PackageVariants now publish to the $CLUSTER repo ->"
say "Config Sync deploys the BESS-UPF + multus + datapath; the MeshLink controller wires N2/N4 over ClusterMesh."
