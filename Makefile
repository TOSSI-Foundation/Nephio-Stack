# Nephio 5G Edge Product — the whole e2e stack. BOOTSTRAP is imperative (you can't install Nephio with
# Nephio); EVERYTHING per-site is pure KRM (an EdgeSite intent the operator expands — no orchestration).
#
#   make mgmt                          # BOOTSTRAP 1: mgmt plane — CK8s + CAPI + BYOH + Nephio + Gitea
#   make publish                       # BOOTSTRAP 2: publish blueprints (v1) + deploy operator/approver/IPAM
#   make edge HOST=<ip> NAME=<n> ROLE=<r>  # PRODUCT: one site (role upf-only | cp+upf)
#   make sites                             # PRODUCT: reconcile the WHOLE fleet from fleet.yaml (declarative)
#   # fleet.yaml is ONE tiny intent — a list of {server, role}. The Fleet controller expands each into an
#   # EdgeSite -> cluster/CP/UPF PackageVariants; Nephio does the rest. Add a line = a site; rm a line = gone.
#   # then (opt-in) validate a UE with e2e/ueransim-*.yaml
#
include platform/mgmt-bootstrap/versions.env
export
# Blueprints are published to the IN-CLUSTER Gitea (appliance model — no external git).
# Host reaches Gitea over the node LAN NodePort; Porch reads it over cluster-internal DNS.
MGMT_NODE_IP ?= $(shell kubectl get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null)
# the address remote hosts use to reach the mgmt API; defaults to the detected node IP
MGMT_LAN_IP := $(if $(strip $(MGMT_LAN_IP)),$(MGMT_LAN_IP),$(MGMT_NODE_IP))
BP_REPO ?= http://$(GITEA_ADMIN_USER):$(GITEA_ADMIN_PASS)@$(MGMT_NODE_IP):32300/$(GITEA_ADMIN_USER)/blueprints.git

SHELL := /bin/bash
.PHONY: mgmt prereqs gnb-image publish add-server edge edge-delete sites sites-delete host-serve blueprint-operator reset clean

## 0. Prereqs: install the pinned CLI toolchain (go/kustomize/porchctl/clusterctl/kpt) on a fresh host.
## `make mgmt` runs this automatically; exposed here to run it standalone.
prereqs:
	$(MAKE) -C platform/mgmt-bootstrap prereqs

## 1. Management plane (Canonical K8s + CAPI + CK8s + BYOH + Nephio). Installs prereqs first.
mgmt:
	$(MAKE) -C platform/mgmt-bootstrap all

## 1b. Turn the mgmt VM into a FULL node too: a colocated cp+upf 5G core + an OCUDU gNB on 192.168.4.54
## (needs `make publish` first so the operator + gNB image are in place).
mgmt-core:
	kubectl apply -f intent/mgmtcore.yaml

## ONE-SHOT bring-up of the WHOLE fabric: mgmt platform + operator/blueprints + the colocated mgmt core
## + every edge site (cp+upf + OCUDU gNB) from fleet.yaml. Prereqs run in order. (Phased is safer for
## debugging: `make mgmt` -> `make publish` -> `make mgmt-core` -> `make sites`.)
up: mgmt publish mgmt-core sites

## Tear the WHOLE fabric down: wipe the edge VMs back to bare (while CAPI is still alive to deprovision
## them), THEN nuke the mgmt platform (CK8s + colocated core) on 192.168.4.54.
down: sites-delete clean

## The per-site flow is now PURE KRM — no `make cp`/`make edge` orchestration. After bootstrap you just:
##    kubectl apply -f <your-edgesites.yaml>
## and the EdgeSite controller emits the cluster/CP/UPF/multus/datapath PackageVariants; Porch + Config
## Sync materialize the SD-Core CP + BESS-UPF + host datapath. (The old deploy-*.sh path is deleted.)

