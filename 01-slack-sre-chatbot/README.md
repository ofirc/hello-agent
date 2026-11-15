# 01 - Slack SRE ChatOps Agent

A production-shaped ChatOps agent that answers deployment questions, finds release drift
between environments, and opens pull requests to fix it.

```
/sre service list                 every service it knows about, and where each one runs
/sre service get <service>        full detail for one service, kubectl describe style

/sre version get [env]            deployed version of every service, per environment
/sre version diff <envA> <envB>   compare two environments side by side

/sre drift list [env]             what has drifted from git, read-only and fast
/sre drift debug [service]        agentic triage: git + Argo CD + Grafana + JIRA -> root cause
/sre drift fix <service> [env]    open a PR that realigns the environment

/sre rollout status <svc> [env]   Argo CD sync and health for one service
/sre doctor                       which integrations are wired up
/sre help
```

Architecture: Slack over Socket Mode into an always-on agent on a Cloud Run instance,
fanning out to Grafana, JIRA, GitHub and Argo CD over REST. Nothing listens. Secrets come
from Secret Manager, identity from the attached service account.

```
User -> Slack -> [ SRE Agent (Socket Mode) ] -> Argo CD -> GKE
                        |      |     |
                        |      |     +-> GitHub (desired state, fix PRs)
                        |      +-------> Grafana (is anyone hurt?)
                        |              > JIRA   (has anyone complained?)
                        +-> Secret Manager + Vertex AI
```

---

## Quickstart

The whole thing, in order. Each step's output feeds the next, so run them in sequence.
Roughly 25 minutes, most of it waiting on the Autopilot cluster.

```bash
# 0. Auth
gcloud auth login --update-adc
gcloud config set project <your-project-id>   # every script reads this

# 1. Slack app: paste manifest.yaml at api.slack.com/apps, install it, copy both tokens
cd 01-slack-sre-chatbot
pbcopy < manifest.yaml
cp .env.example .env && vim .env     # SLACK_BOT_TOKEN, SLACK_APP_TOKEN, GITHUB_TOKEN

# 2. Infrastructure, strictly in this order
cd deploy
./01-gke.sh        # GKE Autopilot + Argo CD                    ~10 min
./02-stack.sh      # build product-catalog v1.1.0 and v1.2.0     ~4 min
./03-argocd.sh     # register apps, mint token, write to .env    ~3 min
./04-agent.sh      # build + deploy the agent                    ~5 min

# 3. In Slack
/sre doctor
/sre drift list
```

Each script prints the next one to run and refuses to start if a previous step has not
happened. `03-argocd.sh` writes `ARGOCD_BASE_URL` and `ARGOCD_TOKEN` into `.env`, so
`04-agent.sh` picks them up with nothing to copy by hand.

**Prerequisites:** `gcloud` (with `beta` and `alpha`), `kubectl`, `docker`, `gh`,
`go` 1.26+, `python3`, `curl`. No `argocd` CLI and no ngrok.

`gh` needs the `write:packages` scope for the GHCR push in step 4:

```bash
gh auth refresh -h github.com -s write:packages
```

---

## 1. (Optional) Create a Slack workspace

Already done for this workshop. We use **harness-engineering.slack.com**.

