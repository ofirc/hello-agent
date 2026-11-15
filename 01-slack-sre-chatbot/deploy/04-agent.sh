#!/usr/bin/env bash
# Step 4 of 4: build the agent image and run it as a Cloud Run instance.
#
# A Cloud Run *instance*, not a service. The agent holds a WebSocket open and keeps
# state between turns, which is the one workload shape a scale-to-zero,
# request-driven service cannot host.
set -euo pipefail
cd "$(dirname "$0")"
source ./env.sh
require_auth

# Read ../.env automatically rather than making you remember to source it. 03-argocd.sh
# writes ARGOCD_BASE_URL and ARGOCD_TOKEN there, so the two scripts chain without a
# copy-paste step in between.
ENV_FILE="../.env"
if [[ -f "$ENV_FILE" ]]; then
  echo "==> Loading $ENV_FILE"
  set -a
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  set +a
fi

need() { # need <VAR> <where it comes from>
  if [[ -z "${!1:-}" ]]; then
    echo "Missing $1." >&2
    echo "  $2" >&2
    exit 1
  fi
}
need SLACK_BOT_TOKEN  "Slack app -> Install App -> Bot User OAuth Token (xoxb-...). Put it in .env (step 2)."
need SLACK_APP_TOKEN  "Slack app -> Basic Information -> App-Level Tokens (xapp-...). Put it in .env (step 2)."
need GITHUB_TOKEN     "A fine-grained PAT with Contents and Pull requests read/write. Put it in .env (step 2)."
need ARGOCD_BASE_URL  "Run ./03-argocd.sh first. It writes this into .env for you."
need ARGOCD_TOKEN     "Run ./03-argocd.sh first. It writes this into .env for you."

if [[ "$ARGOCD_BASE_URL" == *localhost* || "$ARGOCD_BASE_URL" == *127.0.0.1* ]]; then
  echo "ARGOCD_BASE_URL points at localhost, which means a kubectl port-forward." >&2
  echo "That works for 'go run ./cmd/agent' on your laptop but not for Cloud Run." >&2
  echo "Rerun ./03-argocd.sh to get the internal load balancer address." >&2
  exit 1
fi

# instances ssh shells out to alpha even though the command lives in beta. Installing
# both up front avoids an interactive prompt in the middle of a later command.
gcloud components install beta alpha --quiet >/dev/null 2>&1 || true

echo "==> Secrets"
put_secret() {
  local name="$1" value="$2"
  if gcloud secrets describe "$name" --project="$PROJECT" >/dev/null 2>&1; then
    printf '%s' "$value" | gcloud secrets versions add "$name" --project="$PROJECT" --data-file=-
  else
    printf '%s' "$value" | gcloud secrets create "$name" --project="$PROJECT" \
      --data-file=- --replication-policy=automatic --labels="$LABELS"
  fi
}
put_secret slack-bot-token "$SLACK_BOT_TOKEN"
put_secret slack-app-token "$SLACK_APP_TOKEN"
put_secret argocd-token    "$ARGOCD_TOKEN"
put_secret github-token    "$GITHUB_TOKEN"
[[ -n "${JIRA_TOKEN:-}"    ]] && put_secret jira-token    "$JIRA_TOKEN"
[[ -n "${GRAFANA_TOKEN:-}" ]] && put_secret grafana-token "$GRAFANA_TOKEN"

echo "==> Service account"
SA_EMAIL="${AGENT_SA}@${PROJECT}.iam.gserviceaccount.com"
gcloud iam service-accounts create "$AGENT_SA" \
  --project="$PROJECT" --display-name="SRE ChatOps agent" 2>/dev/null || true

# Least privilege: read the secrets it was given, and call Vertex. Nothing else.
# No GKE access at all, because the agent reaches the cluster only through the Argo CD
# API, with an Argo CD account that can get and sync and nothing more.
for ROLE in roles/secretmanager.secretAccessor roles/aiplatform.user; do
  gcloud projects add-iam-policy-binding "$PROJECT" \
    --member="serviceAccount:${SA_EMAIL}" --role="$ROLE" \
    --condition=None --quiet >/dev/null