## Build the UNIVERSAL OCUDU gNB image (UHD + DPDK + MKL + ZMQ) from source — a product artifact, like the
## operator image. It has no public registry, so it's seeded into clusters by `make publish` (mgmt) and
## `make sites`/`make edge` (edges). Needs the OCUDU source at OCUDU_SRC (default ~/ocudu; clone
## gitlab.com/ocudu/ocudu with --recurse-submodules). One image drives n310/b210/x310/liteon-7.2/zmq.
OCUDU_SRC ?= $(HOME)/ocudu
gnb-image:
	@test -d "$(OCUDU_SRC)/cmake" || { echo "OCUDU source not found at $(OCUDU_SRC) — clone gitlab.com/ocudu/ocudu (--recurse-submodules) or pass OCUDU_SRC=<path>"; exit 1; }
	rsync -a --exclude '/build/' --exclude '/build_uhd/' --exclude '/build_*/' --exclude '.git' "$(OCUDU_SRC)/" images/ocudu-gnb/ocudu-src/
	cd images/ocudu-gnb && sudo docker build -t $(RAN_GNB_UHD_IMAGE) -t ocudu/gnb:universal .
	@echo ">> built $(RAN_GNB_UHD_IMAGE) — 'make publish' seeds it to mgmt, 'make sites' to edges."

## 2a. Build the operator image + render its install blueprint
blueprint-operator:
	$(MAKE) -C operator docker-build IMG=$(SDCORE_OPERATOR_IMAGE)
	cd operator && kustomize build config/default > ../blueprints/sdcore-operator/install.yaml

