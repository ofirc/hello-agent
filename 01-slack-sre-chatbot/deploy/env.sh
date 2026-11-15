#!/usr/bin/env bash
# Single source of truth for every deploy script. Source it, do not copy values out.
#
# Override anything by exporting it first:
#   PROJECT=my-project REGION=us-east1 ./01-gke.sh
#
# One region for everything. Cloud Run instances are the constraint: they are available
# in a subset of regions, and the rest of the stack (GKE, Artifact Registry, Cloud
# Build, Vertex) is available far more widely, so the others follow. Co-locating also
# keeps image pulls and the agent's Argo CD calls in-region.

# Defaults to whatever your gcloud CLI is pointed at, so there is no project id baked
# into the repo.
export PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
export REGION="${REGION:-us-east1}"
export ZONE="${ZONE:-${REGION}-b}"

export AR_REPO="${AR_REPO:-stack}"
export AR_HOST="${REGION}-docker.pkg.dev"
export AR_PATH="${AR_HOST}/${PROJECT}/${AR_REPO}"

# Demo workload images live on GHCR, not Artifact Registry.
#
# Not a preference: an Artifact Registry path embeds the project id, and the image
# reference has to be committed to this repo because Argo CD renders the overlays from
# GitHub. GHCR paths contain only a GitHub owner, which is already public here, so the
# repo stays free of anyone's cloud account.
#
# The agent's own image still goes to Artifact Registry (04-agent.sh). That reference
# is passed to Cloud Run as a flag and never committed, so it leaks nothing.
export GHCR_OWNER="${GHCR_OWNER:-ofirc}"
export STACK_IMAGE="${STACK_IMAGE:-ghcr.io/${GHCR_OWNER}/product-catalog}"

# GKE nodes are linux/amd64. This Mac is arm64, and a plain `docker build` would push
# an arm64 image that dies on the cluster with "exec format error", after a pull that
# looked perfectly healthy.
export BUILD_PLATFORM="${BUILD_PLATFORM:-linux/amd64}"

export CLUSTER="${CLUSTER:-sre-demo}"

# Direct VPC egress for the agent instance. The Argo CD API is published on an internal
# load balancer inside this network, so the agent has to be attached to it to reach the
# cluster at all.
export VPC_NETWORK="${VPC_NETWORK:-default}"
export VPC_SUBNET="${VPC_SUBNET:-default}"

export AGENT_INSTANCE="${AGENT_INSTANCE:-sre-agent}"
export AGENT_SA="${AGENT_SA:-sre-agent}"
export AGENT_IMAGE="${AGENT_IMAGE:-${AR_PATH}/sre-agent:v1}"

# Labels on every resource this repo creates, so a demo is easy to find and clean up
# later. GCP label values are lowercase alphanumerics and dashes only.
export LABELS="${LABELS:-owner=$(id -un | tr '[:upper:]_.' '[:lower:]--'),app=sre-agent-demo}"

export ENVIRONMENTS="${ENVIRONMENTS:-test,stage,prod}"

# Cloud Build staging bucket.
#
# `gcloud builds submit` otherwise invents gs://<project>_cloudbuild in the multi-region
# US, which fails outright in any project that restricts resource locations:
#
#   HTTPError 412: 'us' violates constraint 'constraints/gcp.resourceLocations'
#
# Pinning it to REGION also keeps the build source in the same region as the registry
# and the cluster, which is the point of the single-region setup.
export BUILD_BUCKET="${BUILD_BUCKET:-${PROJECT}-cloudbuild-${REGION}}"

# Idempotent. Both 02-stack.sh and 04-agent.sh call this before building.
ensure_build_bucket() {
  if gcloud storage buckets describe "gs://${BUILD_BUCKET}" \
       --project="$PROJECT" >/dev/null 2>&1; then
    return 0
  fi
  echo "==> Creating regional Cloud Build staging bucket gs://${BUILD_BUCKET}"
  gcloud storage buckets create "gs://${BUILD_BUCKET}" \
    --project="$PROJECT" \
    --location="$REGION" \
    --uniform-bucket-level-access \
    --public-access-prevention \
    --default-storage-class=STANDARD
}

# build_image <source-dir> <full-image-tag>
#
# Submits with --async and polls, rather than letting gcloud stream the build log.
#
# Streaming requires project Viewer/Owner to read Cloud Build's default logs bucket,
# and when gcloud cannot read it the command exits non-zero *even though the build
# succeeded*. With `set -e` that silently aborts the rest of the script, which is a
# miserable thing to debug: the image is in the registry but the script says it failed.
#
# Polling the build's own status is the only signal that actually means "the image was
# built", so that is what this waits on.
build_image() {
  local src="$1" tag="$2" id status
  id="$(gcloud builds submit "$src" \
        --project="$PROJECT" \
        --region="$REGION" \
        --gcs-source-staging-dir="gs://${BUILD_BUCKET}/source" \
        --tag="$tag" \
        --async \
        --format='value(id)' 2>/dev/null)"
  if [[ -z "$id" ]]; then
    echo "    could not submit the build" >&2
    return 1
  fi
  echo "    build ${id} submitted, polling"

  for _ in $(seq 1 120); do   # 20 minutes at 10s
    status="$(gcloud builds describe "$id" --project="$PROJECT" --region="$REGION" \
              --format='value(status)' 2>/dev/null || true)"
    case "$status" in
      SUCCESS)
        echo "    SUCCESS  ${tag}"
        return 0 ;;
      FAILURE|TIMEOUT|CANCELLED|EXPIRED|INTERNAL_ERROR)
        echo "    build ${status}" >&2
        echo "    https://console.cloud.google.com/cloud-build/builds;region=${REGION}/${id}?project=${PROJECT}" >&2
        return 1 ;;
    esac
    sleep 10
  done
  echo "    timed out waiting for build ${id}" >&2
  return 1
}

# Fail loudly rather than deploying into whatever project happened to be configured.
require_auth() {
  if [[ -z "$PROJECT" ]]; then
    echo "No GCP project set." >&2
    echo "Run: gcloud config set project <your-project-id>   (or export PROJECT=...)" >&2
    return 1
  fi
  local current
  current="$(gcloud config get-value project 2>/dev/null)"
  if [[ "$current" != "$PROJECT" ]]; then
    echo "gcloud project is '$current', expected '$PROJECT'." >&2
    echo "Run: gcloud config set project $PROJECT" >&2
    return 1
  fi
  if ! gcloud auth print-access-token >/dev/null 2>&1; then
    echo "gcloud credentials are expired. Run: gcloud auth login --update-adc" >&2
    return 1
  fi
}
