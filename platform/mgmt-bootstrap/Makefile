# Management-plane bootstrap (run ONCE per mgmt cluster — this is ops IaC, not product runtime).
# The PRODUCT (cluster + 5G intent) is declarative YAML consumed AFTER this exists.
#
#   make all         # stand up the whole management plane
#   make mgmt-up      # 1. management k8s cluster (LAN-reachable API)
#   make byoh-image   # 2. build BYOH controller from source (upstream image is DEAD)
#   make providers    # 3. CAPI core + Canonical-K8s + BYOH (clusterctl init)
#   make byoh-fix     # 4. repoint BYOH at our built image + fix dead kube-rbac-proxy
#   make nephio       # 5. Nephio (Porch, specializers, IPAM) — vendored+pinned kpt packages
#   make gitea        # 6. in-cluster git backend + Porch repo wiring (appliance; no external git)
include versions.env
export

KUBECONFIG ?= $(HOME)/.kube/config
DNS_UPSTREAMS ?= 8.8.8.8 1.1.1.1     # in-cluster DNS upstreams (override for air-gapped/enterprise DNS)

# Host-agnostic: if MGMT_LAN_IP is unset, auto-detect the host's primary IP.
MGMT_LAN_IP := $(if $(strip $(MGMT_LAN_IP)),$(MGMT_LAN_IP),$(shell ip -4 route get 1.1.1.1 2>/dev/null | awk '{print $$7; exit}'))
export MGMT_LAN_IP

# Load local images into Canonical K8s's OWN containerd (its bundled ctr + socket; not docker's,
# and CK8s has no `k8s ctr`). Socket discovered under the containerd-base-dir we set.
# LAZY (=) not immediate (:=): the socket only exists AFTER `k8s bootstrap` runs, which is a later
# target in the same `make mgmt`. `:=` would expand at parse time (socket absent) -> empty `-a` ->
# `ctr: No help topic for 'k8s.io'`. `=` re-resolves at recipe time, by when the socket exists.
CK8S_SOCK = $(shell sudo find /var/lib/ck8s-containerd -name containerd.sock 2>/dev/null | head -1)
CK8S_CTR = sudo /snap/k8s/current/bin/ctr -a $(CK8S_SOCK) -n k8s.io

.PHONY: all prereqs mgmt-up byoh-image providers byoh-fix nephio gitea add-server clean
all: prereqs mgmt-up byoh-image providers byoh-fix nephio gitea

# Install the pinned CLI toolchain (go/kustomize/porchctl/clusterctl/kpt) this bootstrap needs.
# Runs first so a FRESH host is reproducible — no hand-copying binaries. Idempotent.
prereqs:
	bash install-prereqs.sh

# Onboard ANY bare server as a Nephio edge in ONE command. After this, EdgeSite intent provisions a
# cluster + deploys the UPF on it hands-free.   Usage: make add-server HOST=<server-ip>
add-server:
	@test -n "$(HOST)" || { echo "usage: make add-server HOST=<server-ip>"; exit 1; }
	@echo ">> onboarding $(HOST) as a Nephio edge server (BYOH)"
	MGMT_LAN_IP=$(MGMT_LAN_IP) bash byoh-bootstrap.sh /tmp/bootstrap-kubeconfig.conf
	scp -o StrictHostKeyChecking=no /tmp/byoh-hostagent /tmp/bootstrap-kubeconfig.conf register-host.sh ubuntu@$(HOST):/tmp/
	ssh -o StrictHostKeyChecking=no ubuntu@$(HOST) "sudo bash /tmp/register-host.sh $(MGMT_LAN_IP)"
	@echo ">> $(HOST) onboarded. Apply an EdgeSite (label sdcore.nephio.io/provision-cluster=true) to provision it."