## 2b. Publish the blueprints/ packages to the in-cluster Gitea repo Porch reads (registered by `make gitea`).
## Only blueprints/ is pushed, at the repo ROOT, so Porch sees sdcore-upf/sdcore-cp/... as top-level packages.
publish: blueprint-operator
	@test -n "$(MGMT_NODE_IP)" || (echo "MGMT_NODE_IP empty — is the mgmt kubeconfig active? run 'make mgmt' first"; exit 1)
	# Publish blueprint content on the repo's EXISTING git history (clone -> sync -> commit -> push), NOT a
	# fresh `git init` + force-push. Porch OWNS this repo: a force-push that REWRITES history is not adopted by
	# Porch's `.main` cache, and Porch's own revision-writes then clobber the branch back to the old content
	# (so blueprint EDITS silently never reach any target). A normal commit ON TOP of the existing history is
	# adopted by Porch's git poller (~30s). Clone if the repo exists; init once if it is brand-new/empty.
	rm -rf /tmp/bp-publish
	git clone -q $(BP_REPO) /tmp/bp-publish 2>/dev/null || { mkdir -p /tmp/bp-publish && git -C /tmp/bp-publish init -q -b $(NEPHIO_BRANCH); }
	rsync -a --delete --exclude='.git' blueprints/ /tmp/bp-publish/
	@cd /tmp/bp-publish && git -c user.email=dev@localhost -c user.name=dev add -A; \
	  if git diff --cached --quiet; then echo "   (blueprints unchanged — skipping the Porch republish)"; echo 0 > /tmp/bp-changed; \
	  else git -c user.email=dev@localhost -c user.name=dev commit -qm "publish blueprints"; \
	       git -c http.sslVerify=false push $(BP_REPO) $(NEPHIO_BRANCH); echo 1 > /tmp/bp-changed; fi
	# Wait for Porch to ADOPT the pushed commit into `.main` BEFORE publishing numbered revisions — otherwise
	# the copy below captures Porch's stale cache and Porch's revision-write clobbers our push to old content.
	# Porch records the synced commit in .status.selfLock.git.commit; poll until it equals our pushed HEAD.
	# Only republish blueprints to Porch when they ACTUALLY changed. An operator-only change (99% of
	# iterations) skips this entire fragile Porch-fighting dance, so `make publish` COMPLETES instead of
	# hanging in the re-assert loop. And even for a real blueprint change the loop is non-fatal (the
	# numbered revisions carry the content), so the operator deploy below ALWAYS runs.
	@if [ "$$(cat /tmp/bp-changed 2>/dev/null)" != 1 ]; then echo ">> blueprints unchanged — skipping Porch republish (operator-only publish)."; else \
	  HEAD=$$(git -C /tmp/bp-publish rev-parse HEAD); echo ">> waiting for Porch to adopt blueprints @ $$HEAD into .main…"; \
	  for t in $$(seq 1 40); do c=$$(kubectl get packagerevision blueprints.datapath-actuator.main -o jsonpath='{.status.selfLock.git.commit}' 2>/dev/null); [ "$$c" = "$$HEAD" ] && break; sleep 3; done; \
	  echo ">> publishing each blueprint as the NEXT numbered Porch revision (PackageVariants clone it)…"; \
	  for pkg in cluster-ck8s-byoh sdcore-cp edge-site datapath-actuator multus workload-cluster-ck8s sdcore-upf sdcore-operator n2-expose; do \
	    for t in $$(seq 1 30); do kubectl get packagerevision blueprints.$$pkg.main >/dev/null 2>&1 && break; sleep 2; done; \
	    N=$$(kubectl get packagerevisions 2>/dev/null | awk -v p="$$pkg" '$$1 ~ ("blueprints." p ".v[0-9]+$$") {n=$$1; sub(/.*\.v/,"",n); if(n+0>m)m=n+0} END{print m+0}'); \
	    NEXT=v$$(( N + 1 )); d=""; \
	    for c in 1 2 3; do porchctl rpkg copy blueprints.$$pkg.main -n default --workspace $$NEXT >/dev/null 2>&1 || true; \
	      for t in $$(seq 1 12); do d=$$(kubectl get packagerevisions 2>/dev/null | awk '$$1 ~ /'"$$pkg"'.'"$$NEXT"'$$/ {print $$1}' | head -1); [ -n "$$d" ] && break; sleep 2; done; [ -n "$$d" ] && break; done; \
	    if [ -n "$$d" ]; then for t in $$(seq 1 15); do lc=$$(kubectl get packagerevision $$d -o jsonpath='{.spec.lifecycle}' 2>/dev/null); [ "$$lc" = "Published" ] && break; [ "$$lc" = "Draft" ] && porchctl rpkg propose $$d -n default >/dev/null 2>&1; porchctl rpkg approve $$d -n default >/dev/null 2>&1; sleep 2; done; fi; \
	    echo "   $$pkg -> $$NEXT $$(kubectl get packagerevisions 2>/dev/null | awk '$$1 ~ /'"$$pkg"'.'"$$NEXT"'$$/ {print $$4,$$5,$$6}')"; \
	  done; \
	  ok=0; for attempt in 1 2 3 4 5 6; do \
	    rm -rf /tmp/bp-reassert; git -c http.sslVerify=false clone -q $(BP_REPO) /tmp/bp-reassert; \
	    rsync -a --delete --exclude='.git' blueprints/ /tmp/bp-reassert/; \
	    cd /tmp/bp-reassert && git -c user.email=dev@localhost -c user.name=dev add -A && (git diff --cached --quiet || git -c user.email=dev@localhost -c user.name=dev commit -qm "re-assert $$attempt") && git -c http.sslVerify=false push -q $(BP_REPO) $(NEPHIO_BRANCH); cd - >/dev/null; \
	    HEAD=$$(git -C /tmp/bp-reassert rev-parse HEAD); echo ">> re-assert $$attempt: waiting for Porch .main to adopt $$HEAD…"; \
	    for t in $$(seq 1 24); do CUR=$$(kubectl get packagerevision blueprints.datapath-actuator.main -o jsonpath='{.status.selfLock.git.commit}' 2>/dev/null); [ "$$CUR" = "$$HEAD" ] && break; sleep 5; done; sleep 12; \
	    CUR=$$(kubectl get packagerevision blueprints.datapath-actuator.main -o jsonpath='{.status.selfLock.git.commit}' 2>/dev/null); \
	    if [ "$$CUR" = "$$HEAD" ]; then echo ">> Porch .main STABLE = published content ($$HEAD)"; ok=1; break; fi; \
	    echo ">> Porch pushed again (.main=$$CUR) — re-asserting on top"; \
	  done; \
	  [ "$$ok" = 1 ] || echo ">> WARN: Porch .main not stable after 6 rounds; the numbered revisions (vN) carry the new content — continuing (operator deploy NOT blocked)."; \
	fi
	@echo ">> blueprints published (.main gated = current content). PackageVariants clone .main."
	@echo ">> deploying the SD-Core operator (EdgeSite/MeshLink controllers) + auto-approver + IPAM pool (bootstrap)..."
	SOCK=$$(sudo find /var/lib/ck8s-containerd -name containerd.sock 2>/dev/null | head -1); \
	  sudo docker save $(SDCORE_OPERATOR_IMAGE) -o /tmp/op.tar && sudo /snap/k8s/current/bin/ctr -a $${SOCK:-/run/containerd/containerd.sock} -n k8s.io images import /tmp/op.tar; \
	  for IMG in $(RAN_GNB_IMAGE) $(RAN_GNB_UHD_IMAGE); do \
	    if sudo docker image inspect $$IMG >/dev/null 2>&1; then \
	      echo ">> seeding OCUDU gNB image $$IMG into mgmt containerd"; \
	      sudo docker save $$IMG -o /tmp/gnb.tar && sudo /snap/k8s/current/bin/ctr -a $${SOCK:-/run/containerd/containerd.sock} -n k8s.io images import /tmp/gnb.tar; \
	    else echo "!! $$IMG not in docker (skip — build it if a site uses that device)"; fi; \
	  done
	kubectl apply -f operator/config/crd/bases/ >/dev/null 2>&1 || true
	cd operator && kubectl apply -k config/default >/dev/null 2>&1 || true
	kubectl -n sdcore-operator-system set image deploy/sdcore-operator-controller-manager manager=$(SDCORE_OPERATOR_IMAGE) >/dev/null 2>&1 || true
	kubectl -n sdcore-operator-system patch deploy sdcore-operator-controller-manager --type=json -p '[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' >/dev/null 2>&1 || true
	# force the freshly-imported binary even on an unchanged image tag (IfNotPresent won't re-pull a cached tag)
	kubectl -n sdcore-operator-system delete pod --all >/dev/null 2>&1 || true
	# same for the node-agent DaemonSet (runs this operator image as the datapath actuator) if already present.
	# It MUST be pinned to the CURRENT operator image, not just restarted: the DS tag is set once when its
	# package is first cloned and never updates in place, so after an operator bump it keeps referencing an
	# OLD tag that containerd has since GC'd -> ImagePullBackOff -> the N6 NAT actuator never runs -> UEs get
	# a PDU session + IP but NO internet. Set the image to $(SDCORE_OPERATOR_IMAGE) (just imported above).
	kubectl -n kube-system set image ds/sdcore-datapath $$(kubectl -n kube-system get ds sdcore-datapath -o jsonpath='{.spec.template.spec.containers[0].name}' 2>/dev/null)=$(SDCORE_OPERATOR_IMAGE) >/dev/null 2>&1 || true
	kubectl -n kube-system rollout restart ds/sdcore-datapath >/dev/null 2>&1 || true
	# converge the mgmt's OWN datapath package to the just-published blueprint. PackageVariants can only
	# CLONE once (never update in place), so without this the mgmt node-agent stays pinned to the tag it
	# was first cloned at forever — a published heal/feature never reaches the mgmt host (bit us live:
	# the N2-VIP announce sat unpublished while a hand-added /32 did its job). ONLY datapath-mgmt is
	# purged: touching mgmt.cluster-* would make Config Sync prune the CAPI Clusters = deprovision edges.
	# Safe pairing: this same publish already imported the new image into the mgmt containerd above.
	@for r in $$(kubectl get packagerevisions -o name 2>/dev/null | sed 's#.*/##' | grep -E '^mgmt\.(datapath-mgmt|n2-mgmtcore)\.'); do \
	  porchctl rpkg propose-delete $$r -n default >/dev/null 2>&1 || true; \
	  porchctl rpkg del $$r -n default >/dev/null 2>&1 || true; \
	done
	kubectl annotate edgesite --all sdcore.nephio.io/blueprint-refresh=$$(git -C /tmp/bp-publish rev-parse --short HEAD 2>/dev/null || echo manual) --overwrite >/dev/null 2>&1 || true
	kubectl apply -f platform/controllers/autoapprove/ >/dev/null 2>&1 || true
	kubectl apply -f intent/networkinstance-sdcore-vpc.yaml >/dev/null 2>&1 || true
	@echo ">> NATIVE platform ready: operator + auto-approver + sdcore-vpc IPAM pool."
	@echo ">> Now the WHOLE per-site flow is pure KRM:  kubectl apply -f <your-edgesites.yaml>"

