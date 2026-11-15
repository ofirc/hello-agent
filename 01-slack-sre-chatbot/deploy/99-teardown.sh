#!/usr/bin/env bash
# Tear the whole demo back down.
#
# Numbered 99 rather than 05 on purpose: this is not the next step in the sequence, it
# is the one that undoes all four of them. 04-agent.sh is the last step of the build.
#
# Deletes, in an order that matters:
#
#   1. the Argo CD LoadBalancer Service   (before the cluster, see below)
#   2. the Cloud Run instance
#   3. the GKE cluster
#   4. the Secret Manager secrets
#   5. the service account and its IAM bindings
#   6. the Artifact Registry repo and every image in it
#
# Leaves alone, deliberately: enabled APIs, Cloud NAT and the VPC. None of those were
# created by this demo, and disabling an API or deleting a NAT can break unrelated
# workloads in the same project.
#
# Usage:
#   ./99-teardown.sh                  interactive, asks for confirmation
#   ./99-teardown.sh --yes            no prompt, for CI
#   ./99-teardown.sh --keep-secrets   leave the Slack and GitHub tokens in place
#   ./99-teardown.sh --keep-registry  leave the images, they are slow to rebuild
set -euo pipefail
cd "$(dirname "$0")"
source ./env.sh
require_auth

ASSUME_YES=false
KEEP_SECRETS=false
KEEP_REGISTRY=false
for arg in "$@"; do
  case "$arg" in
    --yes|-y)        ASSUME_YES=true ;;
    --keep-secrets)  KEEP_SECRETS=true ;;
    --keep-registry) KEEP_REGISTRY=true ;;
    *) echo "Unknown flag: $arg" >&2; exit 2 ;;
  esac
done

SA_EMAIL="${AGENT_SA}@${PROJECT}.iam.gserviceaccount.com"
SECRETS=(slack-bot-token slack-app-token argocd-token github-token jira-token grafana-token)

# ---------------------------------------------------------------------------
# Show what actually exists before destroying anything. An inventory built by
# querying beats one built from assumptions, and it makes the confirmation prompt
# something you can actually reason about.
# ---------------------------------------------------------------------------
echo "Project: ${PROJECT}    Region: ${REGION}"
echo
echo "This will delete:"

exists_cluster=false
if gcloud container clusters describe "$CLUSTER" \
     --project="$PROJECT" --region="$REGION" >/dev/null 2>&1; then
  exists_cluster=true
  echo "  [x] GKE cluster            ${CLUSTER}"
else
  echo "  [ ] GKE cluster            ${CLUSTER} (absent)"
fi

exists_instance=false
if gcloud beta run instances describe "$AGENT_INSTANCE" \
     --project="$PROJECT" --region="$REGION" >/dev/null 2>&1; then
  exists_instance=true
  echo "  [x] Cloud Run instance     ${AGENT_INSTANCE}"
else
  echo "  [ ] Cloud Run instance     ${AGENT_INSTANCE} (absent)"
fi

if [[ "$KEEP_REGISTRY" == true ]]; then
  echo "  [ ] Artifact Registry      ${AR_REPO} (kept, --keep-registry)"
elif gcloud artifacts repositories describe "$AR_REPO" \
       --project="$PROJECT" --location="$REGION" >/dev/null 2>&1; then
  echo "  [x] Artifact Registry      ${AR_REPO} and all images in it"
else
  echo "  [ ] Artifact Registry      ${AR_REPO} (absent)"
fi

if [[ "$KEEP_SECRETS" == true ]]; then
  echo "  [ ] Secret Manager         (kept, --keep-secrets)"
