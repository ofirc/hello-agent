#!/usr/bin/env bash
# Tear down the local stack: the agent container if it is running, then the kind
# cluster (and with it Argo CD and the three environments).
#
# Leaves alone, deliberately: .env.local and .local/runs (your tokens and the agent's
# evidence), the built images, and the Ollama models. None of those are expensive to
# keep, and all of them are slow or annoying to recreate.
#
# Does not start the Cloud Run agent again. It prints the command instead, because
# bringing back a bot that is connected to a shared Slack app is a decision, not a
# side effect of cleanup.
set -euo pipefail
cd "$(dirname "$0")"

KIND_CLUSTER="sre-local"
COMPOSE_FILE="../../compose.yaml"

command -v kind >/dev/null 2>&1 || { echo "kind is not installed" >&2; exit 1; }
docker info >/dev/null 2>&1 || {
  echo "the docker daemon is not running. Start Docker Desktop and rerun." >&2
  exit 1
}

if [[ -f "$COMPOSE_FILE" ]] && [[ -n "$(docker compose -f "$COMPOSE_FILE" ps -q 2>/dev/null || true)" ]]; then
  echo "==> Stopping the agent container"
  # Ignored on failure: the cluster is what matters here, and a half-stopped compose
  # project is not a reason to leave it running.
  docker compose -f "$COMPOSE_FILE" down || echo "    docker compose down failed, continuing"
else
  echo "==> No agent container running, skipping"
fi

if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
  echo "==> Deleting kind cluster ${KIND_CLUSTER}"
  kind delete cluster --name "$KIND_CLUSTER"
else
  echo "==> kind cluster ${KIND_CLUSTER} not found, skipping"
fi

# Same values the cloud scripts use. If gcloud has no project, say so with a
# placeholder instead of printing a command that cannot work.
if command -v gcloud >/dev/null 2>&1; then
  # shellcheck source=../env.sh
  source ../env.sh
fi

cat <<MSG

The local stack is down. If you stopped the Cloud Run agent, start it again with:

  gcloud beta run instances start ${AGENT_INSTANCE:-sre-agent} --region ${REGION:-us-east1} --project ${PROJECT:-<project>}
MSG