mgmt-up:
	@echo ">> mgmt = Canonical Kubernetes ($(CK8S_CHANNEL)), API reachable at $(MGMT_LAN_IP):6443"
	sudo snap install k8s --classic --channel=$(CK8S_CHANNEL)
	# idempotent: only bootstrap if this node isn't already a cluster member (re-runs must not error).
	sudo k8s status >/dev/null 2>&1 || sudo k8s bootstrap --file ck8s-bootstrap.yaml
	# A reused host can carry a stray/FOREIGN CNI conf (a prior multus/calico/canal/flannel/weave) that sorts
	# BEFORE CK8s's own cilium conflist and shadows it -> every pod stuck ContainerCreating, or
	# "plugin type=\"calico\" ... not found" pod-sandbox failures. Keep ONLY CK8s's cilium as the node CNI.
	sudo find /etc/cni/net.d -maxdepth 1 -type f \( -iname '*multus*' -o -iname '*calico*' -o -iname '*canal*' -o -iname '*flannel*' -o -iname '*weave*' \) -delete 2>/dev/null || true
	# If docker is co-installed (we use it to build the BYOH image), it forces the iptables
	# FORWARD policy to DROP -> silently kills ALL pod egress (CoreDNS can't reach upstream,
	# Porch can't reach git). Re-open FORWARD and persist it so it survives docker/reboot.
	sudo iptables -P FORWARD ACCEPT
	printf '[Unit]\nDescription=keep FORWARD open for pod egress (docker sets DROP)\nAfter=docker.service\n[Service]\nType=oneshot\nExecStart=/usr/sbin/iptables -P FORWARD ACCEPT\n[Install]\nWantedBy=multi-user.target\n' \
	  | sudo tee /etc/systemd/system/pod-egress.service >/dev/null
	sudo systemctl enable pod-egress.service >/dev/null 2>&1 || true
	sudo k8s status --wait-ready
	sudo k8s enable local-storage 2>/dev/null || true   # default StorageClass (idempotent on re-run)
	# CK8s CoreDNS forwards to the node stub resolver (127.0.0.53); on hosts where that has no
	# usable upstream, ALL in-cluster DNS fails (Porch can't resolve git/registries). Pin upstreams.
	sudo k8s set dns.upstream-nameservers="$(DNS_UPSTREAMS)" 2>/dev/null || true
	mkdir -p $(HOME)/.kube
	sudo k8s config > $(HOME)/.kube/config
	sudo chown $$(id -u):$$(id -g) $(HOME)/.kube/config
	# 5G N2 (gNB->AMF / NGAP) rides SCTP; CK8s Cilium ships enable-sctp=false and silently DROPS it,
	# so the gNB SCTP association hangs. MUST be set via the ck-network HELM release — a raw
	# cilium-config patch is REVERTED by CK8s's helm reconciler on its next pass (cluster-id/sctp snap
	# back). Helm value persists across reconciles.
	# l2announcements: lets a Service type=LoadBalancer own a RAN-reachable IP (ARP-announced on the node's
	# L2) so a REAL gNB can reach the AMF NGAP over SCTP — Cilium's LB datapath handles SCTP (the kube-proxy
	# NodePort path does NOT). This is what makes "plug in any radio" work; the pool/Service are KRM intent.
	# clustermesh: the mgmt cluster is born MESHABLE (distinct cluster.id/name + its own clustermesh-apiserver)
	# so edge UPFs can reach the mgmt SMF/AMF cross-cluster. This is the mesh-ENABLE (bootstrap, per-cluster
	# identity — CK8s exposes no declarative cilium hook, so it's a helm value like sctp). The mesh CONNECT
	# (shared CA + cilium-clustermesh Secrets) is emitted by the MeshLink controller as KRM via Config Sync.
	sudo /snap/k8s/current/bin/helm --kubeconfig /etc/kubernetes/admin.conf -n kube-system \
	  upgrade ck-network $$(ls -d /snap/k8s/current/k8s/manifests/charts/cilium* 2>/dev/null | head -1) \
	  --reuse-values --set sctp.enabled=true --set l2announcements.enabled=true \
	  --set k8sClientRateLimit.qps=50 --set k8sClientRateLimit.burst=100 \
	  --set cluster.id=1 --set cluster.name=mgmt --set clustermesh.useAPIServer=true
	# Resilience: the helm re-roll makes cilium RE-PULL its image from ghcr; on a host with flaky/slow ghcr
	# access (packets to GitHub's CDN time out) or a Tailscale-only resolver, that pull hangs -> no CNI -> the
	# whole cluster wedges (every pod ContainerCreating). Seed the EXACT cilium image into containerd via docker
	# (better client + retries) so bring-up never depends on a live ghcr pull. Best-effort + idempotent.
	@CIMG=$$(kubectl --kubeconfig $(HOME)/.kube/config -n kube-system get ds cilium -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null); \
	 if [ -n "$$CIMG" ] && ! $(CK8S_CTR) images ls 2>/dev/null | grep -q "$$CIMG"; then \
	   echo ">> seeding cilium image $$CIMG into containerd (resilient to flaky ghcr)…"; \
	   for t in 1 2 3 4 5 6; do sudo docker pull "$$CIMG" && break; echo "   ghcr retry $$t…"; sleep 5; done; \
	   sudo docker save "$$CIMG" -o /tmp/cil-seed.tar 2>/dev/null && $(CK8S_CTR) images import /tmp/cil-seed.tar 2>/dev/null; rm -f /tmp/cil-seed.tar; \
	   kubectl --kubeconfig $(HOME)/.kube/config -n kube-system delete pod -l k8s-app=cilium 2>/dev/null || true; \
	 fi
	# helm already re-rolls cilium for the sctp change; just wait for it to settle. Tolerant on purpose:
	# a fresh single-node bootstrap can be slow to converge and must NOT hard-fail the whole bring-up.
	kubectl --kubeconfig $(HOME)/.kube/config -n kube-system rollout status ds/cilium --timeout=240s \
	  || kubectl --kubeconfig $(HOME)/.kube/config -n kube-system wait --for=condition=ready pod -l k8s-app=cilium --timeout=120s \
	  || true
	sudo k8s status --wait-ready
	kubectl get nodes