Creating your own is free at [slack.com/get-started](https://slack.com/get-started), no
card. The free-plan limits that touch this bot are 90-day message history and about ten
app integrations. Neither affects a Socket Mode agent that answers slash commands.

---

## 2. Create the Slack app and collect tokens

```bash
cd 01-slack-sre-chatbot
pbcopy < manifest.yaml
```

1. [api.slack.com/apps](https://api.slack.com/apps) -> **Create new app** -> *From
   scratch* -> **Paste a manifest** -> paste -> pick your workspace.
2. **Install App** -> *Install to `<workspace>`* -> **Allow**.
   Copy the **Bot User OAuth Token** (`xoxb-...`).
3. **Basic Information** -> scroll to *App-Level Tokens* -> **Generate**, scope
   `connections:write`. Copy it (`xapp-...`).

```bash
cp .env.example .env
vim .env          # SLACK_BOT_TOKEN, SLACK_APP_TOKEN, GITHUB_TOKEN
```

Verify the bot token before going further. One call, no Go:

```bash
set -a && source .env && set +a
curl -s -H "Authorization: Bearer $SLACK_BOT_TOKEN" https://slack.com/api/auth.test
```

```json
{"ok":true,"team":"Harness Engineering","user":"sre-agent","bot_id":"B0..."}
```

`{"ok":false,"error":"invalid_auth"}` means the token is wrong, revoked, or you pasted
the Client Secret. `"error":"token_revoked"` means you reinstalled since copying it.

<details>
<summary><b>Installing is not optional, and why you will reinstall</b></summary>

Pasting the manifest only defines the app. **Install App -> Install to `<workspace>`**
is what creates it, and it does three things at once:

1. Mints the bot token. There is no `xoxb-` anywhere until you install.
2. Registers `/sre`. Before installing, typing `/sre` in Slack gets you nothing.
3. Creates the `@sre-agent` bot user you can mention and DM.

The consent screen lists every scope from the manifest. If your workspace requires admin
approval for app installs, this is where it asks.

**Reinstall after any manifest change that touches scopes.** Editing the manifest
updates the app definition but not the grant you already hold, and the symptom is a
`missing_scope` error at runtime that reads like a code bug.

The two tokens are independent. Installing and reinstalling do not affect the app-level
token, so generate it once and forget it.
</details>

<details>
<summary><b>The four credentials you can ignore</b></summary>

Creating the app hands you six credentials. This agent uses two. The other four exist to
establish or secure an **inbound** connection, and a Socket Mode app never has one.

| Credential | What it is for | Why not here |
| --- | --- | --- |
| **App ID** | identifying the app in URLs and support tickets | an identifier, not a secret, and nothing reads it |
| **Client ID** + **Client Secret** | the OAuth 2.0 install flow, for distributing to workspaces other than your own | we install into one workspace with the Install button, which completes OAuth in the browser and hands back the bot token. No OAuth code to write |
| **Signing Secret** | verifying `X-Slack-Signature` on inbound HTTP from Slack | **Socket Mode has no inbound HTTP.** The agent dials out, so there is no request to verify. This is the one people wire up out of habit |
| **Verification Token** | the deprecated predecessor to the Signing Secret | deprecated, and unused here for the same reason |

That is also why there is no `EXPOSE` in the Dockerfile and why `--ingress=internal`
costs us nothing in step 7.
</details>

<details>
<summary><b>Why each scope is in the manifest</b></summary>

| Scope | Earns its place because |
| --- | --- |
| `commands` | the `/sre` slash command |
| `app_mentions:read`, `channels:history`, `groups:history` | `@sre-agent drift list` and reading the surrounding thread for triage context |
| `chat:write`, `chat:write.public` | posting results, including to channels the bot was never invited to |
| `files:write` | drift diffs and Argo CD manifests exceed Slack's 3000-character block limit routinely |
| `reactions:write` | progress signalling on a triage with a two-minute p50 |
| `im:history`, `im:read`, `im:write` | DM conversations with the agent |
| `users:read`, `users:read.email` | mapping the Slack caller to a GitHub and JIRA identity, which is what makes the on-behalf-of audit trail real |
| `assistant:write` | the assistant side panel |
| `channels:manage`, `groups:write` | "open temporary Slack rooms for shared triage", from the below-the-line requirements |

`interactivity` is on because `drift debug` ends in buttons and `drift fix` posts a
review link. `always_online: true` is honest here, the agent really is always on.

`cmd/agent/manifest_test.go` ties the file to the code. It fails the build if a scope the
agent actually calls is missing, if an event `handleEvents` switches on is not
subscribed, if Socket Mode gets turned off, or if interactivity is disabled while the
handlers still render buttons.

If the validator rejects `assistant_view`, delete that block and install without it.
Nothing else depends on it.
</details>

<details>
<summary><b>Rotating a leaked token</b></summary>

A bot token is a single line in a web UI, so it ends up in screenshots, scrollback and
pasted snippets. Rotating is cheap, so do it on suspicion rather than reasoning about
whether the exposure mattered.

1. **Install App -> Reinstall to `<workspace>`.** The old `xoxb-` dies immediately.
2. Update `.env`.
3. Push a new secret version and restart:

```bash
printf '%s' "$NEW_TOKEN" | gcloud secrets versions add slack-bot-token \
  --project=$PROJECT --data-file=-
gcloud beta run instances restart sre-agent --project=$PROJECT --region=us-east1
```

`printf` rather than `echo`: `echo` appends a newline, the trailing `\n` becomes part of
the secret, and the result is an `invalid_auth` that looks nothing like a whitespace bug.

The `xapp-` token rotates separately on Basic Information: delete it, generate a new one.
</details>

---

## 3. Bring up GKE and Argo CD

```bash
cd deploy
./01-gke.sh
```

Enables the APIs, creates the Artifact Registry repo, creates a GKE Autopilot cluster,
installs Argo CD and waits for it. About 10 minutes, nearly all of it the cluster.

It deliberately does **not** register the Argo CD applications yet. That waits for step 5,
after the images exist, so Argo never sits in `ImagePullBackOff` chasing a tag that has
not been built.

<details>
<summary><b>Why GKE, and why Autopilot</b></summary>

GKE rather than Cloud Run services for the workload, because Argo CD is the component
that makes the drift real: it holds the cluster credentials, computes sync and health,
and gives the agent an access-token-shaped credential instead of a kubeconfig. The agent
never talks to Kubernetes directly.

Autopilot rather than Standard: no node pools to size and no upgrades to babysit, for
three tiny Deployments. Scaling is not the point of this demo.
</details>

---

## 4. Build the production stack

One-time setup. The `gh` token needs the `write:packages` scope to push to GHCR, and it
is not included by default:

```bash
gh auth refresh -h github.com -s write:packages
gh auth status                      # confirm the active account is ofirc
docker info >/dev/null && echo "docker ok"
```

Then:

```bash
./02-stack.sh
```

Builds `product-catalog` twice, as `v1.1.0` and `v1.2.0`, and pushes both to
`ghcr.io/<owner>/product-catalog`. Same source both times; only the baked-in
`APP_VERSION` differs, and the `?filter=` feature is gated on it. The script reuses the
`gh` token for `docker login`, so there is no second credential to manage.

**One manual step, first run only.** A new GHCR package is **private**, and the cluster
pulls anonymously with no pull secret. Make it public or every pod sits in
`ImagePullBackOff`:

```
https://github.com/users/<owner>/packages/container/product-catalog/settings
-> Danger Zone -> Change visibility -> Public
```

There is no API for this — `PATCH /user/packages/container/...` returns 404. It is UI
only.

Verify the way the kubelet will, with no docker and no cached credentials:

```bash
TOK=$(curl -s "https://ghcr.io/token?service=ghcr.io&scope=repository:ofirc/product-catalog:pull" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
curl -s -o /dev/null -w "%{http_code}\n" -H "Authorization: Bearer $TOK" \
  https://ghcr.io/v2/ofirc/product-catalog/manifests/v1.1.0
# 200 = the cluster can pull
```

Do not check this with `docker manifest inspect`. Docker Desktop's credential helper
supplies your login even when you point `DOCKER_CONFIG` at an empty directory, so a
private package reports as readable and you find out later, from `ImagePullBackOff`.

<details>
<summary><b>Why GHCR and not Artifact Registry</b></summary>

The image reference has to be committed to this repo, because **Argo CD renders the
overlays from GitHub, not from your laptop**. There is no shell and no substitution at
sync time, so whatever is in the commit is what gets deployed.

An Artifact Registry path embeds a cloud project id, which would then live in public
git history. A GHCR path contains only a GitHub owner, which this repo already
discloses. So the overlays are static and carry nothing new:

```yaml
images:
  - name: product-catalog
    newName: ghcr.io/ofirc/product-catalog
    newTag: v1.1.0          # the only line /sre drift fix touches
```

Nothing is substituted and nothing has to be regenerated before `03-argocd.sh`.

The agent's own image still goes to Artifact Registry in `04-agent.sh`. That reference
is passed to Cloud Run as a command-line flag and never committed, so it leaks nothing.

**Build for `linux/amd64` explicitly.** GKE nodes are amd64. On an Apple Silicon Mac the
default build is arm64, and those images pull perfectly and then crash-loop with `exec
format error`, which reads like an application bug rather than a build problem.
`02-stack.sh` passes `--platform` and re-inspects each pushed image to confirm.
</details>

<details>
<summary><b>Why the registry cannot live in the Argo CD Application instead</b></summary>

It looks like `spec.source.kustomize.images` should solve the project-id problem: put
the version in the overlay, the registry in the Application, commit neither. It does
not work.

An image override there **replaces** the overlay's `images` entry for that name rather
than merging with it. Setting only the image name discards `newTag`, kustomize falls
back to the tag in the base manifest, and every environment silently deploys
`product-catalog:latest` — which does not exist, so the pods sit in
`ImagePullBackOff`.

The trap is that it renders correctly if you test it by *appending* a second entry to
the overlay's own `images:` list, because kustomize merges those. That is not what Argo
CD does.

```bash
kubectl kustomize gitops/overlays/prod | grep -E 'image:|APP_VERSION' -A1
# image: ghcr.io/ofirc/product-catalog:v1.1.0
# APP_VERSION: v1.1.0
```
</details>

---

## 5. Wire up Argo CD

```bash
./03-argocd.sh
```

This is the step that used to be a wall of manual `kubectl` patches. It now:

1. Registers the three Argo CD applications, which starts the sync.
2. Publishes the Argo CD API on an **internal** load balancer, reachable from the VPC
   and not from the internet.
3. Creates a dedicated `sre-agent` Argo CD account scoped to **get and sync only**.
4. Mints its token and writes `ARGOCD_BASE_URL` + `ARGOCD_TOKEN` into `.env`.

### Checkpoint: see it in the Argo CD UI

Worth doing once, before any Slack command exists. This is the picture the agent is
reading over the API, and seeing it yourself makes every later `/sre` answer easy to
sanity-check.

```bash
kubectl port-forward -n argocd svc/argocd-server 8080:443 >/dev/null 2>&1 &
kubectl -n argocd get secret argocd-initial-admin-secret \
  -o jsonpath='{.data.password}' | base64 -d; echo
open https://localhost:8080        # username: admin, password from above
```

Your browser will warn about the certificate. That is expected: Argo CD serves a
self-signed cert by default, which is the same reason the agent sets
`ARGOCD_INSECURE=true`. Click through.

**What good looks like** — three applications, all `Synced` and `Healthy`:

```
product-catalog-test    Synced   Healthy
product-catalog-stage   Synced   Healthy
product-catalog-prod    Synced   Healthy
```

Click into `product-catalog-prod` and the resource tree shows the Deployment, ReplicaSet
and three Pods. Open the Deployment manifest and check the image tag reads `v1.1.0`
while stage reads `v1.2.0`. That difference *is* the drift the demo finds, visible
before the agent says a word.

The same check without a browser:

```bash
kubectl get applications -n argocd
kubectl get deploy -A -l app.kubernetes.io/name=product-catalog \
  -o custom-columns='NS:.metadata.namespace,READY:.status.readyReplicas,IMAGE:.spec.template.spec.containers[0].image'
```

**If health is not `Healthy`**, read it here rather than guessing later:

| What you see | Meaning |
| --- | --- |
| `Synced` / `Progressing` | pods still starting, Autopilot is scheduling nodes. Wait |
| `Synced` / `Degraded` with `ImagePullBackOff` | the rendered image tag does not exist in the registry. Compare the pod's image against `gcloud artifacts docker images list` |
| `Unknown` / `ComparisonError` | Argo cannot read the repo. The path or branch is wrong, or the commit was never pushed |
| `OutOfSync` | git and the cluster disagree. For this demo that is the interesting case, not a failure |

Stop the port-forward when done:

```bash
kill %1    # or: pkill -f 'port-forward.*argocd-server'
```

<details>
<summary><b>How the agent reaches a cluster it has no credentials for</b></summary>

The agent holds no kubeconfig and no GKE IAM. It reads cluster state only through the
Argo CD API, with an Argo CD account that can `get` and `sync` and nothing else. One
component holds cluster-admin, and the agent's access is a token you can attribute in
the audit log and revoke on its own.

That API has to be reachable from Cloud Run, which is outside the cluster. Two pieces
make that work and both are easy to miss:

- `03-argocd.sh` turns `argocd-server` into an **internal** load balancer
  (`networking.gke.io/load-balancer-type: Internal`), so the address lives on the VPC.
- `04-agent.sh` passes `--network` / `--subnet`, giving the Cloud Run instance **Direct
  VPC egress**. Without it the instance has only public internet egress and every Argo CD
  call times out, which looks exactly like a bad token.

**TLS is skipped against Argo CD** (`ARGOCD_INSECURE=true`, the default). A default Argo
CD install serves a self-signed certificate, so a correct client refuses to talk to it.
The connection is still encrypted and still crosses only private addresses, but the
server end is unauthenticated. It is scoped to this one provider in
`httpx.InsecureTransport`. The real fix is a certificate the agent trusts, via
cert-manager with an internal CA, and that is out of scope for a workshop.
</details>

---

## 6. Run the agent locally

Before containerising anything, check the whole chain works. Socket Mode means no
tunnel, no ngrok, no public URL.

```bash
cd ..
kubectl port-forward -n argocd svc/argocd-server 8080:443 &
set -a && source .env && set +a
ARGOCD_BASE_URL=https://localhost:8080 go run ./cmd/agent
```

```json
{"msg":"connected to slack with socket mode","bot":"sre-agent","team":"Harness Engineering"}
```

Then in Slack, `/sre doctor`. The override matters: `.env` holds the internal load
balancer address from step 5, which your laptop cannot route to.

<details>
<summary><b>What the code is, and the three decisions worth defending</b></summary>

```
cmd/agent/main.go              socket loop, ack, event fan-out
internal/config/               env -> Config, with fail-fast token validation
internal/slackio/              Reporter: one message that keeps updating
internal/commands/
  router.go                    the noun-verb spec table, aliases, backpressure
  service.go                   service list, service get
  version.go                   version get, version diff, rollout status
  drift.go                     drift list, drift debug, drift fix
  interactions.go              buttons, routed back through Dispatch
internal/drift/                the diff engine. Pure, no I/O
internal/providers/
  httpx/                       one place for timeouts, retries, error redaction
  argocd/  github/  gitops/    actual state, PRs, desired state
  grafana/ jira/               symptoms and reports
internal/llm/vertex.go         triage narration, with a deterministic fallback
```

```bash
go build ./... && go test ./...
```

**Ack before you work.** Slack gives a slash command three seconds. The socket loop acks
immediately and hands off to a goroutine, so a four-minute triage and a 200ms version
lookup are the same code path:

```go
case socketmode.EventTypeSlashCommand:
    cmd, _ := evt.Data.(slack.SlashCommand)
    client.Ack(*evt.Request)                      // always first
    go router.Dispatch(ctx, cmd.ChannelID, cmd.UserID, cmd.UserName, cmd.Text)
```

**The model explains, it does not decide.** Every fact in a triage is gathered
deterministically by the providers. Vertex receives the assembled evidence and ranks it.
It never picks what to fetch and never calls a tool, so a bad generation produces a
confusing paragraph rather than an unexpected API call against prod. If Vertex is
unconfigured or erroring, `deterministicNarrative` encodes the same reasoning and the
demo does not go blank.

**The fix direction depends on the finding.** This is the subtle one, and getting it
backwards is the easiest mistake in the whole program:

- A **promotion gap** means git itself encodes the lag. Stage runs v1.2.0, the prod
  overlay still pins v1.1.0, and both environments match their own overlay. The fix moves
  the prod overlay *forward*.
- A **version skew or failed sync** means git is already ahead and the cluster could not
  get there. Moving git forward again changes nothing. The useful edit is *backwards*:
  pin the overlay to the version actually running, so Argo stops retrying a broken
  release and git stops lying. Rolling forward is a human decision that needs the broken
  image fixed first.

`drift.Finding.Plan()` encodes both, and `drift_test.go` pins the direction of each.
</details>

---

## 7. Deploy the agent

```bash
cd deploy
./04-agent.sh
```

Reads `.env` itself, upserts every token into Secret Manager, builds the image with
Cloud Build, creates the Cloud Run instance, and waits until the agent logs a Slack
connection.

```bash
gcloud beta run instances describe sre-agent --project=$PROJECT --region=us-east1
gcloud beta run instances logs read sre-agent --project=$PROJECT --region=us-east1
gcloud beta run instances ssh  sre-agent --project=$PROJECT --region=us-east1
```

`describe` reporting uptime proves the control plane started it. `connected to slack with
socket mode` in the logs proves the process is up, which is the signal that matters. SSH
works even with internal ingress, because the management plane is independent of the data
plane.

A `curl` of the instance URL returning HTTP 404 from `server: Google Frontend` is the
ingress gate doing its job. Do not chase it.

Between sessions, `stop` rather than `delete`. Config is preserved, restart is fast,
billing pauses:

```bash
gcloud beta run instances stop sre-agent --project=$PROJECT --region=us-east1
```

<details>
<summary><b>Why a Cloud Run instance, and the flags that are not decoration</b></summary>

[Cloud Run instances](https://cloud.google.com/blog/products/serverless/introducing-cloud-run-instances):
a single always-on container with a VM-shaped lifecycle. The right shape for a headless,
always-on, stateful agent. A Cloud Run *service* scales to zero and is driven by inbound
requests, which is exactly wrong for a process that holds a WebSocket open and keeps
state between turns.

- `--ingress=internal` because nothing should be able to reach this instance. The agent
  only dials out, so accepting inbound traffic would be pure attack surface.
- `--network` / `--subnet` give Direct VPC egress, which is the only reason the agent can
  reach the Argo CD internal load balancer.
- `--set-secrets` keeps tokens out of the image and out of `describe` output. The
  instance authenticates as its attached service account, so there is no JSON key to
  create, store, rotate or leak.
- `--cpu=1 --memory=512Mi` overrides the 2 vCPU / 2048Mi default, roughly double the
  config behind the commonly quoted price. Instances bill continuously.
- `--service-account` holds only `secretAccessor` and `aiplatform.user`. No GKE access
  at all.
- `--labels` tags everything the demo creates, so it is easy to find and clean up later.
</details>

---

## 8. Test it

In any channel in `harness-engineering.slack.com`:

```
/sre doctor
```

```
Integrations
✅ github   - desired state and fix PRs
✅ argocd   - actual cluster state
✅ grafana  - is the drift hurting anyone
❌ jira     - has anyone already reported it
✅ vertex   - root-cause narration

Environments test, stage, prod
Fixable environments stage, prod
GitOps overlays 01-slack-sre-chatbot/gitops/overlays/<env>/kustomization.yaml in ofirc/hello-agent
Remediation opens a PR, never merges
```

```
/sre service list
```

```
Services - 3 known, 1 with drift
SERVICE          ENVIRONMENTS      PROD    STATUS
checkout         stage,prod        v2.4.1  ok
payments         test,stage,prod   v0.9.3  ok
product-catalog  test,stage,prod   v1.1.0  🟠 promotion-gap

/sre service get <service> for detail
```

```
/sre service get product-catalog
```

```
product-catalog
ghcr.io/ofirc/product-catalog

ENV    RUNNING  GIT     SYNC    HEALTH   LAST SYNC
test   v1.2.0   v1.2.0  Synced  Healthy  2h ago
stage  v1.2.0   v1.2.0  Synced  Healthy  2h ago
prod   v1.1.0   v1.1.0  Synced  Healthy  3d ago

Findings
🟠 prod/product-catalog is on v1.1.0, stage already has v1.2.0

Grafana (prod, 6h) 0.00% 5xx, p99 48ms, 12.4 rps over 6h0m0s
JIRA last 30 days
    - SRE-412 product catalog missing the new filters (Open)

/sre drift debug product-catalog to triage  |  /sre drift fix product-catalog to open a PR
```

`RUNNING` and `GIT` agree in every row. Nothing is broken, nothing failed to deploy, and
prod is still a release behind. That distinction is the whole reason `promotion-gap` is a
separate finding kind from `sync-failed`.

```
/sre drift debug product-catalog
```

```
🟠 Triage: product-catalog in prod

Evidence
• git vs cluster: prod/product-catalog is on v1.1.0, stage already has v1.2.0
• Argo CD: sync=Synced health=Healthy, last sync 3d ago
• Grafana (prod, 6h): 0.00% 5xx, p99 48ms, 12.4 rps over 6h0m0s
• JIRA, last 30 days (1):
    - SRE-412 product catalog missing the new filters (Open)

🧠 Root cause
- product-catalog in prod is on v1.1.0 while stage already runs v1.2.0.
- Nothing failed. The release was simply never promoted, so the overlay still pins
  the older tag.
- No user-visible errors (0.00% 5xx, p99 48ms, 12.4 rps). The old version is serving
  correctly, this is staleness rather than an outage.
- SRE-412 was already reported: product catalog missing the new filters
- Next: /sre drift fix product-catalog

🔧 Suggested fix
01-slack-sre-chatbot/gitops/overlays/prod/kustomization.yaml: v1.1.0 -> v1.2.0
stage already runs v1.2.0 and is healthy. This promotes the same version to prod.

[ Open fix PR ]  [ Re-check ]  [ Grafana ]
```

```
/sre drift fix product-catalog
```

```
✅ PR opened #42
promote(prod): product-catalog v1.1.0 -> v1.2.0

01-slack-sre-chatbot/gitops/overlays/prod/kustomization.yaml
  v1.1.0 -> v1.2.0

stage already runs v1.2.0 and is healthy. This promotes the same version to prod.

[ Review PR ]  [ Re-check drift ]
requested by @ofir | the agent does not merge, that stays with a human
```

The agent stops at the PR. Merging is a human decision, which is what makes the
autonomous flow safe to run in front of a room. Merge it live, let Argo CD auto-sync, and
`/sre version get prod` closes the loop on stage.

<details>
<summary><b>The drift the demo finds, and how to stage the louder variant</b></summary>

Stage and test run v1.2.0, prod runs v1.1.0, and every environment matches its own
overlay. Nothing is broken, nothing is red, and the error rate is zero. Prod was simply
never promoted.

That is the scenario from the opening slide: the customer is right, the feature is
missing, it works on your machine, and it is already pushed to git. `product-catalog`
gates its `?filter=` parameter on v1.2.0, so prod returns 501 for a request that works
perfectly in stage.

To stage the louder variant instead, point the prod overlay at a tag that was never
built. Argo CD goes `OutOfSync` with `ImagePullBackOff`, the finding becomes
`sync-failed`, and `drift fix` flips to proposing a rollback rather than a promotion.
</details>

---

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| `./04-agent.sh` says ARGOCD_TOKEN missing | you skipped `./03-argocd.sh`, which mints it and writes it to `.env` |
| `./04-agent.sh` says ARGOCD_BASE_URL points at localhost | `.env` holds a port-forward address from local testing. Rerun `./03-argocd.sh` |
| `/sre doctor` shows argocd unconfigured | `ARGOCD_BASE_URL` or `ARGOCD_TOKEN` unset. The agent boots anyway on purpose, so it can tell you rather than crash-loop |
| `/sre doctor` shows argocd configured but every command times out | the instance has no Direct VPC egress, so it cannot reach the internal load balancer. Delete and recreate it so `--network`/`--subnet` apply |
| `/sre` returns `dispatch_failed` | the ack did not land in 3s. The socket loop acks before dispatching, so suspect a dead WebSocket rather than a slow handler |
| Bot shows offline, no logs | `SLACK_APP_TOKEN` missing or lacking `connections:write`, Socket Mode never connected |
| `missing_scope` in logs | reinstall the app after editing the manifest, scope changes need a fresh install |
| `invalid_auth` right after storing a secret | `echo` instead of `printf`, the trailing newline is part of the secret |
| Pods in `ImagePullBackOff` | the applications were registered before the images were built. Rerun `./02-stack.sh` |
| Drift found for a service that does not exist | the Argo CD application is missing its labels and the name split guessed wrong |
| `no newTag found for service` | the overlay pins by digest, which has no tag to edit |
| Agent silent after ~7 days | instances auto-restart at the 7-day mark. Anything that must survive that has to live outside the container |
| Instance URL returns 404 | expected, that is `--ingress=internal` |

---

## Reference

<details>
<summary><b>Everything is in us-east1</b></summary>

One region, deliberately. As of this writing, Cloud Run instances are available in select regions, including `us-east1`.

GKE, Artifact Registry, Cloud
Build and Vertex are all available in this region, so they follow the constrained component
rather than the other way round. Co-locating also keeps image pulls and the agent's Argo
CD calls in-region.

The region lives in exactly one place, `deploy/env.sh`. Nothing else hardcodes it.
</details>

<details>
<summary><b>On the command naming</b></summary>

The command is `/sre`. Finding drift between environments and opening
promotion PRs is CD and SRE work, and there is no continuous integration anywhere in it.

Everything is noun-verb, the convention `gcloud` uses (`gcloud run services describe`).
Noun-first groups by the thing being operated on, so someone who learns `drift` discovers
`list`, `debug` and `fix` for free. Verb-first scatters those three across the alphabet,
and the surface only grows from here: `service list` and `service get` slotted in without
a single existing command changing shape.

Non-canonical spellings still resolve, and the agent names the canonical form once so the
alias teaches rather than merely tolerating:

| You type | It runs | Why it is accepted |
| --- | --- | --- |
| `list services` | `service list` | verb-first, the kubectl reflex |
| `get service <svc>` | `service get <svc>` | same |
| `describe service <svc>` | `service get <svc>` | same |
| `debug-drift` | `drift debug` | the original hyphenated set |
| `fix-drift <svc>` | `drift fix <svc>` | same |
| `services`, `versions`, `status <svc>` | the obvious thing | bare conveniences |

`TestEveryAliasPointsAtARealCommand` keeps that table from rotting.
</details>

<details>
<summary><b>Adding a second service</b></summary>

The demo focuses on `product-catalog`, but nothing in the agent is specific to it.
Services are **discovered, never configured**: the inventory is the union of what the
overlays declare and what Argo CD reports. There is no service list in the config, no
registry to update, and no redeploy of the agent.

Onboarding `checkout` is two edits and no Go:

1. **Add it to each overlay's `images` block.** The service name the agent uses is the
   last path element of the image reference, so `ghcr.io/ofirc/checkout` becomes
   `checkout`.

   ```yaml
   images:
     - name: product-catalog
       newName: ghcr.io/ofirc/product-catalog
       newTag: v1.1.0
     - name: checkout
       newName: ghcr.io/ofirc/checkout
       newTag: v2.4.1
   ```

2. **Add an Argo CD Application per environment**, carrying the two labels. Copy
   `deploy/argocd/application-prod.yaml` and change the name, path and labels.
   `app.kubernetes.io/name` must match the image's last path element, or the desired and
   actual sides will not join and the service will show up twice, once as `missing` and
   once as `undeclared`.

Then `service list`, `version get`, `drift list` and the triage all pick it up on the
next command.

Two things worth knowing before the inventory gets large:

- **`version get` stops being readable past roughly a dozen services**, because it is a
  matrix with one column per environment. That is why `service list` exists: one row per
  service, the production version, and a status.
- **`gather` makes one Argo CD call and one GitHub call per environment**, not per
  service, so the cost is flat in the number of services. The fan-out that would bite is
  Grafana and JIRA, which is why only `drift debug` and `service get` call them, and only
  for one service at a time.
</details>

---

## Cleanup

Instances bill continuously, and an Autopilot cluster is not free either.

```bash
gcloud beta run instances delete sre-agent --project=$PROJECT --region=us-east1
gcloud container clusters delete sre-demo  --project=$PROJECT --region=us-east1
```
