#!/usr/bin/env bash
# Bring up the local stack: a kind cluster running Argo CD and the three product-catalog
# environments, plus the images and .env.local the agent needs. No GCP resources, no
# registry push.
#
# What this does NOT do is start the agent. That is a second, deliberate step (printed
# at the end), so the agent can be restarted, rebuilt, or run under `go run` without
# touching the cluster.
#
# Safe to re-run. Every step checks before acting and says when it skips:
#
#   ./deploy/local/up.sh
#
# Image builds are the one exception to "skip": they always run, because the agent's
# source may have changed since last time. They are layer-cached, so an unchanged tree
# costs a few seconds, and `kind load` itself skips an image the node already has.
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck source=../lib/argocd.sh
source ../lib/argocd.sh

KIND_CLUSTER="sre-local"
CTX="kind-${KIND_CLUSTER}"
ACCOUNT="sre-agent"

# Where the agent reaches Argo CD from the host. kind.yaml maps node port 30443 to this.
ARGOCD_URL="https://localhost:8443"
ARGOCD_NODE_PORT=30443

# The same image reference gitops/overlays-local pins, so the kubelet finds it already
# loaded and never tries to pull. Deliberately not STACK_IMAGE from env.sh: that follows
# GHCR_OWNER, while the committed overlays do not.
STACK_IMAGE_LOCAL="ghcr.io/ofirc/product-catalog"
TAGS=(v1.1.0 v1.2.0)
AGENT_IMAGE_LOCAL="sre-agent:local"

BASE_MODEL="qwen3:14b"
LOCAL_MODEL="sre-qwen3:14b"
OLLAMA_MODEL="${OLLAMA_MODEL:-$LOCAL_MODEL}"

PROJECT_DIR="$(cd ../.. && pwd -P)"
ENV_FILE="${PROJECT_DIR}/.env.local"
RUNS_DIR="${PROJECT_DIR}/.local/runs"

