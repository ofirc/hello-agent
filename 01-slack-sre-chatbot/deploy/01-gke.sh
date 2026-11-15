#!/usr/bin/env bash
# Step 1 of 4: GKE Autopilot cluster + Argo CD.
#
# This script does NOT register the Argo CD applications. That happens in 03-argocd.sh,
# after 02-stack.sh has pushed the images, so Argo never spends time in ImagePullBackOff
# trying to sync a tag that does not exist yet.
#
# Autopilot rather than Standard: no node pools to size, no upgrades to babysit, and the
# workload is three tiny Deployments. Scaling is not the point of this demo.
set -euo pipefail
cd "$(dirname "$0")"
source ./env.sh
require_auth

echo "==> Enabling APIs"
gcloud services enable \
  container.googleapis.com \
  artifactregistry.googleapis.com \
  cloudbuild.googleapis.com \
  secretmanager.googleapis.com \
  aiplatform.googleapis.com \
  --project="$PROJECT"

echo "==> Creating Artifact Registry repo ${AR_REPO} in ${REGION}"
gcloud artifacts repositories create "$AR_REPO" \
  --project="$PROJECT" \
  --location="$REGION" \
  --repository-format=docker \
  --description="SRE demo stack and agent images" \
  --labels="$LABELS" 2>/dev/null || echo "    already exists, continuing"

echo "==> Creating Autopilot cluster ${CLUSTER} in ${REGION} (this takes ~8 minutes)"
#
# --enable-private-nodes gives the nodes no external IP. Worth doing on its own merits,
# and required outright in any environment that disallows external IPs on VMs.
#
# If external IPs are disallowed and you leave this off, the failure is quiet and slow:
# Autopilot defaults to public nodes, every node creation is refused, the cluster still
# reports RUNNING, and pods sit in Pending forever. The only clear signal is in the
# autoscaler, not in the cluster or the pods:
#
#   kubectl get configmap -n kube-system cluster-autoscaler-status -o jsonpath='{.data}'
#   errorCode: VM_EXTERNAL_IP_ACCESS_POLICY_CONSTRAINT
#
# Private nodes then need Cloud NAT to pull images from public registries such as
# quay.io, where the Argo CD images live. The check below fails loudly rather than
# letting you discover that as an ImagePullBackOff ten minutes from now.
gcloud container clusters create-auto "$CLUSTER" \
  --project="$PROJECT" \
  --region="$REGION" \
  --enable-private-nodes \
  --labels="$LABELS" 2>/dev/null || echo "    already exists, continuing"

# Guard against the other half of the same problem: a private cluster with no route to
# the internet cannot pull the Argo CD images.
echo "==> Checking Cloud NAT exists for private nodes"
if ! gcloud compute routers list --project="$PROJECT" \
      --filter="region:${REGION} AND network:${VPC_NETWORK}" \
      --format='value(name)' | grep -q .; then
  cat >&2 <<EOF
No Cloud Router/NAT found on network '${VPC_NETWORK}' in ${REGION}.

Private nodes have no external IP, so without NAT they cannot reach quay.io and the
Argo CD pods will sit in ImagePullBackOff. Create one:

  gcloud compute routers create ${CLUSTER}-nat-router \\
    --project=${PROJECT} --region=${REGION} --network=${VPC_NETWORK}
  gcloud compute routers nats create ${CLUSTER}-nat \\
    --project=${PROJECT} --region=${REGION} --router=${CLUSTER}-nat-router \\
    --auto-allocate-nat-external-ips --nat-all-subnet-ip-ranges
EOF
  exit 1
fi

echo "==> Fetching credentials"
gcloud container clusters get-credentials "$CLUSTER" \
  --project="$PROJECT" --region="$REGION"

echo "==> Installing Argo CD"
kubectl create namespace argocd --dry-run=client -o yaml | kubectl apply -f -

# --server-side is required, not a preference.
#
# A client-side `kubectl apply` stores the whole object in the
# kubectl.kubernetes.io/last-applied-configuration annotation, and the ApplicationSet
# CRD is larger than the 262144-byte annotation limit. The apply fails with:
#
#   The CustomResourceDefinition "applicationsets.argoproj.io" is invalid:
#   metadata.annotations: Too long: may not be more than 262144 bytes
#
# Server-side apply keeps field ownership in the object's metadata instead of an
# annotation, so the limit never applies. --force-conflicts lets a rerun take ownership
# of fields a previous client-side apply claimed.
kubectl apply -n argocd --server-side --force-conflicts \
  -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml

echo "==> Waiting for Argo CD to become ready (a few minutes on a cold Autopilot cluster)"
kubectl rollout status -n argocd deployment/argocd-server --timeout=10m
kubectl rollout status -n argocd deployment/argocd-repo-server --timeout=10m

echo
echo "Cluster and Argo CD are up."
echo "Next: ./02-stack.sh"