## 3. Register servers. THREE ways, by scale — none require typing N IPs:
##
##  (a) A FEW servers  -> convenience push:
##        make add-server HOST=<ip>
##  (b) MANY servers   -> self-register (recommended). Serve the installer ONCE:
##        make host-serve      # then each server (cloud-init/Ansible/client) runs:
##        #   curl -sfL http://<MGMT_IP>:8080/register | sudo bash
##      100 servers = 100 one-liners fired by the image/Ansible, NOT 100 IPs typed here.
##  (c) ZERO-touch bare metal -> MAAS PXE-provisions + auto-registers (no command at all).
##
## Servers may be on ANY subnet/switch — registration is L3 (just reach the mgmt API);
## the cross-subnet DATAPATH is handled by the MeshLink controller (Cilium ClusterMesh), not here.
SSH_OPTS ?= -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15
# SSH_USER: the login on the target hosts (mgmt -> edge). Empty = current user (works when both are
# `ubuntu`). Set it when the edge login differs, e.g. `make sites SSH_USER=one`. Threaded into add-server
# AND install-edge-platform.sh (as EDGE_USER) so onboarding never assumes a hardcoded `ubuntu`.
SSH_USER ?=
SSHUP = $(if $(strip $(SSH_USER)),$(SSH_USER)@,)
add-server:
	@test -n "$(HOST)" || (echo "usage: make add-server HOST=<ip> [SSH_USER=<login>]"; exit 1)
	@ssh $(SSH_OPTS) $(SSHUP)$(HOST) true 2>/dev/null || { \
	  echo ">> ERROR: the mgmt node cannot SSH to $(SSHUP)$(HOST) (key-based auth failed)."; \
	  echo "   The push model (make add-server / make edge) needs SSH from mgmt to the target. Fix ONE:"; \
	  echo "     1) ssh-copy-id $(SSHUP)$(HOST)                       # add the mgmt's key to the target, then re-run"; \
	  echo "     2) self-register (no SSH from mgmt): 'make host-serve', then ON $(HOST) run:"; \
	  echo "          curl -sfL http://$(MGMT_LAN_IP):8080/register | sudo bash"; \
	  exit 1; }
	@HN=$$(ssh $(SSH_OPTS) $(SSHUP)$(HOST) hostname 2>/dev/null | tr -d '[:space:]'); \
	 BOUND=$$(kubectl get byohost "$$HN" -n default -o jsonpath='{.status.machineRef.name}' 2>/dev/null || true); \
	 if [ -n "$$HN" ] && [ -n "$$BOUND" ]; then \
	   echo ">> $$HN already registered + bound to machine $$BOUND — IDEMPOTENT skip (re-registering a"; \
	   echo "   provisioned host would churn CAPI: delete the ByoHost -> orphan the Machine -> re-bootstrap)."; \
	 else \
	   echo ">> registering $(HOST)$${HN:+ ($$HN)}"; \
	   MGMT_LAN_IP=$(MGMT_LAN_IP) platform/mgmt-bootstrap/byoh-bootstrap.sh /tmp/bootstrap-kubeconfig.conf; \
	   if [ -n "$$HN" ]; then \
	     kubectl delete csr byoh-csr-$$HN --ignore-not-found >/dev/null 2>&1 || true; \
	     kubectl delete byohost $$HN -n default --ignore-not-found >/dev/null 2>&1 || true; \
	   fi; \
	   scp $(SSH_OPTS) /tmp/byoh-hostagent /tmp/bootstrap-kubeconfig.conf platform/mgmt-bootstrap/register-host.sh $(HOST):/tmp/; \
	   ssh $(SSH_OPTS) $(HOST) "sudo bash /tmp/register-host.sh $(MGMT_LAN_IP)"; \
	   echo ">> $(HOST) registering; watch: kubectl get byohosts"; \
	 fi