byoh-image:
	# Upstream BYOH images are gone (projects.registry.vmware.com decommissioned) -> build + mirror.
	rm -rf /tmp/byoh-src
	git clone -q --depth 1 --branch $(BYOH_VERSION) \
	  https://github.com/vmware-tanzu/cluster-api-provider-bringyourownhost.git /tmp/byoh-src
	cd /tmp/byoh-src && GOTOOLCHAIN=auto IMG=$(BYOH_CONTROLLER_IMAGE) make docker-build
	cd /tmp/byoh-src && GOTOOLCHAIN=auto GOOS=linux GOARCH=amd64 go build -o /tmp/byoh-hostagent ./agent
	docker save $(BYOH_CONTROLLER_IMAGE) -o /tmp/byoh.tar
	# import into the Canonical K8s containerd (its own socket + ctr)
	$(CK8S_CTR) images import /tmp/byoh.tar
	# pre-seed the dead kube-rbac-proxy from a live mirror. Both steps idempotent on re-run: `images tag`
	# errors "already exists" the second time, which must NOT fail the bootstrap (re-runs are expected).
	-$(CK8S_CTR) images pull $(KUBE_RBAC_PROXY)
	-$(CK8S_CTR) images tag $(KUBE_RBAC_PROXY) gcr.io/kubebuilder/kube-rbac-proxy:v0.8.0 2>/dev/null || true

providers:
	clusterctl init --config clusterctl-config.yaml \
	  --bootstrap canonical-kubernetes \
	  --control-plane canonical-kubernetes \
	  --infrastructure byoh

byoh-fix:
	kubectl set image deploy/byoh-controller-manager -n byoh-system manager=$(BYOH_CONTROLLER_IMAGE)
	kubectl patch deploy byoh-controller-manager -n byoh-system --type=json \
	  -p '[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]'
	# Upstream kustomize caps the manager at 30Mi — it OOMKills (exit 137, crash-loop) once a few edge
	# clusters register, which takes down the BootstrapKubeconfig webhook -> `make add-server` fails with
	# "failed calling webhook". Same failure class as the multus 50Mi OOM. Give it real headroom.
	kubectl -n byoh-system set resources deploy/byoh-controller-manager -c manager \
	  --limits=cpu=500m,memory=256Mi --requests=cpu=100m,memory=64Mi
	# make CK8s accept BYOH host self-registration (bootstrap-token auth + client-CA trust + host RBAC)
	./ck8s-byoh-integration.sh

nephio:
	# Nephio core (Porch, specializers, workload CRDs, IPAM backend) from VENDORED pinned
	# kpt packages under ./nephio-packages — idempotent kpt-live reconcile, no external deps.
	./install-nephio.sh

gitea:
	# In-cluster git backend + Porch repo wiring (appliance model). Needs Porch (make nephio) first.
	./install-gitea.sh

clean:
	sudo snap remove k8s --purge 2>/dev/null || true
	@for br in sdcore-access sdcore-core; do sudo ip link delete $$br 2>/dev/null || true; done
	sudo rm -rf /var/lib/ck8s-containerd /etc/kubernetes /capi /run/containerd 2>/dev/null || true
	sudo find /etc/cni/net.d -maxdepth 1 -type f -delete 2>/dev/null || true
	@echo ">> mgmt host wiped (pristine)."