else
  found=()
  for s in "${SECRETS[@]}"; do
    gcloud secrets describe "$s" --project="$PROJECT" >/dev/null 2>&1 && found+=("$s")
  done
  if (( ${#found[@]} )); then
    echo "  [x] Secret Manager         ${found[*]}"
  else
    echo "  [ ] Secret Manager         (none present)"
  fi
fi

if gcloud iam service-accounts describe "$SA_EMAIL" --project="$PROJECT" >/dev/null 2>&1; then
  echo "  [x] Service account        ${SA_EMAIL}"
else
  echo "  [ ] Service account        ${SA_EMAIL} (absent)"
fi

echo
echo "Left alone: enabled APIs, Cloud NAT, VPC. This demo did not create them."
echo

if [[ "$ASSUME_YES" != true ]]; then
  if [[ ! -t 0 ]]; then
    echo "Not a terminal. Re-run with --yes if you mean it." >&2
    exit 1
  fi
  # Typing the project id, rather than y/N. A reflexive "y" into the wrong terminal is
  # exactly how someone deletes a cluster they meant to keep.
  read -r -p "Type the project id to confirm: " reply
  if [[ "$reply" != "$PROJECT" ]]; then
    echo "Did not match. Nothing deleted."
    exit 1
  fi
fi

echo

# ---------------------------------------------------------------------------
# 1. The LoadBalancer Service, before the cluster.
#
# 03-argocd.sh turned argocd-server into a LoadBalancer, and GKE created a forwarding
# rule, a backend service and firewall rules in the project to back it. Deleting the
# cluster outright can orphan those: they survive, they keep costing money, and they
# are invisible unless you go looking in the networking console. Deleting the Service
# first lets the GKE controller clean up its own resources while it still exists.
# ---------------------------------------------------------------------------
if [[ "$exists_cluster" == true ]]; then
  echo "==> Removing the Argo CD load balancer before the cluster"

  # Guard the kubectl context. These projects routinely hold other people's clusters,
  # and kubectl acts on whatever context happens to be current, not on $CLUSTER. The
  # next command deletes a Service in the argocd namespace, which would be a genuinely
  # bad thing to run against someone else's cluster.
  ctx="$(kubectl config current-context 2>/dev/null || true)"
  if [[ "$ctx" != *"${CLUSTER}"* ]]; then
    echo "    kubectl context is '${ctx}', which is not ${CLUSTER}." >&2
    echo "    Refusing to delete Services. Point kubectl at the right cluster with:" >&2
    echo "      gcloud container clusters get-credentials ${CLUSTER} \\" >&2
    echo "        --project=${PROJECT} --region=${REGION} --dns-endpoint" >&2
    exit 1
  fi

  if kubectl get svc argocd-server -n argocd >/dev/null 2>&1; then
    kubectl delete svc argocd-server -n argocd --wait=true --timeout=5m || true
    # The forwarding rule is torn down asynchronously, so give the controller a moment
    # before pulling the cluster out from under it.
    sleep 20
  else
    echo "    no argocd-server Service, or kubectl is pointed elsewhere"
  fi
fi

if [[ "$exists_instance" == true ]]; then
  echo "==> Deleting Cloud Run instance ${AGENT_INSTANCE}"
  gcloud beta run instances delete "$AGENT_INSTANCE" \
    --project="$PROJECT" --region="$REGION" --quiet
fi

if [[ "$exists_cluster" == true ]]; then
  echo "==> Deleting GKE cluster ${CLUSTER} (several minutes)"
  gcloud container clusters delete "$CLUSTER" \
    --project="$PROJECT" --region="$REGION" --quiet
  kubectl config delete-context "gke_${PROJECT}_${REGION}_${CLUSTER}" >/dev/null 2>&1 || true
fi

if [[ "$KEEP_SECRETS" != true ]]; then
  echo "==> Deleting secrets"
  for s in "${SECRETS[@]}"; do
    gcloud secrets delete "$s" --project="$PROJECT" --quiet 2>/dev/null \
      && echo "    deleted $s" || true
  done
fi

if gcloud iam service-accounts describe "$SA_EMAIL" --project="$PROJECT" >/dev/null 2>&1; then
  echo "==> Removing IAM bindings and the service account"
  # Bindings first. Deleting the account leaves its bindings behind as orphaned
  # deleted:serviceAccount: entries that clutter the policy forever.
  for role in roles/secretmanager.secretAccessor roles/aiplatform.user; do
    gcloud projects remove-iam-policy-binding "$PROJECT" \
      --member="serviceAccount:${SA_EMAIL}" --role="$role" \
      --condition=None --quiet >/dev/null 2>&1 || true
  done
  gcloud iam service-accounts delete "$SA_EMAIL" --project="$PROJECT" --quiet
fi

if [[ "$KEEP_REGISTRY" != true ]]; then
  if gcloud artifacts repositories describe "$AR_REPO" \
       --project="$PROJECT" --location="$REGION" >/dev/null 2>&1; then
    echo "==> Deleting Artifact Registry repo ${AR_REPO}"
    gcloud artifacts repositories delete "$AR_REPO" \
      --project="$PROJECT" --location="$REGION" --quiet
  fi
fi

# ---------------------------------------------------------------------------
# Orphan check. GKE load balancers are the thing that most often survives a cluster
# delete, so look rather than assume.
# ---------------------------------------------------------------------------
echo
echo "==> Checking for orphaned load balancer resources"
orphans="$(gcloud compute forwarding-rules list --project="$PROJECT" \
  --filter="region:${REGION}" --format='value(name)' 2>/dev/null \
  | grep -i -E "^a[0-9a-f]{31}$|${CLUSTER}" || true)"
if [[ -n "$orphans" ]]; then
  echo "    Possible leftovers, check and delete by hand:"
  echo "$orphans" | sed 's/^/      /'
  echo "      gcloud compute forwarding-rules delete <name> --region=${REGION}"
else
  echo "    none found"
fi

cat <<EOF

Done. Still present on purpose:

  - Enabled APIs (container, artifactregistry, cloudbuild, secretmanager, aiplatform)
  - Cloud NAT and the VPC
  - Cloud Build staging bucket gs://${PROJECT}_cloudbuild

On the Slack side, nothing here touched your app. To finish the cleanup there, delete
it at api.slack.com/apps, which revokes both tokens.

Local leftovers: .env still holds your tokens. If you are done for good:

  shred -u ../.env 2>/dev/null || rm -P ../.env
EOF