## Onboard a whole EDGE (separate server) in ONE command: host-join -> CAPI provisions the cluster ->
## edge-platform bootstrap (Config Sync + sdcore-operator + repo/RootSync — the one imperative per-edge
## step, same bootstrap paradox as `make mgmt`) -> apply the EdgeSite intent. Everything after that is
## PURE KRM: the EdgeSite controller's upf-/multus-/datapath- PackageVariants deploy the BESS-UPF +
## multus + datapath on the edge via Config Sync, and the MeshLink wires N2/N4. (ClusterMesh connect is
## the final cross-cluster step — see install-edge-platform.sh [6/6].)   Usage: make edge HOST=<ip> NAME=<name>
# ROLE selects the site topology (default a UPF steered by the central mgmt CP):
#   ROLE=upf-only  (default) — a BESS-UPF on a new cluster, N2/N4 meshed back to the mgmt CP.
#   ROLE=cp+upf              — a SELF-CONTAINED 5G core (SD-Core CP + its own UPF + webui + slice + SIMs)
#                             on a new cluster; no mesh, a real gNB attaches straight to THIS site's AMF.
ROLE ?= upf-only
edge:
	@test -n "$(HOST)" || { echo 'usage: make edge HOST=<edge-ip> NAME=<edge-name> [ROLE=upf-only|cp+upf]'; exit 1; }
	@test -n "$(NAME)" || { echo 'usage: make edge HOST=<edge-ip> NAME=<edge-name> [ROLE=upf-only|cp+upf]'; exit 1; }
	$(MAKE) add-server HOST=$(HOST)
	# Apply the EdgeSite FIRST: its cluster-$(NAME) PackageVariant -> Config Sync -> CAPI provisions the
	# edge cluster onto the just-registered host. install-edge-platform.sh (below) WAITS for that cluster,
	# so it must exist first. The cp-/upf-/multus-/datapath- PackageVariants are emitted at the same time
	# but stay pending until install-edge-platform registers the edge deploy repo + RootSync. colocated:false
	# so BOTH roles provision the site's OWN cluster (co-located cp+upf onto mgmt is the separate `mgmtcore`).
	@printf 'apiVersion: sdcore.nephio.io/v1alpha1\nkind: EdgeSite\nmetadata: { name: %s, namespace: default }\nspec: { server: %s, role: %s, colocated: false }\n' "$(NAME)" "$(HOST)" "$(ROLE)" | kubectl apply -f -
	EDGE_USER=$(SSH_USER) platform/mgmt-bootstrap/install-edge-platform.sh $(NAME) $(HOST) $(ROLE)
	@echo ">> edge $(NAME) ($(ROLE)) onboarded onto $(HOST) via Config Sync."

