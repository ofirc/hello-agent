#!/usr/bin/env bash
# Step 2 of 4: build and push the two versions of product-catalog the demo needs.
#
# v1.1.0 and v1.2.0 are the same source. Only the baked-in APP_VERSION differs, and the
# filters feature is gated on it. That is enough to reproduce the opening scenario: a
# customer reports a missing feature, nothing is crashing, prod is one release behind.
set -euo pipefail
cd "$(dirname "$0")"
source ./env.sh
require_auth

SERVICE=product-catalog
SRC="../stack/${SERVICE}"

ensure_build_bucket

for TAG in v1.1.0 v1.2.0; do
  echo "==> Building ${SERVICE}:${TAG} with Cloud Build (no local Docker needed)"
  build_image "$SRC" "${AR_PATH}/${SERVICE}:${TAG}"
done

echo "==> Images in Artifact Registry"
gcloud artifacts docker tags list "${AR_PATH}/${SERVICE}" \
  --project="$PROJECT" --format='table(tag,version.basename())'

cat <<'EOF'

Both tags are pushed. Nothing is deployed yet: 03-argocd.sh registers the applications,
and Argo CD then converges each environment on whatever its overlay pins:

  gitops/overlays/test   v1.2.0
  gitops/overlays/stage  v1.2.0
  gitops/overlays/prod   v1.1.0   <- the drift the demo finds

Images come first on purpose. Registering the applications before the tags exist just
puts every pod into ImagePullBackOff while you wait.

After 03-argocd.sh, watch it land:

  kubectl get deploy -A -l app.kubernetes.io/name=product-catalog \
    -o custom-columns='NS:.metadata.namespace,IMAGE:.spec.template.spec.containers[0].image'

Next: ./03-argocd.sh
EOF