done

echo "==> Building the agent image"
ensure_build_bucket
build_image .. "$AGENT_IMAGE"

SECRETS="SLACK_BOT_TOKEN=slack-bot-token:latest"
SECRETS+=",SLACK_APP_TOKEN=slack-app-token:latest"
SECRETS+=",ARGOCD_TOKEN=argocd-token:latest"
SECRETS+=",GITHUB_TOKEN=github-token:latest"
[[ -n "${JIRA_TOKEN:-}"    ]] && SECRETS+=",JIRA_TOKEN=jira-token:latest"
[[ -n "${GRAFANA_TOKEN:-}" ]] && SECRETS+=",GRAFANA_TOKEN=grafana-token:latest"

ENVVARS="ARGOCD_BASE_URL=${ARGOCD_BASE_URL}"
ENVVARS+=",GITHUB_OWNER=${GITHUB_OWNER:-ofirc}"
ENVVARS+=",GITHUB_REPO=${GITHUB_REPO:-hello-agent}"
ENVVARS+=",GITOPS_PREFIX=01-slack-sre-chatbot/gitops/overlays"
ENVVARS+=",ENVIRONMENTS=${ENVIRONMENTS}"
ENVVARS+=",VERTEX_PROJECT=${PROJECT}"
ENVVARS+=",VERTEX_LOCATION=${REGION}"
[[ -n "${JIRA_BASE_URL:-}"    ]] && ENVVARS+=",JIRA_BASE_URL=${JIRA_BASE_URL},JIRA_EMAIL=${JIRA_EMAIL:-}"
[[ -n "${GRAFANA_BASE_URL:-}" ]] && ENVVARS+=",GRAFANA_BASE_URL=${GRAFANA_BASE_URL},GRAFANA_DATASOURCE_UID=${GRAFANA_DATASOURCE_UID:-}"

if gcloud beta run instances describe "$AGENT_INSTANCE" \
     --project="$PROJECT" --region="$REGION" >/dev/null 2>&1; then
  echo "==> Updating the existing instance"
  gcloud beta run instances update "$AGENT_INSTANCE" \
    --project="$PROJECT" --region="$REGION" \
    --image="$AGENT_IMAGE" \
    --set-env-vars="$ENVVARS" \
    --set-secrets="$SECRETS"
else
  echo "==> Creating the instance"
  # --ingress=internal because nothing should be able to reach this instance. The agent
  # only dials out, so accepting inbound traffic would be pure attack surface. Many
  # organisations disallow the default of 'all' outright.
  #
  # --network/--subnet give the instance Direct VPC egress, which is what lets it reach
  # the Argo CD internal load balancer from 03-argocd.sh. Without them the instance has
  # only public internet egress and every Argo CD call times out, which looks exactly
  # like a wrong token.
  #
  # --cpu/--memory override the 2 vCPU / 2048Mi default, which is roughly double the
  # config behind the commonly quoted price. Instances bill continuously.
  gcloud beta run instances create "$AGENT_INSTANCE" \
    --project="$PROJECT" \
    --region="$REGION" \
    --image="$AGENT_IMAGE" \
    --ingress=internal \
    --network="$VPC_NETWORK" \
    --subnet="$VPC_SUBNET" \
    --cpu=1 --memory=512Mi \
    --service-account="$SA_EMAIL" \
    --set-env-vars="$ENVVARS" \
    --set-secrets="$SECRETS" \
    --labels="$LABELS"
fi

echo "==> Waiting for the agent to report a Slack connection"
for _ in $(seq 1 20); do
  if gcloud beta run instances logs read "$AGENT_INSTANCE" \
       --project="$PROJECT" --region="$REGION" 2>/dev/null \
       | grep -q "connected to slack with socket mode"; then
    echo "    connected"
    exit 0
  fi
  sleep 5
done

echo "    no connection logged yet. Check with:" >&2
echo "    gcloud beta run instances logs read $AGENT_INSTANCE --project=$PROJECT --region=$REGION" >&2
exit 1