## Reconcile the WHOLE FLEET from ONE tiny intent file (fleet.yaml) — a list of {server, role}, nothing
## else. `kubectl apply` it and the Fleet CONTROLLER expands each entry into an EdgeSite (add a line ->
## a site joins; remove a line -> its EdgeSite is GC'd -> the site is decommissioned). This target applies
## the intent, then does the one imperative seam controllers can't (SSH-register each bare host + bootstrap
## Config Sync onto its fresh cluster) — driven FROM the EdgeSites the controller produced, so there is no
## second inventory to keep in sync. Continues past a failed site.
##   make sites                 # reconcile fleet.yaml
##   make sites FLEET=prod.yaml # a different fleet intent
FLEET     ?= fleet.yaml
FLEET_SEL ?= fleet.sdcore.nephio.io/managed=true
sites:
	@test -f "$(FLEET)" || { echo "$(FLEET) not found"; exit 1; }
	@echo ">> applying fleet intent $(FLEET) — the Fleet controller expands it into EdgeSites"
	kubectl apply -f $(FLEET)
	@echo ">> reconciling (remove-a-line from $(FLEET) = the Fleet controller owner-GCs that EdgeSite -> CAPI"
	@echo "   deprovisions the site, declaratively — no per-host script)…"; sleep 6
	@echo ">> registering hosts (clusters provision in parallel)…"; \
	 kubectl get edgesite -l $(FLEET_SEL) -o jsonpath='{range .items[*]}{.metadata.name} {.spec.server} {.spec.user}{"\n"}{end}' | \
	 while read -r name host user; do [ -n "$$host" ] || continue; tgt=$${user:+$$user@}$$host; echo "   -> $$name ($$tgt)"; \
	   $(MAKE) --no-print-directory add-server HOST=$$tgt </dev/null >/dev/null 2>&1 || echo "   !! register $$name failed"; done
	@echo ">> bootstrapping platforms…"; ok=0; fail=0; \
	 while read -r name host role user; do [ -n "$$host" ] || continue; \
	   echo "=================================================================="; echo ">> SITE $$name ($${user:+$$user@}$$host) role=$$role"; \
	   if EDGE_USER=$${user:-$(SSH_USER)} platform/mgmt-bootstrap/install-edge-platform.sh $$name $$host $$role </dev/null; then ok=$$((ok+1)); \
	   else fail=$$((fail+1)); echo "!! $$name bootstrap FAILED — continuing"; fi; \
	 done < <(kubectl get edgesite -l $(FLEET_SEL) -o jsonpath='{range .items[*]}{.metadata.name} {.spec.server} {.spec.role} {.spec.user}{"\n"}{end}'); \
	 echo "=================================================================="; \
	 echo ">> fleet reconciled: $$ok site(s) up, $$fail failed."