TMP_FILES=()
cleanup() { [[ ${#TMP_FILES[@]} -eq 0 ]] || rm -f "${TMP_FILES[@]}"; }
trap cleanup EXIT

k() { kubectl --context "$CTX" "$@"; }

# ---------------------------------------------------------------------------
# 1. Preflight. Fail early and clearly on what is required, warn on what is not.
# ---------------------------------------------------------------------------
echo "==> Preflight"
for tool in docker kind kubectl curl python3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "${tool} is not installed" >&2; exit 1; }
done
docker info >/dev/null 2>&1 || {
  echo "the docker daemon is not running. Start Docker Desktop and rerun." >&2
  exit 1
}

if ! command -v ollama >/dev/null 2>&1; then
  echo "    warning: ollama is not installed, so the agent will have no model." >&2
  echo "    install it from https://ollama.com, then: ollama pull ${BASE_MODEL}" >&2
else
  models="$(ollama list 2>/dev/null || true)"
  if [[ -z "$models" ]]; then
    echo "    warning: could not list ollama models. Is the Ollama app running?" >&2
  elif grep -qE "^${LOCAL_MODEL}[[:space:]]" <<<"$models"; then
    echo "    ${LOCAL_MODEL} already exists, skipping"
  elif grep -qE "^${BASE_MODEL}[[:space:]]" <<<"$models"; then
    # Ollama's default context is far smaller than the debug prompt (instructions, skill
    # text, tool schemas, facts, overlays), and its OpenAI-compatible endpoint silently
    # cuts an overlong prompt from the front. The endpoint cannot set the context per
    # request, so the reliable fix is a model variant that carries a bigger one.
    echo "    creating ${LOCAL_MODEL} (${BASE_MODEL} with a 32K context window)"
    modelfile="$(mktemp)"
    TMP_FILES+=("$modelfile")
    printf 'FROM %s\nPARAMETER num_ctx 32768\n' "$BASE_MODEL" >"$modelfile"
    ollama create "$LOCAL_MODEL" -f "$modelfile"
  else
    echo "    warning: ${BASE_MODEL} is not pulled, so the agent will have no model." >&2
    echo "    run: ollama pull ${BASE_MODEL}   (then rerun this script to create ${LOCAL_MODEL})" >&2
  fi
fi

# ---------------------------------------------------------------------------
# 2. One Slack connection. The local agent uses the same Slack app as the Cloud Run
#    one, and Socket Mode lets only one process hold it at a time, so a running cloud
#    agent has to be stopped. Needs gcloud present, authenticated and a project
#    resolved; whenever the outcome is uncertain, warn instead of assuming.
# ---------------------------------------------------------------------------
echo "==> Checking for a running Cloud Run agent"
# True only when we KNOW no cloud agent is left running: the instance is absent, or
# `stop` succeeded. Every other outcome leaves it false, so the warning below prints.
# Guessing "nothing to do" is the dangerous direction: it leaves two agents on one Slack
# app. Stopping when unsure is harmless, so that is the side to err on.
#
# gcloud runs with --quiet and stdin from /dev/null throughout, so a missing beta
# component fails with a message instead of hanging on an install prompt.
cloud_checked=false
cloud_why=""
if command -v gcloud >/dev/null 2>&1 && gcloud auth print-access-token --quiet >/dev/null 2>&1 </dev/null; then
  # shellcheck source=../env.sh
  source ../env.sh
  if [[ -n "$PROJECT" ]]; then
    if describe_err="$(gcloud beta run instances describe "$AGENT_INSTANCE" --quiet \
          --region "$REGION" --project "$PROJECT" --format='value(name)' 2>&1 >/dev/null </dev/null)"; then
      # It exists. Do not try to read its state out of describe: stopping an instance
      # that is already stopped just fails, and that failure is handled below.
      echo "    stopping Cloud Run instance ${AGENT_INSTANCE} in ${PROJECT}/${REGION} (one agent per Slack app)"
      if stop_err="$(gcloud beta run instances stop "$AGENT_INSTANCE" --quiet \
            --region "$REGION" --project "$PROJECT" 2>&1 </dev/null)"; then
        cloud_checked=true
      else
        cloud_why="could not stop ${AGENT_INSTANCE} (it may already be stopped): $(tail -n 1 <<<"$stop_err")"
      fi
    elif grep -qiE 'NOT_FOUND|not found' <<<"$describe_err"; then
      echo "    no Cloud Run instance ${AGENT_INSTANCE} in ${PROJECT}/${REGION}, skipping"
      cloud_checked=true
    else
      cloud_why="could not describe ${AGENT_INSTANCE}: $(tail -n 1 <<<"$describe_err")"
    fi
  else
    cloud_why="no GCP project resolved"
  fi
else
  cloud_why="gcloud is missing or signed out"
fi
if [[ "$cloud_checked" != true ]]; then
  echo "    warning: ${cloud_why}." >&2
  echo "    Only one agent may be connected to the Slack app at a time. If the cloud agent" >&2
  echo "    is running, stop it before starting the local one." >&2
fi

# ---------------------------------------------------------------------------
# 3. Cluster.
# ---------------------------------------------------------------------------
if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
  echo "==> kind cluster ${KIND_CLUSTER} already exists, skipping"
else
  echo "==> Creating kind cluster ${KIND_CLUSTER}"
  kind create cluster --name "$KIND_CLUSTER" --config ./kind.yaml || {
    echo "kind could not create the cluster. If the error mentions port 8443, something" >&2
    echo "else on this machine is already listening there." >&2
    exit 1
  }
fi
kubectl config use-context "$CTX" >/dev/null

# Guard the context, same as the cloud scripts. Every kubectl call below also names the
# context explicitly, but a mismatch here means something is badly wrong.
ctx_now="$(kubectl config current-context 2>/dev/null || true)"
if [[ "$ctx_now" != "$CTX" ]]; then
  echo "kubectl context is '${ctx_now}', not ${CTX}. Refusing to continue." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 4. Argo CD: the same install as 01-gke.sh, published as a NodePort instead of a load
#    balancer. One port serves both sides with no port-forward: https://localhost:8443
#    from this Mac, https://sre-local-control-plane:30443 from containers on kind's
#    Docker network.
# ---------------------------------------------------------------------------
argocd_ready() {
  k -n argocd wait --for=condition=Available --timeout=1s \
    deployment/argocd-server deployment/argocd-repo-server >/dev/null 2>&1
}

if argocd_ready; then
  echo "==> Argo CD already installed and ready, skipping"
else
  echo "==> Installing Argo CD"
  k create namespace argocd --dry-run=client -o yaml | k apply -f -
  # --server-side is required, not a preference. See 01-gke.sh: the ApplicationSet CRD
  # is larger than the 262144-byte last-applied-configuration annotation limit.
  k apply -n argocd --server-side --force-conflicts \
    -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml

  echo "==> Waiting for Argo CD to become ready"
  k rollout status -n argocd deployment/argocd-server --timeout=10m
  k rollout status -n argocd deployment/argocd-repo-server --timeout=10m
fi

svc_type="$(k get svc argocd-server -n argocd -o jsonpath='{.spec.type}' 2>/dev/null || true)"
node_port="$(k get svc argocd-server -n argocd \
  -o jsonpath='{.spec.ports[?(@.name=="https")].nodePort}' 2>/dev/null || true)"
if [[ "$svc_type" == "NodePort" && "$node_port" == "$ARGOCD_NODE_PORT" ]]; then
  echo "==> argocd-server already on NodePort ${ARGOCD_NODE_PORT}, skipping"
else
  echo "==> Exposing argocd-server on NodePort ${ARGOCD_NODE_PORT}"
  # Service ports merge by port number, so this touches only the https entry.
  k patch svc argocd-server -n argocd \
    -p "{\"spec\":{\"type\":\"NodePort\",\"ports\":[{\"name\":\"https\",\"port\":443,\"nodePort\":${ARGOCD_NODE_PORT}}]}}"
fi

wait_for_argocd_api() {
  for _ in $(seq 1 60); do
    curl -sk --max-time 5 "${ARGOCD_URL}/api/version" >/dev/null 2>&1 && return 0
    sleep 2
  done
  echo "Argo CD did not answer at ${ARGOCD_URL}/api/version." >&2
  echo "  Check: kubectl --context ${CTX} get svc argocd-server -n argocd" >&2
  return 1
}
echo "==> Waiting for ${ARGOCD_URL}"
wait_for_argocd_api

# ---------------------------------------------------------------------------
# 5. Images. Built natively (no --platform): kind runs on this machine's own
#    architecture, unlike GKE's amd64 nodes. Loaded straight into the node, so there is
#    no registry, no push and no login.
#
#    The agent image is only built, not loaded into kind: it never runs in the cluster.
#    It stays in this machine's Docker for Compose and for the code runner.
# ---------------------------------------------------------------------------
for TAG in "${TAGS[@]}"; do
  echo "==> Building product-catalog:${TAG}"
  docker build -t "${STACK_IMAGE_LOCAL}:${TAG}" ../../stack/product-catalog
  echo "==> Loading ${STACK_IMAGE_LOCAL}:${TAG} into kind"
  kind load docker-image "${STACK_IMAGE_LOCAL}:${TAG}" --name "$KIND_CLUSTER"
done

echo "==> Building the agent image ${AGENT_IMAGE_LOCAL}"
docker build -t "$AGENT_IMAGE_LOCAL" ../..

# ---------------------------------------------------------------------------
# 6. Applications. Always applied: kubectl apply is idempotent and cheap.
# ---------------------------------------------------------------------------
echo "==> Applying Argo CD applications"
k apply -f ./argocd/

# ---------------------------------------------------------------------------
# 7. The agent's Argo CD account and token, through the helpers shared with
#    03-argocd.sh.
# ---------------------------------------------------------------------------
argocd_ensure_account "$ACCOUNT" "$CTX"
# A restart drops the API for a few seconds, and the token check below needs it.
wait_for_argocd_api

AGENT_TOKEN=""
candidate="$(argocd_existing_token "$ENV_FILE" || true)"
if argocd_token_works "$candidate" "$ARGOCD_URL"; then
  echo "==> Existing ARGOCD_TOKEN in .env.local still valid, keeping it"
  AGENT_TOKEN="$candidate"
elif [[ -n "$candidate" ]]; then
  echo "==> Existing ARGOCD_TOKEN no longer accepted, minting a new one"
fi
if [[ -z "$AGENT_TOKEN" ]]; then
  echo "==> Minting a token for ${ACCOUNT}"
  AGENT_TOKEN="$(argocd_mint_token "$ARGOCD_URL" "$ACCOUNT" "$CTX")"
fi

# ---------------------------------------------------------------------------
# 8. .env.local: Slack and GitHub from .env, plus the local overrides, with host-side
#    addresses so `go run` works as is. Compose overrides the two that differ inside a
#    container.
#
#    Rebuilt from its source on every run rather than edited in place, so a change to
#    .env reaches it. Written to a temp file first so an unchanged result is reported
#    as unchanged and a failure halfway never leaves a half-written file.
# ---------------------------------------------------------------------------
echo "==> Writing ${ENV_FILE}"
new_env="$(mktemp "${ENV_FILE}.XXXXXX")"
TMP_FILES+=("$new_env")
chmod 600 "$new_env"   # it holds the Slack, GitHub and Argo CD tokens
if [[ -f "${PROJECT_DIR}/.env" ]]; then
  cp "${PROJECT_DIR}/.env" "$new_env"
elif [[ -f "$ENV_FILE" ]]; then
  # No .env, but a .env.local someone filled in by hand. Keep their Slack tokens.
  cp "$ENV_FILE" "$new_env"
else
  cp "${PROJECT_DIR}/.env.example" "$new_env"
  echo "    warning: no .env, so .env.local starts from .env.example." >&2
  echo "    Fill in SLACK_BOT_TOKEN, SLACK_APP_TOKEN, GITHUB_APP_* or GITHUB_TOKEN, and AUTHORIZED_EMAILS in .env.local." >&2
fi

# Only the Docker runner opt-in (compose.docker-sandbox.yaml, or SANDBOX=docker with
# go run) uses this folder: it bind-mounts it into code-run containers by host path, so
# it has to be an absolute path that exists. The default shim runner does not need it.
mkdir -p "$RUNS_DIR"
# 1777, like /tmp. With the Docker opt-in the agent container runs as uid 65532, not as
# you, and must be able to create run directories in here. The sticky bit means that each user can only delete
# its own entries, while you, as owner of the directory, can still clear it out. Each
# run directory is created 0700 by the agent, and holds evidence only: no credentials
# ever reach it, so a wide-open parent exposes nothing worth protecting.
chmod 1777 "$RUNS_DIR"

env_file_set "$new_env" \
  "ARGOCD_BASE_URL=${ARGOCD_URL}" \
  "ARGOCD_TOKEN=${AGENT_TOKEN}" \
  "ARGOCD_INSECURE=true" \
  "GITOPS_PREFIX=01-slack-sre-chatbot/gitops/overlays-local" \
  "MODEL_PROVIDER=ollama" \
  "OLLAMA_MODEL=${OLLAMA_MODEL}" \
  "SANDBOX=shim" \
  "SANDBOX_CLI=${PROJECT_DIR}/deploy/local/sandbox-shim" \
  "SANDBOX_IMAGE=${AGENT_IMAGE_LOCAL}" \
  "AGENT_WORKDIR=${RUNS_DIR}" \
  "VERTEX_PROJECT=" \
  "GRAFANA_BASE_URL=" "GRAFANA_TOKEN=" "GRAFANA_DATASOURCE_UID=" \
  "JIRA_BASE_URL=" "JIRA_EMAIL=" "JIRA_TOKEN=" "JIRA_PROJECT="

if [[ -f "$ENV_FILE" ]] && cmp -s "$new_env" "$ENV_FILE"; then
  echo "    unchanged, skipping"
else
  mv "$new_env" "$ENV_FILE"
fi

# ---------------------------------------------------------------------------
# 9. Next steps.
# ---------------------------------------------------------------------------
cat <<EOF

The local stack is up.

  cluster       ${CTX}
  Argo CD       ${ARGOCD_URL}   (user admin; password below)
  env file      ${ENV_FILE}

Run the agent, from 01-slack-sre-chatbot/, in a container:

  docker compose up --build

Code runs then use the shim: the same runner code as Cloud Run, with NO isolation.
For isolated code runs through the Docker runner instead:

  docker compose -f compose.yaml -f compose.docker-sandbox.yaml up --build

or run the agent directly on this machine (shim too):

  set -a && source .env.local && set +a && go run ./cmd/agent

The Argo CD UI admin password:

  kubectl --context ${CTX} -n argocd get secret argocd-initial-admin-secret \\
    -o jsonpath='{.data.password}' | base64 -d

Tear it all down with ./deploy/local/down.sh
EOF
