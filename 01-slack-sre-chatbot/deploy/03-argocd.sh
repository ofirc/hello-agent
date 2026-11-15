#!/usr/bin/env bash
# Step 3 of 4: register the Argo CD applications, expose the API inside the VPC, and
# mint the agent's own Argo CD token.
#
# This used to be a wall of kubectl patches and argocd CLI commands printed for you to
# copy. It is a script now because it is the step that hands the next step its inputs,
# and a handoff that depends on a human pasting four commands correctly is the reason
# ./04-agent.sh used to fail on a missing ARGOCD_TOKEN.
#
# It appends ARGOCD_BASE_URL and ARGOCD_TOKEN to ../.env, so 04-agent.sh just works.
#
# No argocd CLI needed: everything here is kubectl plus the Argo CD REST API.
set -euo pipefail
cd "$(dirname "$0")"
source ./env.sh
require_auth

ENV_FILE="../.env"
ACCOUNT="sre-agent"
PF_PORT=18080
PF_PID=""

cleanup() { [[ -n "$PF_PID" ]] && kill "$PF_PID" 2>/dev/null || true; }
trap cleanup EXIT

if ! kubectl get ns argocd >/dev/null 2>&1; then
  echo "Argo CD is not installed. Run ./01-gke.sh first." >&2
  exit 1
fi

# The committed Application manifests carry an IMAGE_REPO placeholder so the repo holds
# no project id. Substitute it into a temp copy and apply that, leaving the working
# tree clean and nothing project-specific ever committed.
echo "==> Registering the Argo CD applications (image repo: ${AR_PATH})"
RENDERED="$(mktemp -d)"
trap 'rm -rf "$RENDERED"; cleanup' EXIT
for f in ./argocd/*.yaml; do
  perl -pe "s{IMAGE_REPO}{${AR_PATH}}g" "$f" > "${RENDERED}/$(basename "$f")"
done

if grep -rq "IMAGE_REPO" "$RENDERED"; then
  echo "IMAGE_REPO placeholder survived substitution, refusing to apply." >&2
  exit 1
fi

kubectl apply -f "$RENDERED"

# The agent runs on Cloud Run, outside the cluster, so it needs a routable address for
# the Argo CD API. An internal load balancer keeps that address on the VPC: reachable
# from the Cloud Run instance via Direct VPC egress, unreachable from the internet.
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

# Create a dedicated account rather than reusing admin. The agent's actions are then
# attributable in the Argo CD audit log and its token is revocable on its own.
echo "==> Creating the ${ACCOUNT} account, scoped to get and sync"
kubectl patch configmap argocd-cm -n argocd --type merge \
  -p "{\"data\":{\"accounts.${ACCOUNT}\":\"apiKey\"}}"
kubectl patch configmap argocd-rbac-cm -n argocd --type merge -p "$(cat <<JSON
{"data":{"policy.csv":"p, role:${ACCOUNT}, applications, get, */*, allow\np, role:${ACCOUNT}, applications, sync, */*, allow\ng, ${ACCOUNT}, role:${ACCOUNT}\n"}}
JSON
)"

# argocd-server caches the configmaps, so it has to be restarted before it will issue a
# token for an account it has not seen.
echo "==> Restarting argocd-server to pick up the new account"
kubectl rollout restart deployment/argocd-server -n argocd
kubectl rollout status  deployment/argocd-server -n argocd --timeout=5m

echo "==> Port-forwarding to mint the token"
kubectl port-forward -n argocd svc/argocd-server "${PF_PORT}:443" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 20); do
  curl -sk "https://localhost:${PF_PORT}/api/version" >/dev/null 2>&1 && break
  sleep 1
done

ADMIN_PASSWORD="$(kubectl -n argocd get secret argocd-initial-admin-secret \
  -o jsonpath='{.data.password}' | base64 -d)"

api() { # api <method> <path> <json-body>
  curl -sk -X "$1" "https://localhost:${PF_PORT}$2" \
    -H "Content-Type: application/json" \
    ${ADMIN_JWT:+-H "Authorization: Bearer ${ADMIN_JWT}"} \
    -d "$3"
}

jsonfield() { python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1],""))' "$1"; }

ADMIN_JWT=""
ADMIN_JWT="$(api POST /api/v1/session \
  "{\"username\":\"admin\",\"password\":\"${ADMIN_PASSWORD}\"}" | jsonfield token)"
if [[ -z "$ADMIN_JWT" ]]; then
  echo "Could not log in to Argo CD as admin." >&2
  exit 1
fi

AGENT_TOKEN="$(api POST "/api/v1/account/${ACCOUNT}/token" '{}' | jsonfield token)"
if [[ -z "$AGENT_TOKEN" ]]; then
  echo "Could not mint a token for ${ACCOUNT}. Is accounts.${ACCOUNT} set in argocd-cm?" >&2
  exit 1
fi

# Replace rather than append, so rerunning this script does not leave a stale token
# earlier in the file shadowing the fresh one further down.
echo "==> Writing ARGOCD_* into ${ENV_FILE}"
[[ -f "$ENV_FILE" ]] || cp ../.env.example "$ENV_FILE"
python3 - "$ENV_FILE" "https://${ARGOCD_IP}" "$AGENT_TOKEN" <<'PY'
import sys, re
path, base, token = sys.argv[1], sys.argv[2], sys.argv[3]
text = open(path).read()
for key, val in (("ARGOCD_BASE_URL", base), ("ARGOCD_TOKEN", token)):
    line = f"{key}={val}"
    if re.search(rf"^{key}=.*$", text, re.M):
        text = re.sub(rf"^{key}=.*$", line, text, flags=re.M)
    else:
        text += ("" if text.endswith("\n") else "\n") + line + "\n"
open(path, "w").write(text)
PY

cat <<EOF

Argo CD is wired up.

  ARGOCD_BASE_URL  https://${ARGOCD_IP}   (private, VPC only)
  ARGOCD_TOKEN     written to .env        (account: ${ACCOUNT}, get + sync only)

That address is not reachable from your laptop. To use the agent locally, port-forward
and point it at localhost instead:

  kubectl port-forward -n argocd svc/argocd-server 8080:443 &
  ARGOCD_BASE_URL=https://localhost:8080 go run ./cmd/agent

The Argo CD UI, same forward, user admin:

  kubectl -n argocd get secret argocd-initial-admin-secret \\
    -o jsonpath='{.data.password}' | base64 -d

Next: ./04-agent.sh
EOF