## Tear the WHOLE fleet down — DECLARATIVE. One `kubectl delete` retracts the Fleet intent; the Fleet
## controller owner-GCs every EdgeSite -> its PackageVariants/MeshLink(finalizer)/NetworkSlice/IPClaims ->
## Porch removes the packages -> Config Sync prunes -> CAPI deprovisions EVERY cluster IN PARALLEL. No SSH,
## no per-host script — scales to any N. A released host is made pristine at its NEXT `make add-server`
## (register-host.sh resets any stale CK8s), so 'release' (declarative) and 'wipe' (at re-onboard) are decoupled.
sites-delete:
	kubectl delete -f $(FLEET) --ignore-not-found
	@echo ">> waiting for CAPI to deprovision all edge clusters (parallel)…"
	@for t in $$(seq 1 72); do n=$$(kubectl get clusters -A -o name 2>/dev/null | grep -c '/cluster-' || true); \
	   [ "$$n" = 0 ] && { echo ">> all edge clusters deprovisioned"; break; }; echo "   $$n cluster(s) still unwinding…"; sleep 5; done
	@echo ">> fleet torn down (declarative)."

## Tear ONE edge down — DECLARATIVE. Retract the EdgeSite intent; owner-GC + Config Sync prune + CAPI do the
## rest (PackageVariants/MeshLink/NetworkSlice GC'd, cluster deprovisioned, ByoHost released). No SSH, no
## script. The host is made pristine at its next `make add-server`. Re-onboard any time with `make edge`.
##   Usage: make edge-delete NAME=<edge-name>
edge-delete:
	@test -n "$(NAME)" || { echo 'usage: make edge-delete NAME=<edge-name>'; exit 1; }
	kubectl delete edgesite $(NAME) --ignore-not-found
	@echo ">> waiting for CAPI to deprovision cluster-$(NAME)…"
	@for t in $$(seq 1 72); do kubectl get cluster cluster-$(NAME) >/dev/null 2>&1 || { echo ">> cluster-$(NAME) deprovisioned"; break; }; sleep 5; done

host-serve:
	@echo ">> serving self-register endpoint at http://$(MGMT_LAN_IP):8080/register"
	@echo ">> point servers at:  curl -sfL http://$(MGMT_LAN_IP):8080/register | sudo bash"
	@mkdir -p /tmp/host-serve
	@cp platform/mgmt-bootstrap/register-host.sh /tmp/host-serve/register
	@cp /tmp/byoh-hostagent /tmp/host-serve/byoh-hostagent 2>/dev/null || echo "  (run 'make mgmt' first to build the agent)"
	@echo ">> (also drop the bootstrap-kubeconfig.conf into /tmp/host-serve/)"
	cd /tmp/host-serve && python3 -m http.server 8080

