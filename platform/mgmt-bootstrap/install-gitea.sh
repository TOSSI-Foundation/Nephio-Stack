#!/usr/bin/env bash
# Deploy the in-cluster git backend (Gitea) and wire it into Porch — the appliance's git.
#   * blueprints repo  = SOURCE   (make publish pushes our 5G packages here)
#   * mgmt repo        = DEPLOYMENT (Nephio writes specialized packages here; Config Sync reads it)
# Porch reaches Gitea over cluster-internal DNS -> no external git dependency. Idempotent.
set -euo pipefail
: "${GITEA_CHART_VERSION:?}" "${GITEA_ADMIN_USER:?}" "${GITEA_ADMIN_PASS:?}"
HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib.sh"
NS=gitea
GITEA_INT="gitea-http.${NS}.svc.cluster.local:3000"        # in-cluster (Porch uses this)
NODEPORT=32300

phase "In-cluster git backend (Gitea) + Porch repo wiring"
helm repo add gitea https://dl.gitea.com/charts/ >/dev/null 2>&1 || true
helm repo update gitea >/dev/null
run "install Gitea (chart $GITEA_CHART_VERSION) into ns/$NS" \
  helm upgrade --install gitea gitea/gitea --version "$GITEA_CHART_VERSION" \
  -n "$NS" --create-namespace -f "$HERE/gitea-values.yaml" \
  --set gitea.admin.username="$GITEA_ADMIN_USER" \
  --set gitea.admin.password="$GITEA_ADMIN_PASS" \
  --wait --timeout 8m

NODE_IP="$(kubectl get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')"
API="http://${NODE_IP}:${NODEPORT}/api/v1"
ok "Gitea up — UI + host push at http://${NODE_IP}:${NODEPORT}  (user: $GITEA_ADMIN_USER)"

# create the repos we need (idempotent). Nephio deployment-repo topology:
#   blueprints=source | mgmt=cluster CRs (CAPI provisions from here) | mgmt-staging=workload pkgs
for repo in blueprints mgmt mgmt-staging; do
  code=$(curl -s -o /dev/null -w '%{http_code}' -u "$GITEA_ADMIN_USER:$GITEA_ADMIN_PASS" \
    -X POST "$API/user/repos" -H 'Content-Type: application/json' \
    -d "{\"name\":\"$repo\",\"private\":false,\"auto_init\":true,\"default_branch\":\"main\"}")
  case "$code" in 201) ok "created repo /$repo" ;; 409) say "repo /$repo already exists" ;; *) warn "repo /$repo -> HTTP $code" ;; esac
done

# secret Porch uses to auth to Gitea
kubectl create secret generic gitea-auth -n default \
  --type=kubernetes.io/basic-auth \
  --from-literal=username="$GITEA_ADMIN_USER" \
  --from-literal=password="$GITEA_ADMIN_PASS" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# (re)register repos in Porch against the INTERNAL service URL. Drop any prior 'blueprints'
# registration (e.g. the earlier external-GitHub one) so we point cleanly at Gitea.
kubectl delete repository blueprints -n default >/dev/null 2>&1 || true
porchctl repo register --namespace default --name blueprints --deployment=false \
  --repo-basic-username="$GITEA_ADMIN_USER" --repo-basic-password="$GITEA_ADMIN_PASS" \
  "http://${GITEA_INT}/${GITEA_ADMIN_USER}/blueprints.git" 2>/dev/null || echo "  blueprints already registered"
porchctl repo register --namespace default --name mgmt --deployment=true \
  --repo-basic-username="$GITEA_ADMIN_USER" --repo-basic-password="$GITEA_ADMIN_PASS" \
  "http://${GITEA_INT}/${GITEA_ADMIN_USER}/mgmt.git" 2>/dev/null || echo "  mgmt already registered"
porchctl repo register --namespace default --name mgmt-staging --deployment=true \
  --repo-basic-username="$GITEA_ADMIN_USER" --repo-basic-password="$GITEA_ADMIN_PASS" \
  "http://${GITEA_INT}/${GITEA_ADMIN_USER}/mgmt-staging.git" 2>/dev/null || echo "  mgmt-staging already registered"

# Config Sync RootSync on the mgmt cluster, watching the `mgmt` deploy repo: this is the automation
# link that APPLIES whatever Porch writes there (cluster CRs from PackageVariants) — no manual apply.
kubectl create secret generic git-creds -n config-management-system \
  --from-literal=username="$GITEA_ADMIN_USER" --from-literal=token="$GITEA_ADMIN_PASS" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: configsync.gke.io/v1beta1
kind: RootSync
metadata:
  name: mgmt-sync
  namespace: config-management-system
spec:
  sourceFormat: unstructured          # Porch writes arbitrary kpt packages, not the hierarchy layout
  sourceType: git
  git:
    repo: http://${GITEA_INT}/${GITEA_ADMIN_USER}/mgmt.git
    branch: main
    dir: /
    auth: token
    secretRef:
      name: git-creds
EOF

ok "Gitea + Config Sync wired: Porch writes to the mgmt repo -> RootSync auto-applies"
