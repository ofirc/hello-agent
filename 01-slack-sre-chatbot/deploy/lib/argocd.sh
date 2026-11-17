#!/usr/bin/env bash
# Argo CD helpers shared by deploy/03-argocd.sh (GKE) and deploy/local/up.sh (kind).
#
# Source it, do not execute it. It sets no shell options and changes no directory: the
# caller owns `set -euo pipefail` and its own working directory.
#
# Every function takes what it needs as arguments (base URL, env file, kubectl context)
# and touches no caller globals, so the two scripts cannot disturb each other. What
# differs between them stays in the scripts: how the API is reached (an internal load
# balancer behind a port-forward on GKE, a NodePort on kind) and which cluster the
# kubectl context must name.
#
# The bodies below were moved out of 03-argocd.sh as they were. Only the inputs changed,
# from globals to arguments.

# argocd_ensure_account <account> <kubectl-context>
#
# A dedicated Argo CD account, so the agent's actions are attributable in the audit
# log and its token is revocable without touching admin. Skips when the account and its
# RBAC already exist, and restarts argocd-server only when it changed something.
argocd_ensure_account() {
  local ACCOUNT="$1" kube_ctx="$2" have_account have_rbac ACCOUNT_CHANGED
  have_account="$(kubectl --context "$kube_ctx" get cm argocd-cm -n argocd \
    -o jsonpath="{.data.accounts\.${ACCOUNT}}" 2>/dev/null || true)"
  have_rbac="$(kubectl --context "$kube_ctx" get cm argocd-rbac-cm -n argocd \
    -o jsonpath='{.data.policy\.csv}' 2>/dev/null | grep -c "role:${ACCOUNT}" || true)"

  if [[ "$have_account" == "apiKey" && "$have_rbac" != "0" ]]; then
    echo "==> Account ${ACCOUNT} already configured, skipping"
    ACCOUNT_CHANGED=false
  else
    echo "==> Creating the ${ACCOUNT} account, scoped to get and sync"
    kubectl --context "$kube_ctx" patch configmap argocd-cm -n argocd --type merge \
      -p "{\"data\":{\"accounts.${ACCOUNT}\":\"apiKey\"}}"
    kubectl --context "$kube_ctx" patch configmap argocd-rbac-cm -n argocd --type merge -p "$(cat <<JSON
{"data":{"policy.csv":"p, role:${ACCOUNT}, applications, get, */*, allow\np, role:${ACCOUNT}, applications, sync, */*, allow\ng, ${ACCOUNT}, role:${ACCOUNT}\n"}}
JSON
)"
    ACCOUNT_CHANGED=true
  fi

  argocd_restart_server_if_changed "$ACCOUNT_CHANGED" "$kube_ctx"
}

# argocd_restart_server_if_changed <true|false> <kubectl-context>
#
# argocd-server caches those configmaps, so it only needs restarting when they
# actually changed. Restarting unconditionally would drop the API for a minute on
# every re-run, which is exactly what makes a script feel unsafe to repeat.
argocd_restart_server_if_changed() {
  local changed="$1" kube_ctx="$2"
  if [[ "$changed" == true ]]; then
    echo "==> Restarting argocd-server to pick up the new account"
    kubectl --context "$kube_ctx" rollout restart deployment/argocd-server -n argocd
    kubectl --context "$kube_ctx" rollout status  deployment/argocd-server -n argocd --timeout=5m
  fi
}

# argocd_existing_token <env-file>
# Prints the ARGOCD_TOKEN already in the file, if any.
argocd_existing_token() {
  local env_file="$1"
  [[ -f "$env_file" ]] || return 1
  grep -E '^ARGOCD_TOKEN=.+' "$env_file" 2>/dev/null | head -1 | cut -d= -f2-
}

# argocd_token_works <token> <base-url>
#
# A token counts as "already fine" only if Argo CD still accepts it. Rebuilding the
# cluster leaves a perfectly well-formed token in .env that no longer authenticates,
# so ask the API rather than testing for a non-empty string.
argocd_token_works() {
  local t="$1" base_url="$2" code
  [[ -n "$t" ]] || return 1
  code="$(curl -sk -o /dev/null -w '%{http_code}' --max-time 10 \
    -H "Authorization: Bearer ${t}" \
    "${base_url}/api/v1/applications" 2>/dev/null || echo 000)"
  [[ "$code" == "200" ]]
}

argocd_jsonfield() { python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1],""))' "$1"; }

# argocd_mint_token <base-url> <account> <kubectl-context>
#
# Logs in as admin with the initial admin secret and mints a token for <account>.
# Prints only the token on stdout. Failures go to stderr and return non-zero, so a
# caller using `TOKEN="$(argocd_mint_token ...)"` under `set -e` stops, exactly as the
# inline code did. The explicit `|| return` matters: bash clears `-e` inside command
# substitution, so without it a failed kubectl would carry on with an empty password.
argocd_mint_token() {
  local base_url="$1" account="$2" kube_ctx="$3" ADMIN_PASSWORD ADMIN_JWT AGENT_TOKEN
  ADMIN_PASSWORD="$(kubectl --context "$kube_ctx" -n argocd get secret argocd-initial-admin-secret \
    -o jsonpath='{.data.password}' | base64 -d)" || return $?

  ADMIN_JWT="$(curl -sk -X POST "${base_url}/api/v1/session" \
    -H "Content-Type: application/json" \
    -d "{\"username\":\"admin\",\"password\":\"${ADMIN_PASSWORD}\"}" | argocd_jsonfield token)" || return $?
  if [[ -z "$ADMIN_JWT" ]]; then
    echo "Could not log in to Argo CD as admin." >&2
    return 1
  fi

  AGENT_TOKEN="$(curl -sk -X POST "${base_url}/api/v1/account/${account}/token" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${ADMIN_JWT}" \
    -d '{}' | argocd_jsonfield token)" || return $?
  if [[ -z "$AGENT_TOKEN" ]]; then
    echo "Could not mint a token for ${account}." >&2
    return 1
  fi
  printf '%s\n' "$AGENT_TOKEN"
}

# env_file_set <env-file> KEY=value [KEY=value ...]
#
# Replace in place rather than append, so repeated runs cannot leave a stale value
# earlier in the file shadowing the fresh one. The file must already exist.
env_file_set() {
  python3 - "$@" <<'PY'
import sys, re
path = sys.argv[1]
text = open(path).read()
for pair in sys.argv[2:]:
    key, val = pair.split("=", 1)
    line = f"{key}={val}"
    if re.search(rf"^{key}=.*$", text, re.M):
        text = re.sub(rf"^{key}=.*$", line, text, flags=re.M)
    else:
        text += ("" if text.endswith("\n") else "\n") + line + "\n"
open(path, "w").write(text)
PY
}
