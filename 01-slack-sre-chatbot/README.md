# 01 - Slack SRE ChatOps Agent

A production-shaped ChatOps agent that answers deployment questions, finds release drift
between environments, and opens pull requests to fix it.

```
/sre service list                 every service it knows about, and where each one runs
/sre service get <service>        full detail for one service, kubectl describe style

/sre version get [env]            deployed version of every service, per environment
/sre version diff <envA> <envB>   compare two environments side by side

/sre drift list [env]             where git and the cluster disagree, facts only, read-only and fast
/sre drift debug [service]        the model investigates git + Argo CD + Grafana + JIRA -> root cause  (restricted)
/sre drift fix <service> [env]    the model proposes an overlay edit, you confirm, then a PR opens  (restricted)

/sre rollout status <svc> [env]   Argo CD sync and health for one service
/sre doctor                       which integrations are wired up  (restricted)
/sre help
```

Restricted commands run only for the Slack users whose email is in `AUTHORIZED_EMAILS`.

Architecture: Slack over Socket Mode into an always-on agent on a Cloud Run instance,
fanning out to Grafana, JIRA, GitHub and Argo CD over REST. No inbound control path:
the only listener is a health endpoint Cloud Run requires. Secrets come
from Secret Manager, identity from the attached service account. `drift debug` and
`drift fix` are driven by a Vertex AI model that calls those providers as read-only
tools, and can run short scripts in a Cloud Run sandbox with no network. To try it with
no cloud account, the same agent also runs against a local kind cluster and a local
Ollama model, see [Run it locally on kind](#run-it-locally-on-kind).

```
User -> Slack -> [ SRE Agent (Socket Mode) ] -> Argo CD -> GKE
                        |      |     |
                        |      |     +-> GitHub (desired state, fix PRs)
                        |      +-------> Grafana (is anyone hurt?)
                        |              > JIRA   (has anyone complained?)
                        +-> Secret Manager + Vertex AI (a model is required for debug and fix)
```

---

## Quickstart

The whole thing, in order. Each step's output feeds the next, so run them in sequence.
Roughly 25 minutes, most of it waiting on the Autopilot cluster. To run it on your own
machine instead, with no GCP project, see
[Local Development (Docker Compose)](#local-development-docker-compose).

```bash
# 0. Auth
gcloud auth login --update-adc
gcloud config set project <your-project-id>   # every script reads this

# 1. Slack app: paste manifest.yaml at api.slack.com/apps, install it, copy both tokens
cd 01-slack-sre-chatbot
pbcopy < manifest.yaml
cp .env.example .env && vim .env     # SLACK_BOT_TOKEN, SLACK_APP_TOKEN, GITHUB_TOKEN, AUTHORIZED_EMAILS

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

## Local Development (Docker Compose)

The same demo on your machine: a kind cluster running Argo CD and the three
`product-catalog` environments, the agent in a container, and a local model served by
Ollama. No GCP project, no Cloud Run, nothing to tear down in the cloud. Run the steps in
order.

```bash
# 0. Tools (macOS). Start Docker Desktop once it is installed.
brew install --cask docker
brew install kind kubectl ollama
brew services start ollama
ollama pull qwen3:14b                # up.sh turns this into sre-qwen3:14b (32K context)

# 1. Slack app: same as the Quickstart. Skip if .env already has the tokens
cd 01-slack-sre-chatbot
pbcopy < manifest.yaml
cp .env.example .env && vim .env     # SLACK_BOT_TOKEN, SLACK_APP_TOKEN, GITHUB_TOKEN, AUTHORIZED_EMAILS

# 2. The local stack
./deploy/local/up.sh                 # kind + Argo CD + images + model + .env.local. Safe to rerun
docker compose up --build            # the agent, in a container

# 3. In Slack
/sre doctor
/sre drift list

# 4. Stop it
docker compose down                  # the agent
./deploy/local/down.sh               # the cluster
```

`up.sh` also stops the Cloud Run agent if it is running, because two agents on one Slack
app would split the commands between them. `down.sh` prints how to start it again.

Code runs default to a shim that uses the same runner code as production but gives **no
isolation**. For isolated code runs through Docker, start the agent with the override
instead:

```bash
docker compose -f compose.yaml -f compose.docker-sandbox.yaml up --build
```

**Before the first run:** `gitops/overlays-local/` must be on the `main` branch on GitHub,
because Argo CD inside kind reads the overlays from GitHub, not from your disk. Run
everything from `01-slack-sre-chatbot/`.

The full walkthrough, with what each step does, the `go run` alternative, the shim's
trade-offs and troubleshooting, is in [Run it locally on kind](#run-it-locally-on-kind).

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
vim .env          # SLACK_BOT_TOKEN, SLACK_APP_TOKEN, GITHUB_TOKEN, AUTHORIZED_EMAILS
```

**`AUTHORIZED_EMAILS` decides who may drive the agent.** `drift debug`, `drift fix`,
`doctor` and the *Open this PR* / *Cancel* buttons are restricted: the bot looks up the
caller's Slack email (`users.info`, scope `users:read.email`) and runs the command only
if it is in this comma-separated list. Everyone else gets a message only they can see,
and the attempt is logged with their Slack user ID. Left empty, **nobody** can run them:
the gate fails closed. The read-only commands (`drift list`, `service`, `version`,
`rollout status`, `help`) stay open to the whole workspace. Keep the addresses in `.env`,
never in the repo: `04-agent.sh` passes them to Cloud Run, and `up.sh` copies them into
`.env.local` for the local stack.

```bash
AUTHORIZED_EMAILS=you@example.com,you@work.example
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

That is also why the only listener in the Dockerfile is a health check, and why
`--ingress=internal` costs us nothing in step 7.
</details>

<details>
<summary><b>Why each scope is in the manifest</b></summary>

| Scope | Earns its place because |
| --- | --- |
| `commands` | the `/sre` slash command |
| `app_mentions:read`, `channels:history`, `groups:history` | `@sre-agent drift list` and reading the surrounding thread for triage context |
| `chat:write`, `chat:write.public` | posting results, including to channels the bot was never invited to |
| `files:write` | long fix diffs, the code the model runs, and the tool trail exceed Slack's 3000-character block limit routinely |
| `reactions:write` | progress signalling on a triage with a two-minute p50 |
| `im:history`, `im:read`, `im:write` | DM conversations with the agent |
| `users:read`, `users:read.email` | the authorization gate: the caller's email is matched against `AUTHORIZED_EMAILS` before a restricted command runs. Also maps the caller to a GitHub and JIRA identity for the audit trail |
| `assistant:write` | the assistant side panel |
| `channels:manage`, `groups:write` | "open temporary Slack rooms for shared triage", from the below-the-line requirements |

`interactivity` is on because `drift debug` ends in buttons and `drift fix` asks you to
confirm with one. `always_online: true` is honest here, the agent really is always on.

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
    newTag: v1.1.0          # the line /sre drift fix normally changes
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

**Use two terminals.** Backgrounding the port-forward with `&` looks tidier and will
bite you: it dies with the shell, or silently loses its connection, and every Argo CD
call then fails in a way that reads like a bad token. Keep it in the foreground where
you can see it.

```bash
# Terminal A - the tunnel to Argo CD. Leave it running.
pkill -f 'port-forward.*argocd-server'        # clear any zombie holding port 8080
kubectl port-forward -n argocd svc/argocd-server 8080:443
```

```bash
# Terminal B - the agent
cd 01-slack-sre-chatbot
set -a && source .env && set +a
ARGOCD_BASE_URL=https://localhost:8080 go run ./cmd/agent
```

The `ARGOCD_BASE_URL` override is load-bearing. `.env` holds the internal load balancer
address from step 5, which your laptop cannot route to.

Wait for this line before touching Slack:

```json
{"msg":"starting","bot":"sre-agent","team":"Harness Engineering","envs":"test,stage,prod",
 "github":true,"argocd":true,"grafana":false,"jira":false,
 "model_provider":"vertex","model":"gemini-2.5-pro","sandbox":"none"}
{"msg":"connected to slack with socket mode"}
```

`github:true` and `argocd:true` are the two that matter for the read-only commands.
`model_provider` and `model` are the third if you want `drift debug` and `drift fix`:
they are run by the model, and without one they say they need a model rather than
guess. Grafana and JIRA are optional, and the agent says so in its answer when they are
missing.

Locally that means `VERTEX_PROJECT` set in `.env` and `gcloud auth application-default
login`. `sandbox:"none"` is expected on a laptop that has neither: the Cloud Run sandbox
only exists on Cloud Run, so the model investigates without being able to run code. If
Docker is running and an image named by `SANDBOX_IMAGE` exists, the default `SANDBOX=auto`
picks the Docker runner instead and this says `docker`. The local stack sets
`SANDBOX=shim`, and this says `shim` (see
[Run it locally on kind](#run-it-locally-on-kind)).

### Where to type the commands

**Use a channel, not the app's DM, and never your own self-DM.**

| Where | Works? | Why |
| --- | --- | --- |
| A channel such as `#all-harness-engineering` | yes | the bot has `chat:write.public`, so it can post without being invited |
| DM with `sre-agent` | only after enabling the messages tab | Slack shows *"Sending messages to this app has been turned off"* unless the manifest declares `app_home.messages_tab_enabled` |
| Your own self-DM (*"Ofir Cohen (you)"*) | **no** | the bot is not a participant and cannot post there |

The self-DM failure is the confusing one, because the command *does* reach the agent and
run. Only the reply fails, so Slack looks silent while the log shows:

```json
{"level":"ERROR","msg":"cannot post anchor message","err":"channel_not_found","channel":"D0..."}
```

If you see that, you are typing in the wrong conversation. Nothing is wrong with the agent.

### Try it

```
/sre doctor
```

Confirms which integrations came up, including `model`, `sandbox` and the `skills` this
build carries. This is the first thing to run after any change, and the fastest way to
tell a configuration problem from a code problem. The full output is in step 8.

```
/sre drift list
```

```
1 difference
ENV   SERVICE          GIT     RUNNING  SYNC    HEALTH   NOTE
prod  product-catalog  v1.1.0  v1.1.0   Synced  Healthy  git v1.1.0, stage git v1.2.0

/sre drift debug product-catalog to triage
```

One row: prod is on `v1.1.0` while stage's overlay already has `v1.2.0`. The command
shows facts and nothing else. It does not say what kind of problem this is, because that
is a judgement and it is the model's to make, not the code's.

```
/sre drift debug product-catalog
```

The reply is in step 8. Four things about it are worth pointing at during a demo:

- **"Nothing failed. The release was simply never promoted"** is the model's reading, not
  a code path. Both environments match their own overlay and the overlays disagree with
  each other, which the `drift-triage` skill teaches it to recognise as a release nobody
  promoted rather than one that failed.
- **The suggested fix goes forward**, `v1.1.0 -> v1.2.0`. For a failed sync it would go
  *backwards* instead, pinning git to the version actually running. That rule is in
  `skills/gitops-fix/references/direction.md`, a markdown file you can read and edit.
- **"User impact unknown, Grafana is not wired up"**: the skill requires that exact
  sentence, so a missing integration reads as unknown rather than as fine.
- **Show your work.** The thread under the reply lists every tool the model called, with
  its arguments, and any code it ran appears there with its output. That reply is the
  audit record of how the answer was reached.

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
  drift.go                     drift list, drift debug, drift fix: facts, then the agents
  apply.go                     the hidden drift apply behind Open this PR and Cancel
  interactions.go              buttons, routed back through Dispatch
internal/drift/                the fact table: git versus cluster, per env and service.
                               Pure comparison, no severity, no I/O
internal/agentloop/            the ADK agents: model, tools, skills, run budget, proposals
internal/sandbox/              runs the model's Python and bash: Cloud Run sandboxes
                               (sandbox.go), or throwaway Docker containers through the
                               Engine API on the Docker socket (docker.go)
skills/                        drift-triage and gitops-fix, embedded into the binary
deploy/                        01-04 and 99: the GKE and Cloud Run scripts
deploy/local/                  up.sh, down.sh, kind.yaml, Argo CD apps: the local kind stack;
                               sandbox-shim stands in for the Cloud Run CLI (no isolation)
deploy/lib/                    Argo CD shell helpers shared by 03-argocd.sh and up.sh
gitops/overlays-local/         the same overlays, for the local stack (1 replica each)
compose.yaml                   the agent as a container beside the kind cluster (shim runner)
compose.docker-sandbox.yaml    opt-in override: the Docker runner and the socket it needs
internal/providers/
  httpx/                       one place for timeouts, retries, error redaction
  argocd/  github/  gitops/    actual state, PRs, desired state
  grafana/ jira/               symptoms and reports
```

```bash
go build ./... && go test ./...
```

Those tests need no network and no model. The checks on what the model actually
concludes (a promotion proposes moving forward, a failed sync pins back) run against real
Vertex behind a build tag:

```bash
VERTEX_PROJECT=<project> go test -tags=eval -timeout 30m ./internal/agentloop/ -run TestEval -v
```

They cost money and the model is not deterministic, so they assert what a correct answer
must contain rather than its wording. If the default model (`gemini-2.5-pro`) is not
served to your project, set `VERTEX_MODEL` to one that is, for the checks and for the
agent.

The same checks run against a local model, with no Google project and no cost, but they
need Ollama running with the model pulled. They skip themselves if Ollama is not
reachable:

```bash
MODEL_PROVIDER=ollama go test -tags=eval -timeout 60m ./internal/agentloop/ -run TestEval -v
```

A 14B model on a laptop is slower and less reliable than Gemini, so a failure here is
information about the model, not a broken build.

**Ack before you work.** Slack gives a slash command three seconds. The socket loop acks
immediately and hands off to a goroutine, so a four-minute triage and a 200ms version
lookup are the same code path:

```go
case socketmode.EventTypeSlashCommand:
    cmd, _ := evt.Data.(slack.SlashCommand)
    client.Ack(*evt.Request)                      // always first
    go router.Dispatch(ctx, cmd.ChannelID, cmd.UserID, cmd.UserName, cmd.Text)
```

**The model investigates, but every tool it has only reads.** Go computes the fact
table, gives the disagreeing rows to the model, and the model decides what else to look
at: the overlay, Argo CD, Grafana, JIRA. Its tools are `list_disagreements`,
`get_service_facts`, `refresh_app`, `get_overlay`, `get_service_health` and
`search_tickets`, plus `run_code` when the sandbox exists. None of them changes git or the
cluster, so a bad generation costs a wrong paragraph, not an unexpected write against
prod. A run is bounded: 20 model calls for debug, 12 for fix, and at most 5 code runs.
There is no rule-based fallback. If the model is unavailable (Vertex, or Ollama on the
local stack) the command fails and says so, instead of answering with logic that was
never the model's.

**The knowledge lives in skills, not in Go.** What the kinds of drift are, how to rank
them, how to word a root cause and which way a fix goes are markdown files under
`skills/`, embedded in the binary and loaded by the agent on demand. They are reviewed
like code and cost no Go change to correct. The direction rule is the one that matters
and the easiest to get backwards: a promotion moves the lagging overlay *forward*, and a
skew or failed sync pins the overlay *back* to what is running, because rolling forward
needs the broken image fixed first. It is written down, with the reasoning, in
`skills/gitops-fix/references/direction.md`, and the eval checks above are what verify
the model follows it.

**Model-written code runs in a sandbox with nothing to steal.** When the model decides a
calculation over the evidence would change its conclusion, `run_code` writes the script
to a per-run directory and runs it with `sandbox do` on Cloud Run (or in a throwaway
Docker container locally, with the same limits): no network, no environment
variables, the image's filesystem read-only, and the evidence files mounted read-only at
`/data`. Python standard library and bash only, 60 seconds and 16 KB of output per call.
Every script and its output is posted in the thread. That is why the image has `python3`
and `bash` and why nothing secret can be written to disk.

**The fix is a proposal, and a human confirms it.** `drift fix` runs a second agent that
reads the overlay and ends in exactly one of `propose_fix` or `decline_fix`. Declining
is a normal answer: an unhealthy workload on the right version is not fixed by a tag.
A proposal is checked before it is shown: it must be a changed, valid kustomization
for an allowed environment, and `resources`, `namespace`, the `images` names and the
`part-of` label patch must be unchanged. Nothing is written to GitHub yet. Slack shows
the full diff and the PR description with **Open this PR** and **Cancel**, and only the person who ran the
command can click either. Opening the PR re-reads the overlay and refuses if it changed
since the proposal, and the proposal is single-use and expires after 30 minutes. The PR
body says it was model-proposed and human-confirmed. The agent still never merges.
</details>

---

## Run it locally on kind

The whole demo on this machine, with no GCP project: a kind cluster running Argo CD and
the three `product-catalog` environments, the agent in a container (or under `go run`),
and a local model served by Ollama. It uses the same Slack app, the same GitHub repo and
the same agent code as the cloud path, so it is also the cheapest way to change the agent
and see the result.

**Prerequisites.**

- Docker Desktop, running.
- `kind`, `kubectl`, `python3` and `curl`. `gcloud` is optional: with it, `up.sh` can stop
  the Cloud Run agent for you (below).
- [Ollama](https://ollama.com), running, with the base model pulled:

  ```bash
  ollama pull qwen3:14b
  ```

- A `.env` in this directory with `SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN` and `GITHUB_TOKEN`
  (step 2). Nothing else in it is needed.
- `gitops/overlays-local/` on the `main` branch of the GitHub repo. Argo CD inside kind
  renders the overlays from GitHub, not from your disk, so until that folder is merged
  the three applications show `ComparisonError`.

**Start it.**

```bash
cd 01-slack-sre-chatbot
./deploy/local/up.sh            # once. Safe to rerun
docker compose up --build       # the agent, in a container
```

Code runs default to the **shim**: the same Cloud Run runner code as production, with
**no isolation** (see "How code runs locally" below). For isolated code runs through the
Docker runner instead:

```bash
docker compose -f compose.yaml -f compose.docker-sandbox.yaml up --build
```

`up.sh` checks Docker, creates the `sre-qwen3:14b` model variant (below), stops the Cloud
Run agent, creates the `sre-local` kind cluster, installs Argo CD, builds and loads
`product-catalog` as `v1.1.0` and `v1.2.0`, builds the agent image as `sre-agent:local`,
registers the applications, mints the `sre-agent` token and writes `.env.local`. It does
not start the agent, so you can rebuild and restart the agent without touching the
cluster. It stops at the first thing that is wrong and says what to do.

Wait for `connected to slack with socket mode` in the compose output, then type
`/sre doctor` in a channel. Locally the model and sandbox rows read:

```
✅ model (ollama, sre-qwen3:14b) - required for drift debug and drift fix
✅ sandbox (shim, no isolation) - code execution for the agent
```

With the Docker runner override, the sandbox row reads `sandbox (docker)` instead.

`/sre drift list` then shows prod behind stage, as in step 6.

To run the agent on your machine instead of in a container, from this directory:

```bash
set -a && source .env.local && set +a && go run ./cmd/agent
```

`.env.local` already holds the host-side values (`https://localhost:8443` for Argo CD,
and `SANDBOX=shim` with `SANDBOX_CLI` pointing at `deploy/local/sandbox-shim`), so no
overrides are needed. Ollama defaults to `http://localhost:11434/v1`. This is the simplest
route when you are changing code. Code runs use the shim here too; add `SANDBOX=docker`
to the command line for the Docker runner.

**Stop it.**

```bash
docker compose down             # the agent
./deploy/local/down.sh          # the cluster. Also runs `docker compose down` if needed
```

`down.sh` leaves `.env.local`, `.local/runs`, the built images and the Ollama models in
place, and prints how to start the Cloud Run agent again.

**Two things that catch people out.**

- **`.env.local` is regenerated from `.env` on every `up.sh` run.** Hand edits are lost.
  It keeps a still-valid `ARGOCD_TOKEN`, and nothing else you changed. To try a different
  setting, export it in the shell you run the agent from instead (with `go run`, after
  sourcing `.env.local`). A setting `up.sh` does not own, such as `ALLOW_FIX_ENVS`, can go
  in `.env` and reaches `.env.local` on the next run. The ones `up.sh` sets itself (the
  model, sandbox, Argo CD and GitOps values) are overwritten from `.env`, so give those to
  `up.sh` in its environment: `OLLAMA_MODEL=sre-qwen3:8b ./deploy/local/up.sh`.
- **Run `docker compose` from `01-slack-sre-chatbot/`.** That is where `.env.local` is.
  With the Docker runner override it matters more: `compose.docker-sandbox.yaml` mounts
  `${PWD}/.local/runs` at the same absolute path in the container. From another directory
  the mount lands somewhere else, the Docker runner reads an empty folder, and doctor
  shows `sandbox (docker)` as ❌.

If `docker compose up` fails on a missing `kind` network, the cluster is not up: run
`up.sh` first. kind creates that network, and `compose.yaml` joins it so the agent can
reach Argo CD at `https://sre-local-control-plane:30443`.

<details>
<summary><b>How code runs locally: the shim by default, Docker as an opt-in</b></summary>

**The shim (default).** `SANDBOX=shim` makes the agent use the same Cloud Run runner code
as production: the same arguments, timeouts, output truncation and startup probe. The
difference is that the runner calls `deploy/local/sandbox-shim` instead of the Cloud Run
sandbox CLI. `compose.yaml` mounts the shim where Cloud Run puts the CLI, and `.env.local`
points `go run` at it. The shim rewrites `/data/...` paths, in the command line and in the
code file, to the run's evidence folder, then runs the interpreter.

**It provides no isolation.** The model's code runs with network access, wherever the
agent runs, as the agent's user. The runner gives it an empty environment and the shim
starts it in the run's evidence folder, but neither is a boundary:

- **In the container** (`docker compose up`), code running as the agent's user can read
  the agent's environment from `/proc/<pid>/environ`. That environment holds the Slack,
  GitHub and Argo CD tokens, which locally are the same real credentials the cloud uses.
- **With `go run` on your Mac** it is wider still: the code runs as *you*. It can read
  anything you can, including your home directory, `~/.kube/config`, your cloud CLI
  credentials and this directory's `.env.local`, and it can write to them.

A prompt injection that gets the model to write a few lines of Python could send any of
that anywhere. The injection surface is small locally (JIRA is off, and Argo CD messages
come from your own kind cluster), so the shim is a reasonable default for working on the
agent. But use a separate, narrowly scoped GitHub token for local runs (fine-grained,
this one repository, contents and pull requests only), prefer the container over
`go run` when you are not changing code, and switch to the Docker runner when you want
isolation.

One limit: the `/data` rewrite inside the code is textual. It covers `/data` as a path
next to quotes, whitespace and shell or Python punctuation, but code that builds the path
some other way, such as `os.path.join("/", "data")`, does not find its files and fails
with a file-not-found error in the thread.

The agent recognises the shim by a marker line in the script, not by `SANDBOX`, so
wherever `SANDBOX_CLI` points at it doctor says `shim, no isolation`, and `SANDBOX=auto`
never picks it.

**If doctor shows `sandbox (shim, no isolation)` as ❌**, the startup probe could not run
Python through the shim. Check that `deploy/local/sandbox-shim` is executable, and with
`go run`, that `SANDBOX_CLI` in `.env.local` points at it and `python3` is on your `PATH`.

**The Docker runner (opt-in).** `compose.docker-sandbox.yaml` mounts `/var/run/docker.sock`
into the agent container. That is how the
Docker code runner starts a throwaway container for each `run_code` call, and it means
**anything that controls the agent process controls the host's Docker**, which is close to
root on this machine. This is the trade-off for running code locally, and it is acceptable
for a demo on your own laptop. It is not acceptable on a shared host.

What it does not mean: the model's code never runs in the agent container. Each run is a
sibling container from the same `sre-agent:local` image with no network, no environment
variables, a read-only root filesystem, all capabilities dropped, 256 MB and one CPU, and
the run's evidence folder mounted read-only at `/data`. It runs as the agent's own uid and
never as root, and the image's entrypoint is replaced so it starts the interpreter, not the
agent. The agent itself holds your Slack, GitHub and Argo CD tokens, but none of them are
passed to those containers.

The agent container runs as the image's non-root user, `65532:65532`, and
`compose.docker-sandbox.yaml` adds the least that makes two things work:

| What | How | Why |
| --- | --- | --- |
| Reading the socket | `group_add: ["0"]` | Docker Desktop usually presents the socket inside containers as `root:root`, mode 660. Group 0 lets uid 65532 open that one file. It grants no other file access and no capabilities |
| Writing run directories | `up.sh` runs `chmod 1777 .local/runs` | the agent is uid 65532, not you, and must create a directory there per run. `1777` is the same mode as `/tmp`: anyone can create entries, only the owner can delete their own, and you own the folder so you can clear it. Each run directory is created `0700`, and holds evidence only, never a credential |

**Not verified.** Both were written against the usual Docker Desktop for Mac behaviour
without being run on it, because the daemon was not available when they were written. Check
them on your machine:

```bash
docker compose exec agent ls -ln /var/run/docker.sock
```

The output shows the socket's owner and group as numbers. If the group is `0`, nothing to
change. If it is another number, put that number in `group_add` in
`compose.docker-sandbox.yaml`.

**If doctor shows `sandbox (docker)` as ❌**, the agent could not run a probe container.
`docker compose logs agent` says `docker sandbox unavailable`, and its `err` says why. The
causes, most likely first:

1. *The socket is not readable by the agent* (the check above). Fix the `group_add`
   number. If the socket is not group-accessible at all, a different group cannot help,
   and running the agent as root does not either: the per-run directories are `0700` and
   owned by the user that made them, so code containers, which are never root, could
   not read a root-owned one. Use the `go run` route instead.
2. *`sre-agent:local` is missing.* Rerun `up.sh`, which builds it.
3. *The folder is not shared.* Docker resolves the code container's bind mount on the
   host, so `.local/runs` must be a path Docker Desktop shares (anything under `/Users`
   is, by default) and compose must have been run from this directory, as above.
4. *Docker is too old.* Docker Engine API older than 1.44 (Docker Engine < 25) is not
   supported, and the reason appears in the agent log.

A failed probe is not fatal. The agent still boots and the model still works, it just has
no `run_code` tool until you fix the cause and restart it.
</details>

<details>
<summary><b>The Cloud Run instance is stopped, and how to start it again</b></summary>

The local agent connects to the same Slack app as the Cloud Run one, and Socket Mode lets
one process hold that connection. So `up.sh` runs `gcloud beta run instances stop` on the
`sre-agent` instance (name, region and project come from `deploy/env.sh`) when `gcloud`
is signed in and the instance exists. If it cannot tell, it prints a warning instead of
guessing: stop the cloud agent yourself before starting the local one.

Nothing starts it again automatically, because bringing back a bot connected to a shared
Slack app is a decision. When you are done:

```bash
gcloud beta run instances start sre-agent --region us-east1 --project <project>
```

`down.sh` prints that command with your values filled in.
</details>

<details>
<summary><b><code>overlays-local</code>, and re-staging the drift after a fix is merged</b></summary>

The local applications point at `gitops/overlays-local/<env>`, not `gitops/overlays`. The
two sets have the same shape and the same staged drift (test and stage on `v1.2.0`, prod
on `v1.1.0`), but local replicas are 1 everywhere to keep kind light. Keeping them apart
means a fix PR opened from the local stack (`GITOPS_PREFIX` in `.env.local` points the agent
at `overlays-local`) touches only `overlays-local`, and never the overlays the cloud
cluster syncs.

Those PRs are real PRs on GitHub. Once one is merged, prod is no longer behind and
`drift list` has nothing to show. To get the drift back, **revert that PR on GitHub**,
and Argo CD syncs the local prod back to `v1.1.0` on its own.
</details>

<details>
<summary><b>What differs from the cloud, and the local model</b></summary>

- **Model quality.** `qwen3:14b` on a laptop is slower and less reliable than Gemini. Give
  `drift debug` a few minutes, and expect some answers to be worse or to fail with a
  readable error. Failures are information about the model, not about the agent.
- **A larger context window.** Ollama's default context is far smaller than the debug
  prompt (instructions, skill text, tool schemas, facts, overlays), and its OpenAI-style
  endpoint cuts an overlong prompt from the front without saying so, so the model answers
  without having seen its instructions. The endpoint cannot set the context per request,
  so `up.sh` creates a variant: `sre-qwen3:14b`, from a Modelfile containing
  `FROM qwen3:14b` and `PARAMETER num_ctx 32768`. `OLLAMA_MODEL` defaults to it. For a
  different base model, make one the same way and set `OLLAMA_MODEL` to its name:

  ```bash
  printf 'FROM qwen3:8b\nPARAMETER num_ctx 32768\n' > /tmp/Modelfile
  ollama create sre-qwen3:8b -f /tmp/Modelfile
  ```

- **How the debug answer is returned.** Gemini returns it against an output schema. A
  non-Gemini model finishes by calling a `submit_triage` tool instead, which is checked the
  same way, so the Slack reply looks the same.
- **The shim, or Docker, instead of Cloud Run sandboxes.** The shim runs the same runner
  code with no isolation; the Docker runner keeps the isolation, as described above.
  `/sre doctor` names the runner: `sandbox (shim, no isolation)` or `sandbox (docker)`
  locally, `sandbox (cloudrun)` in the cloud.
- **No Grafana and no JIRA.** `up.sh` leaves them unset, so `drift debug` says it could not
  check user impact, which is the honest answer. Set them in `.env` only if you can reach
  them from here.
- **The first prompt is slow.** Ollama loads the model into memory on first use.

The model checks run against Ollama too (see step 6 for what they assert and the Vertex
command):

```bash
MODEL_PROVIDER=ollama go test -tags=eval -timeout 60m ./internal/agentloop/ -run TestEval -v
```

They skip themselves if Ollama is not reachable.
</details>

---

## 7. Deploy the agent

```bash
cd deploy
./04-agent.sh
```

Reads `.env` itself, upserts every token into Secret Manager, builds the image with
Cloud Build, creates the Cloud Run instance with `--sandbox-launcher` and 1Gi of memory,
and waits until the agent logs a Slack connection. It sets `VERTEX_PROJECT` for you, since
`drift debug` and `drift fix` cannot run without it.

Run `/sre doctor` afterwards. `model`, `sandbox` and `skills` should all be green.

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

- `--ingress=internal` plus `--port=8080`. The port is not optional: Cloud Run rejects
  the create with *"Ingress settings require a single container with a port"*. The agent
  is otherwise outbound-only, so that listener serves nothing but `/healthz` and is
  unreachable from the internet. No Slack traffic arrives inbound either way, commands
  come down the outbound WebSocket.
- `--network` / `--subnet` give Direct VPC egress, which is the only reason the agent can
  reach the Argo CD internal load balancer.
- `--set-secrets` keeps tokens out of the image and out of `describe` output. The
  instance authenticates as its attached service account, so there is no JSON key to
  create, store, rotate or leak.
- `--sandbox-launcher` makes the Cloud Run sandbox CLI available, which is what lets
  `drift debug` run the model's scripts with no network and a read-only filesystem.
  The image is Debian slim with `python3` and `bash`, because the sandbox can only
  launch interpreters that exist in it. Without the flag the agent still starts, and
  `/sre doctor` shows `sandbox` as ❌.
- `--cpu=1 --memory=1Gi` overrides the 2 vCPU / 2048Mi default, roughly double the
  config behind the commonly quoted price. Instances bill continuously. 1Gi rather than
  512Mi because the sandboxes share the instance's memory with the agent.
- `--service-account` holds only `secretAccessor` and `aiplatform.user`. No GKE access
  at all. `aiplatform.user` is what the model calls need.
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
✅ model (vertex, gemini-2.5-pro) - required for drift debug and drift fix
✅ sandbox (cloudrun) - code execution for the agent
✅ skills   - drift-triage, gitops-fix

Environments test, stage, prod
Fixable environments stage, prod
GitOps overlays 01-slack-sre-chatbot/gitops/overlays/<env>/kustomization.yaml in ofirc/hello-agent
Remediation opens a PR, never merges
```

```
/sre service list
```

```
Services - 3 known, 1 with differences
SERVICE          ENVIRONMENTS     PROD    STATUS
checkout         stage,prod       v2.4.1  in sync
payments         test,stage,prod  v0.9.3  in sync
product-catalog  test,stage,prod  v1.1.0  differs: prod

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

Differences
• prod: git v1.1.0, stage git v1.2.0

Grafana (prod, 6h) 0.00% 5xx, p99 48ms, 12.4 rps over 6h0m0s
JIRA last 30 days
    - SRE-412 product catalog missing the new filters (Open)

/sre drift debug product-catalog to triage
```

`RUNNING` and `GIT` agree in every row, and the only difference is between two overlays.
The static commands print that and stop. They do not name it, rank it or say what to do,
so what you read is what the data says. `service list` shows `in sync` or the
environments that differ, and `service get` looks at Grafana and JIRA for the most
production-like environment that differs.

```
/sre drift debug product-catalog
```

```
🔍 Triage: product-catalog

Facts
• prod: git v1.1.0, stage git v1.2.0
• Argo CD prod/product-catalog: sync=Synced health=Healthy
• Git overlay prod: 01-slack-sre-chatbot/gitops/overlays/prod/kustomization.yaml
• Grafana (prod, 6h): 0.00% 5xx, p99 48ms, 12.4 rps over 6h0m0s
• JIRA, last 30 days (1): SRE-412 product catalog missing the new filters

🧠 Root cause
- product-catalog in prod is on v1.1.0 while stage already runs v1.2.0.
- Nothing failed. The release was simply never promoted, so the overlay still pins
  the older tag. SRE-412 already reports the missing filters.
- No user-visible errors (0.00% 5xx, p99 48ms, 12.4 rps). The old version is serving
  correctly, this is staleness rather than an outage.
- A pull request can fix this, see the suggested fix.

🔧 Suggested fix (prod)
promote prod to v1.2.0
stage already runs v1.2.0 and is healthy, so this promotes the same version to prod.

[ Open fix PR ]  [ Re-check ]  [ Grafana ]
```

The facts come first and are shown as fetched, so you can check the root cause against
what the model was looking at. The root cause and suggested fix are the model's. **Open
fix PR** runs `drift fix product-catalog prod`, a fresh run that proposes the actual edit.
**Grafana** appears only when `GRAFANA_BASE_URL` is set. With no fix suggested, the
reply says `No fix suggested.` instead.

While it works, the message keeps its last twelve steps, such as `Loading skill
drift-triage` and `Checking Grafana for prod/product-catalog`. When it ends, the thread
gets one more reply, which is the record of the run:

```
Tool trail:
load_skill({"name":"drift-triage"})
load_skill_resource({"skill_name":"drift-triage","resource_path":"references/kinds.md"})
get_service_facts({"service":"product-catalog"})
get_overlay({"env":"prod"})
get_service_health({"env":"prod","service":"product-catalog","hours":6})
search_tickets({"service":"product-catalog","days":30})
```

The calls are the model's choice, so yours will differ. A long trail is uploaded as a
file instead of posted inline.

```
/sre drift fix product-catalog
```

```
📝 Proposed change for product-catalog in prod
promote(prod): product-catalog v1.1.0 -> v1.2.0

--- a/01-slack-sre-chatbot/gitops/overlays/prod/kustomization.yaml
+++ b/01-slack-sre-chatbot/gitops/overlays/prod/kustomization.yaml
@@ -38,7 +38,7 @@
 images:
   - name: product-catalog
     newName: ghcr.io/ofirc/product-catalog
-    newTag: v1.1.0
+    newTag: v1.2.0
 
 # APP_VERSION is derived from the image tag rather than duplicated, so there is exactly
 # one place to change a version and the running pod can never disagree with the tag it

stage already runs v1.2.0 and is healthy. This promotes the same version to prod.

PR description
Opened by the SRE Agent.
## What drifted
prod/product-catalog is on v1.1.0, stage already has v1.2.0
...

[ Open this PR ]  [ Cancel ]
requested by @ofir | only they can confirm | the agent does not merge
```

Nothing has been written to GitHub at this point. The diff is the whole change, never a
summary, because reading it is the safeguard. The PR description the model wrote is
shown too, since it goes to GitHub under your confirmation. If either is too long for a
message, both are attached in the thread, and the buttons only appear once that upload
has worked. Click
**Open this PR**:

```
✅ PR opened #42 for product-catalog in prod

[ Review PR ]  [ Re-check drift ]
confirmed by @ofir | the agent does not merge, that stays with a human
```

Anyone else who clicks gets `Only @ofir can confirm this proposal.` and the proposal stays
pending. The PR title and body are the model's, with a footer saying it was
model-proposed and confirmed by you. After 30 minutes, or after a restart, the buttons
answer `That proposal expired or was already used`, and re-running `drift fix` is the
way forward.

If the model decides no overlay edit would help, for example a workload that is
unhealthy on the right version, you get `No PR proposed` with its reason and the facts,
and no buttons. That is a valid answer, not a failure.

The agent stops at the PR. Merging is a human decision, which is what makes the
flow safe to run in front of a room. Merge it live, let Argo CD auto-sync, and
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
built. Argo CD goes `OutOfSync` with `ImagePullBackOff`, so the row now carries the sync
message and the model reads it as a failed rollout, not a forgotten promotion. `drift fix`
should flip to proposing a rollback rather than a promotion. This is the
variant where `run_code` is most likely to appear, so check the thread for scripts and
their output.
</details>

---

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| `drift debug`, `drift fix` or `doctor` says "nobody is authorized yet" | `AUTHORIZED_EMAILS` is empty, so the gate refuses everyone. Set it in `.env` and rerun `./04-agent.sh` (cloud) or `./deploy/local/up.sh` (local) |
| "You are not authorized to use ..." for your own account | the email on your Slack profile is not in `AUTHORIZED_EMAILS`. Check the address Slack has (profile, then *Contact information*) |
| "I could not verify your Slack email" | `users.info` failed, your profile has no email, or the account is a bot, deactivated, or from another organization in a shared channel. A missing `users:read.email` scope shows up here: reinstall the app after manifest changes, and the next attempt picks it up |
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
| `drift debug` or `drift fix` says it needs the model | no model is configured: `MODEL_PROVIDER` is empty and `VERTEX_PROJECT` is unset. `04-agent.sh` sets `VERTEX_PROJECT` on Cloud Run. Locally, set it in `.env` and run `gcloud auth application-default login`, or use the Ollama stack below. If it says the model failed to start, `/sre doctor` shows `model (<provider>, <model>)` as ❌ and the logs say why. For Vertex, a model that is not served to the project is the usual reason, so set `VERTEX_MODEL` |
| `sandbox` is ❌ in `/sre doctor` | on Cloud Run: the instance predates `--sandbox-launcher` (rerun `./04-agent.sh`, which applies it), or the image lacks `python3` (the Dockerfile must install it). Locally: the shim (the default) or the Docker runner could not run its startup probe, see [Run it locally on kind](#run-it-locally-on-kind) for both. The agent still works, it just cannot run code |
| `That proposal expired or was already used` | the confirm button is single-use, valid for 30 minutes, and lost on a restart. Run `/sre drift fix <service>` again |
| `the overlay changed since the proposal` | someone edited that overlay between the proposal and the click, so the model's edit was made against text that no longer exists. Run `/sre drift fix <service>` again |
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
