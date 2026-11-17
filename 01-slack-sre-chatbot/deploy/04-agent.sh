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

# Create it, but distinguish "already exists" from a real failure. Swallowing every
# error with 2>/dev/null||true hides a permissions problem and leaves the next command
# reporting "service account does not exist", which sends you looking in the wrong place.
if ! create_err="$(gcloud iam service-accounts create "$AGENT_SA" \
      --project="$PROJECT" --display-name="SRE ChatOps agent" 2>&1)"; then
  if [[ "$create_err" != *"already exists"* ]]; then
    echo "    could not create the service account:" >&2
    echo "$create_err" | head -3 >&2
    exit 1
  fi
  echo "    already exists, reusing"
fi

# IAM is eventually consistent. A service account is routinely not yet usable in a
# policy binding for a few seconds after it is created, and the binding fails with
# "Service account ... does not exist" even though the create succeeded. Wait for it
# to resolve rather than racing.
echo "    waiting for the service account to propagate"
for i in $(seq 1 20); do
  gcloud iam service-accounts describe "$SA_EMAIL" --project="$PROJECT" >/dev/null 2>&1 && break
  sleep 3
done

# Least privilege: read the secrets it was given, and call Vertex (the model behind
# drift debug and drift fix). Nothing else.
# No GKE access at all, because the agent reaches the cluster only through the Argo CD
# API, with an Argo CD account that can get and sync and nothing more.
for ROLE in roles/secretmanager.secretAccessor roles/aiplatform.user; do
  bound=false
  for i in $(seq 1 10); do
    if gcloud projects add-iam-policy-binding "$PROJECT" \
         --member="serviceAccount:${SA_EMAIL}" --role="$ROLE" \
         --condition=None --quiet >/dev/null 2>&1; then
      bound=true
      break
    fi
    sleep 5
  done
  if [[ "$bound" != true ]]; then
    echo "    could not bind ${ROLE} to ${SA_EMAIL} after retries" >&2
    exit 1
  fi
  echo "    bound ${ROLE}"
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

# gcloud parses --set-env-vars as comma-separated KEY=VALUE pairs, so any value that
# itself contains a comma (ENVIRONMENTS=test,stage,prod) is misread as the start of a
# new pair and fails with "Bad syntax for dict arg: [stage]". The ^DELIM^ prefix picks
# a different separator. Not '@', because JIRA_EMAIL contains one.
ENVVARS="ARGOCD_BASE_URL=${ARGOCD_BASE_URL}"
ENVVARS+="|GITHUB_OWNER=${GITHUB_OWNER:-ofirc}"
ENVVARS+="|GITHUB_REPO=${GITHUB_REPO:-hello-agent}"
ENVVARS+="|GITOPS_PREFIX=01-slack-sre-chatbot/gitops/overlays"
ENVVARS+="|ENVIRONMENTS=${ENVIRONMENTS}"
# Vertex is required, not optional: drift debug and drift fix run the model and have no
# rule-based fallback, so without VERTEX_PROJECT both answer "needs vertex".
ENVVARS+="|VERTEX_PROJECT=${PROJECT}"
ENVVARS+="|VERTEX_LOCATION=${REGION}"
# Only when set, so an empty .env value does not override the agent's default model.
[[ -n "${VERTEX_MODEL:-}" ]] && ENVVARS+="|VERTEX_MODEL=${VERTEX_MODEL}"
# The authorization gate fails closed: without AUTHORIZED_EMAILS, drift debug, drift fix
# and doctor are refused for everyone. Its commas are safe, ENVVARS is |-separated.
if [[ -n "${AUTHORIZED_EMAILS:-}" ]]; then
  ENVVARS+="|AUTHORIZED_EMAILS=${AUTHORIZED_EMAILS}"
else
  echo "    warning: AUTHORIZED_EMAILS is not set in .env, so drift debug, drift fix and doctor will refuse everyone" >&2
fi
[[ -n "${JIRA_BASE_URL:-}"    ]] && ENVVARS+="|JIRA_BASE_URL=${JIRA_BASE_URL}|JIRA_EMAIL=${JIRA_EMAIL:-}"
[[ -n "${GRAFANA_BASE_URL:-}" ]] && ENVVARS+="|GRAFANA_BASE_URL=${GRAFANA_BASE_URL}|GRAFANA_DATASOURCE_UID=${GRAFANA_DATASOURCE_UID:-}"

if gcloud beta run instances describe "$AGENT_INSTANCE" \
     --project="$PROJECT" --region="$REGION" >/dev/null 2>&1; then
  echo "==> Updating the existing instance"
  # Memory is set here as well as on create: an instance made before the sandbox existed
  # has 512Mi, and the sandboxes that run the model's scripts share the instance's memory.
  gcloud beta run instances update "$AGENT_INSTANCE" \
    --project="$PROJECT" --region="$REGION" \
    --image="$AGENT_IMAGE" \
    --sandbox-launcher \
    --memory=1Gi \
    --set-env-vars="^|^$ENVVARS" \
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
  # config behind the commonly quoted price. Instances bill continuously. 1Gi rather
  # than 512Mi because the sandboxes that run the model's scripts share the instance's
  # memory with the agent, so the update branch above sets it too.
  #
  # The sandbox launcher flag installs the sandbox CLI that `drift debug` uses to run
  # model-written code with no network and a read-only filesystem. Without it the agent
  # still starts, /sre doctor shows sandbox as unavailable, and the model is told it
  # cannot run code.
  gcloud beta run instances create "$AGENT_INSTANCE" \
    --project="$PROJECT" \
    --region="$REGION" \
    --image="$AGENT_IMAGE" \
    --sandbox-launcher \
    --ingress=internal \
    --port=8080 \
    --network="$VPC_NETWORK" \
    --subnet="$VPC_SUBNET" \
    --cpu=1 --memory=1Gi \
    --service-account="$SA_EMAIL" \
    --set-env-vars="^|^$ENVVARS" \
    --set-secrets="$SECRETS" \
    --labels="$LABELS"
fi

echo "==> Waiting for the agent to report a Slack connection"
# Query Cloud Logging rather than `instances logs read`. The agent logs structured
# JSON, and `logs read` renders only textPayload, so the ready line comes back as a
# blank line and the grep never matches: the script reports failure while the agent is
# connected and working.
for _ in $(seq 1 24); do
  if gcloud logging read \
       'resource.type="cloud_run_instance" AND resource.labels.instance_name="'"${AGENT_INSTANCE}"'" AND jsonPayload.msg="connected to slack with socket mode"' \
       --project="$PROJECT" --limit=1 --freshness=10m \
       --format='value(jsonPayload.msg)' 2>/dev/null | grep -q .; then
    echo "    connected"
    echo
    echo "The agent is live. Try /sre doctor and /sre drift list in Slack."
    exit 0
  fi
  sleep 5
done

echo "    no connection logged yet. Check with:" >&2
echo "    gcloud beta run instances logs read ${AGENT_INSTANCE} --project=${PROJECT} --region=${REGION}" >&2
echo "    gcloud logging read 'resource.type=\"cloud_run_instance\" AND resource.labels.instance_name=\"${AGENT_INSTANCE}\"' \\" >&2
echo "      --project=${PROJECT} --limit=20 --freshness=10m --format='value(jsonPayload.msg,jsonPayload.err)'" >&2
exit 1
