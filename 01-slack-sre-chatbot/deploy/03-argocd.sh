#!/usr/bin/env bash
# Step 3 of 4: register the Argo CD applications, expose the API inside the VPC, and
# mint the agent's own Argo CD token.
#
# Safe to re-run. Every phase checks before acting, so this is also the command for
# "I edited an Application manifest, make the cluster match":
#
#   ./03-argocd.sh                 converge everything, skipping what is already done
#   ./03-argocd.sh --apps-only     re-apply the Applications and stop (seconds)
#   ./03-argocd.sh --rotate-token  force a fresh Argo CD token even if the old one works
#
# Why --apps-only exists. Argo CD reads the *overlays* from GitHub but the
# *Applications* from the cluster. Only the first of those changes when you push, so
# editing an Application manifest and pushing does nothing until it is re-applied.
# That gap once left a stale kustomize image override running against a repo that no
# longer had one, which silently deployed :latest to every environment.
#
# No argocd CLI needed: everything here is kubectl plus the Argo CD REST API.
set -euo pipefail
cd "$(dirname "$0")"
source ./env.sh
# The account, token and env-file logic is shared with local/up.sh. What stays here is
# what is specific to GKE: the internal load balancer and the port-forward.
source ./lib/argocd.sh
require_auth

ENV_FILE="../.env"
ACCOUNT="sre-agent"
PF_PORT=18080
PF_PID=""

APPS_ONLY=false
ROTATE_TOKEN=false
for arg in "$@"; do
  case "$arg" in
    --apps-only)    APPS_ONLY=true ;;
    --rotate-token) ROTATE_TOKEN=true ;;
    *) echo "Unknown flag: $arg" >&2; exit 2 ;;
  esac
done

# shellcheck disable=SC2015  # `|| true` is the point: cleanup must never fail the exit trap
cleanup() { [[ -n "$PF_PID" ]] && kill "$PF_PID" 2>/dev/null || true; }
trap cleanup EXIT

if ! kubectl get ns argocd >/dev/null 2>&1; then
  echo "Argo CD is not installed. Run ./01-gke.sh first." >&2
  exit 1
fi

# Guard the context. These projects routinely hold other people's clusters, and
# kubectl acts on whatever context is current rather than on $CLUSTER.
ctx="$(kubectl config current-context 2>/dev/null || true)"
if [[ "$ctx" != *"${CLUSTER}"* ]]; then
  echo "kubectl context is '${ctx}', not ${CLUSTER}. Refusing to act." >&2
  echo "  gcloud container clusters get-credentials ${CLUSTER} \\" >&2
  echo "    --project=${PROJECT} --region=${REGION} --dns-endpoint" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Applications. Always applied: kubectl apply is idempotent by nature, it is the cheap
# part, and it is the entire point of --apps-only.
# ---------------------------------------------------------------------------
echo "==> Applying Argo CD applications"
kubectl apply -f ./argocd/

if [[ "$APPS_ONLY" == true ]]; then
  echo
  kubectl get applications -n argocd
  exit 0
fi

# ---------------------------------------------------------------------------
# Internal load balancer for the Argo CD API, so the Cloud Run agent can reach it over
# Direct VPC egress without the API being on the internet.
# ---------------------------------------------------------------------------
svc_type="$(kubectl get svc argocd-server -n argocd -o jsonpath='{.spec.type}' 2>/dev/null || true)"
lb_kind="$(kubectl get svc argocd-server -n argocd \
  -o jsonpath='{.metadata.annotations.networking\.gke\.io/load-balancer-type}' 2>/dev/null || true)"
ARGOCD_IP="$(kubectl get svc argocd-server -n argocd \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"

if [[ "$svc_type" == "LoadBalancer" && "$lb_kind" == "Internal" && -n "$ARGOCD_IP" ]]; then
  echo "==> Internal load balancer already present (${ARGOCD_IP}), skipping"
else
  echo "==> Exposing argocd-server on an internal load balancer"
  kubectl annotate svc argocd-server -n argocd \
    networking.gke.io/load-balancer-type=Internal --overwrite
  kubectl patch svc argocd-server -n argocd -p '{"spec":{"type":"LoadBalancer"}}'

  echo "==> Waiting for the load balancer address (up to 3 minutes)"
  ARGOCD_IP=""
  for _ in $(seq 1 36); do
    ARGOCD_IP="$(kubectl get svc argocd-server -n argocd \
      -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    [[ -n "$ARGOCD_IP" ]] && break
    sleep 5
  done
  if [[ -z "$ARGOCD_IP" ]]; then
    echo "No load balancer IP yet. Check: kubectl get svc argocd-server -n argocd" >&2
    exit 1
  fi
  echo "    https://${ARGOCD_IP}"
fi

# ---------------------------------------------------------------------------
# A dedicated Argo CD account, so the agent's actions are attributable in the audit
# log and its token is revocable without touching admin.
# ---------------------------------------------------------------------------
argocd_ensure_account "$ACCOUNT" "$ctx"

start_port_forward() {
  [[ -n "$PF_PID" ]] && return 0
  kubectl port-forward -n argocd svc/argocd-server "${PF_PORT}:443" >/dev/null 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 20); do
    curl -sk "https://localhost:${PF_PORT}/api/version" >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "Could not reach argocd-server through the port-forward." >&2
  return 1
}

AGENT_TOKEN=""
start_port_forward

if [[ "$ROTATE_TOKEN" != true ]]; then
  candidate="$(argocd_existing_token "$ENV_FILE" || true)"
  if argocd_token_works "$candidate" "https://localhost:${PF_PORT}"; then
    echo "==> Existing ARGOCD_TOKEN still valid, keeping it"
    AGENT_TOKEN="$candidate"
  elif [[ -n "$candidate" ]]; then
    echo "==> Existing ARGOCD_TOKEN no longer accepted, minting a new one"
  fi
fi

if [[ -z "$AGENT_TOKEN" ]]; then
  echo "==> Minting a token for ${ACCOUNT}"
  AGENT_TOKEN="$(argocd_mint_token "https://localhost:${PF_PORT}" "$ACCOUNT" "$ctx")"
fi

echo "==> Writing ARGOCD_* into ${ENV_FILE}"
[[ -f "$ENV_FILE" ]] || cp ../.env.example "$ENV_FILE"
env_file_set "$ENV_FILE" "ARGOCD_BASE_URL=https://${ARGOCD_IP}" "ARGOCD_TOKEN=${AGENT_TOKEN}"

cat <<EOF

Argo CD is wired up.

  ARGOCD_BASE_URL  https://${ARGOCD_IP}   (private, VPC only)
  ARGOCD_TOKEN     in .env                (account: ${ACCOUNT}, get + sync only)

That address is not reachable from your laptop. To run the agent locally, port-forward
and point it at localhost instead:

  kubectl port-forward -n argocd svc/argocd-server 8080:443 &
  ARGOCD_BASE_URL=https://localhost:8080 go run ./cmd/agent

The Argo CD UI, same forward, user admin:

  kubectl -n argocd get secret argocd-initial-admin-secret \\
    -o jsonpath='{.data.password}' | base64 -d

After editing an Application manifest, this is the cheap way to make the cluster match:

  ./03-argocd.sh --apps-only

Next: ./04-agent.sh
EOF
