#!/usr/bin/env bash
# Step 2 of 4: build and push the two versions of product-catalog the demo needs.
#
# v1.1.0 and v1.2.0 are the same source. Only the baked-in APP_VERSION differs, and the
# filters feature is gated on it. That is enough to reproduce the opening scenario: a
# customer reports a missing feature, nothing is crashing, prod is one release behind.
#
# Images go to GHCR rather than Artifact Registry. The image reference has to be
# committed to this repo, because Argo CD renders the overlays from GitHub rather than
# from your laptop, and an Artifact Registry path would publish a cloud project id.
# A GHCR path contains only a GitHub owner, which is already public here.
#
# The agent's own image still goes to Artifact Registry in 04-agent.sh. That reference
# is passed to Cloud Run as a flag and never committed, so it leaks nothing.
#
# No gcloud needed in this step at all.
set -euo pipefail
cd "$(dirname "$0")"
source ./env.sh

SERVICE=product-catalog
SRC="../stack/${SERVICE}"
TAGS=(v1.1.0 v1.2.0)

command -v docker >/dev/null 2>&1 || { echo "docker is not installed" >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "the docker daemon is not running" >&2; exit 1; }

echo "==> Logging in to ghcr.io as ${GHCR_OWNER}"
# Reuse the gh CLI token so there is no second credential to manage. It needs the
# write:packages scope.
if ! gh auth token 2>/dev/null | docker login ghcr.io -u "$GHCR_OWNER" --password-stdin >/dev/null 2>&1; then
  cat >&2 <<EOF
Could not log in to ghcr.io.

Most likely the gh token lacks the write:packages scope:

  gh auth refresh -h github.com -s write:packages

Also confirm the active account is ${GHCR_OWNER}:

  gh auth status
EOF
  exit 1
fi

for TAG in "${TAGS[@]}"; do
  echo "==> Building ${SERVICE}:${TAG} for ${BUILD_PLATFORM}"
  # --platform is load-bearing on an Apple Silicon machine. The default would be arm64
  # and the pods would crash-loop with "exec format error" on amd64 nodes, after a pull
  # that looked perfectly healthy.
  #
  # --provenance=false keeps this a plain image manifest rather than an OCI index with
  # an attestation attached, which is what you want when a kubelet is the only consumer.
  docker build \
    --platform="$BUILD_PLATFORM" \
    --provenance=false \
    -t "${STACK_IMAGE}:${TAG}" \
    "$SRC"

  echo "==> Pushing ${STACK_IMAGE}:${TAG}"
  docker push "${STACK_IMAGE}:${TAG}"
done

echo "==> Verifying the pushed architecture"
for TAG in "${TAGS[@]}"; do
  arch="$(docker image inspect "${STACK_IMAGE}:${TAG}" --format '{{.Os}}/{{.Architecture}}' 2>/dev/null || echo unknown)"
  echo "    ${TAG}  ${arch}"
  if [[ "$arch" != "$BUILD_PLATFORM" ]]; then
    echo "    expected ${BUILD_PLATFORM}, these pods would not start" >&2
    exit 1
  fi
done

cat <<EOF

Both tags are pushed to ${STACK_IMAGE}.

  gitops/overlays/test   v1.2.0
  gitops/overlays/stage  v1.2.0
  gitops/overlays/prod   v1.1.0   <- the drift the demo finds

The overlays reference this image by name already, so there is nothing to substitute
and nothing to commit before ./03-argocd.sh.

One manual step, first run only: a new GHCR package is private and the cluster pulls
anonymously. Make it public or every pod sits in ImagePullBackOff.

  https://github.com/users/${GHCR_OWNER}/packages/container/${SERVICE}/settings
  -> Danger Zone -> Change visibility -> Public

Verify anonymously, which is what the kubelet does:

  docker logout ghcr.io >/dev/null 2>&1
  docker manifest inspect ${STACK_IMAGE}:v1.1.0 >/dev/null && echo public

Next: ./03-argocd.sh
EOF