## Reset all deployed SITES back to the clean platform (KEEPS CK8s + Nephio: operator/Porch/Gitea/IPAM/
## approver — the make mgmt+publish bootstrap). The inverse of `kubectl apply -f edgesites.yaml`: deletes
## every EdgeSite+MeshLink (GC their PackageVariants), purges the per-site downstream package revisions from
## the DEPLOY repos (mgmt./cluster-*. — never the blueprints), uninstalls the operator-rendered SD-Core helm
## releases (they outlive the pruned NFDeployment CR), and drops the datapath + multus DaemonSets + the
## UPFDataPath CRs. After this the product deploy is a clean slate again.  Usage: make reset
reset:
	-kubectl delete edgesite --all --wait=false 2>/dev/null
	-kubectl delete meshlink --all --wait=false 2>/dev/null
	@echo ">> purging per-site downstream package revisions from the deploy repos (not the blueprints)..."
	@# TWO passes: propose-delete ALL first, let it propagate, THEN delete ALL. A single patch+delete per
	@# revision races (the delete fires before DeletionProposed propagates and is rejected, leaving the
	@# revision stuck -> a later deploy's clone fails "already exists in repo"). Covers .main AND drafts.
	-@for pr in $$(kubectl get packagerevisions -o name 2>/dev/null | grep -E '/(mgmt|cluster-[a-z0-9]+)\.' || true); do \
	  kubectl patch $${pr#*/} --type=merge -p '{"spec":{"lifecycle":"DeletionProposed"}}' >/dev/null 2>&1 || true; \
	done; true
	@sleep 4
	-@for pr in $$(kubectl get packagerevisions -o name 2>/dev/null | grep -E '/(mgmt|cluster-[a-z0-9]+)\.' || true); do \
	  kubectl delete $${pr#*/} --wait=false >/dev/null 2>&1 || true; \
	done; true
	@# SELF-HEAL: porch occasionally wedges a downstream revision (esp. the published .main) so it won't
	@# delete -> a later deploy's clone fails "already exists". Poll; if any remain, bounce porch-server
	@# (clears its in-memory repo state) and re-run the propose+delete. So the clean-slate is RELIABLE,
	@# never a hand-run porch restart + revision surgery (which is what cost so much time before).
	@-for attempt in 1 2 3; do \
	  left=$$(kubectl get packagerevisions -o name 2>/dev/null | grep -cE '/(mgmt|cluster-[a-z0-9]+)\.' || true); \
	  [ "$${left:-0}" -eq 0 ] && break; \
	  echo "   $${left} downstream revision(s) stuck (attempt $${attempt}) -> bouncing porch-server..."; \
	  kubectl -n porch-system rollout restart deploy/porch-server >/dev/null 2>&1 || true; \
	  kubectl -n porch-system rollout status deploy/porch-server --timeout=90s >/dev/null 2>&1 || true; sleep 5; \
	  for pr in $$(kubectl get packagerevisions -o name 2>/dev/null | grep -E '/(mgmt|cluster-[a-z0-9]+)\.' || true); do \
	    kubectl patch $${pr#*/} --type=merge -p '{"spec":{"lifecycle":"DeletionProposed"}}' >/dev/null 2>&1 || true; \
	    kubectl delete $${pr#*/} --wait=false >/dev/null 2>&1 || true; \
	  done; sleep 3; \
	done
	@echo ">> uninstalling operator-rendered SD-Core helm releases (survive CR deletion)..."
	@# bare `helm` is NOT on PATH (CK8s bundles it under /snap) — the old line silently no-op'd, leaving a
	@# stale upf-0 crashing forever. Use the resolved helm + the active kubeconfig.
	-@H=$$(command -v helm 2>/dev/null || echo /snap/k8s/current/bin/helm); \
	  $$H --kubeconfig $$HOME/.kube/config uninstall sdcore-cp sdcore-upf -n default 2>/dev/null || true
	-kubectl delete nfdeployment --all -n default --wait=false 2>/dev/null
	@# HARD fallback: force-remove the operator-rendered workloads directly, so a reset ALWAYS yields a clean
	@# slate even if the helm release is orphaned/uninstall failed (no lingering StatefulSet -> no stale upf-0).
	-kubectl delete statefulset upf mongodb -n default --wait=false 2>/dev/null
	-kubectl delete deployment amf smf nrf ausf nssf pcf udm udr webui -n default --wait=false 2>/dev/null
	-kubectl -n kube-system delete ds sdcore-datapath --wait=false 2>/dev/null
	-kubectl delete upfdatapath --all 2>/dev/null
	@# DELIBERATELY do NOT delete kube-multus-ds: multus is the node CNI, and ripping it out mid-life kills
	@# pod networking for the WHOLE node (every pod stalls FailedCreatePodSandBox). The Porch purge above
	@# already unmanages the multus package; a redeploy re-clones + Config Sync UPDATES the DS in place.
	@echo ">> sites reset to the clean platform (CNI left intact). Redeploy with: kubectl apply -f <edgesites.yaml>"

clean:
	$(MAKE) -C platform/mgmt-bootstrap clean
