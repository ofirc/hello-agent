//go:build eval

package agentloop

// These checks run the real model on fixed scenarios, so they replace the direction tests
// the rule engine used to have. They cost money and need credentials, so they sit behind
// the eval build tag and skip when VERTEX_PROJECT is unset:
//
//	VERTEX_PROJECT=<project> go test -tags=eval ./internal/agentloop/ -run TestEval -v
//
// VERTEX_LOCATION and VERTEX_MODEL default to the production defaults. The model is not
// deterministic, so each check asserts what a correct answer must contain, not its wording.
//
// MODEL_PROVIDER=ollama runs the same checks on a local Ollama instead, with no credentials:
//
//	MODEL_PROVIDER=ollama go test -tags=eval ./internal/agentloop/ -run TestEval -v
//
// OLLAMA_BASE_URL and OLLAMA_MODEL default to http://localhost:11434/v1 and sre-qwen3:14b.
// The checks skip when Ollama does not answer.

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/skills"
)

const evalService = "product-catalog"

var evalEnvs = []string{"test", "stage", "prod"}

// envState is what git and the cluster say about one environment.
type envState struct {
	Git, Running string
	Sync, Health string // default Synced, Healthy
	Message      string // Argo CD operation message, usually the failure reason
}

// scenario is a fixed state of the three environments.
type scenario map[string]envState

func (sc scenario) table() drift.Table {
	var desired []drift.Desired
	var actual []drift.Actual
	for _, env := range evalEnvs {
		st := sc[env]
		desired = append(desired, drift.Desired{
			Service: evalService, Env: env, Version: st.Git,
			Path: "gitops/overlays/" + env + "/kustomization.yaml",
		})
		sync, health := st.Sync, st.Health
		if sync == "" {
			sync = "Synced"
		}
		if health == "" {
			health = "Healthy"
		}
		actual = append(actual, drift.Actual{
			Service: evalService, Env: env, Version: st.Running,
			Image: "ghcr.io/ofirc/product-catalog", App: env + "-" + evalService, Namespace: env,
			SyncStatus: sync, Health: health, LastMessage: st.Message,
		})
	}
	return drift.Build(desired, actual, evalEnvs)
}

var newTagLine = regexp.MustCompile(`(?m)^(\s*newTag:\s*).*$`)

// overlay serves the real overlay file with newTag set to the scenario's git version, so
// the file the model edits agrees with the facts it was given.
func (sc scenario) overlay(t *testing.T, env string) *Overlay {
	t.Helper()
	b, err := os.ReadFile("../../gitops/overlays/" + env + "/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	content := newTagLine.ReplaceAllString(string(b), "${1}"+sc[env].Git)
	return &Overlay{Path: "gitops/overlays/" + env + "/kustomization.yaml", Content: content, SHA: "sha-" + env}
}

// ollamaReachable reports whether the Ollama OpenAI-compatible API answers GET <base>/models
// within two seconds, so a missing server skips the checks instead of failing every one.
func ollamaReachable(baseURL string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 400
}

// evalModel picks the model under test. MODEL_PROVIDER=ollama runs it on a local Ollama;
// anything else runs on Vertex. It returns the model and the Config fields the model needs.
func evalModel(t *testing.T) (llm model.LLM, answerTool bool, endpoint string) {
	t.Helper()
	ctx := context.Background()
	if os.Getenv("MODEL_PROVIDER") == "ollama" {
		baseURL, modelName := os.Getenv("OLLAMA_BASE_URL"), os.Getenv("OLLAMA_MODEL")
		if baseURL == "" {
			baseURL = "http://localhost:11434/v1"
		}
		if modelName == "" {
			modelName = "sre-qwen3:14b"
		}
		if !ollamaReachable(baseURL) {
			t.Skipf("Ollama is not reachable at %s", baseURL)
		}
		llm, err := NewOllamaModel(ctx, baseURL, modelName)
		if err != nil {
			t.Fatalf("NewOllamaModel: %v", err)
		}
		return llm, true, "ollama at " + baseURL
	}
	project := os.Getenv("VERTEX_PROJECT")
	if project == "" {
		t.Skip("VERTEX_PROJECT is not set; these checks call a real model")
	}
	location, modelName := os.Getenv("VERTEX_LOCATION"), os.Getenv("VERTEX_MODEL")
	if location == "" {
		location = "us-east1"
	}
	if modelName == "" {
		modelName = "gemini-2.5-pro"
	}
	llm, err := NewVertexModel(ctx, project, location, modelName)
	if err != nil {
		t.Fatalf("NewVertexModel: %v", err)
	}
	return llm, false, ""
}

// evalAgent builds the real agent on the real model with fake providers for sc.
func evalAgent(t *testing.T, sc scenario) *Agent {
	t.Helper()
	llm, answerTool, endpoint := evalModel(t)
	ctx := context.Background()
	a, err := New(ctx, Config{
		Model:         llm,
		AnswerTool:    answerTool,
		ModelEndpoint: endpoint,
		Sandbox:       &fakeSandbox{available: false},
		Skills:        skills.FS,
		Providers: Providers{
			Facts: func(context.Context) (drift.Table, error) { return sc.table(), nil },
			GetOverlay: func(_ context.Context, env string) (*Overlay, error) {
				if _, ok := sc[env]; !ok {
					return nil, nil
				}
				return sc.overlay(t, env), nil
			},
			// Health and Tickets stay nil: the tools report "not configured", so the model
			// has to work from the facts alone.
		},
		Envs:         evalEnvs,
		AllowFixEnvs: []string{"stage", "prod"},
		WorkDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func evalContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// evalFix asks the model to fix prod and returns what it did.
func evalFix(t *testing.T, sc scenario) *FixResult {
	t.Helper()
	a := evalAgent(t, sc)
	res, err := a.Fix(evalContext(t), FixRequest{
		Service: evalService, Env: "prod",
		Rows: sc.table().DisagreeingFor(evalService), Progress: &fakeProgress{},
	})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	return res
}

// wantProposal fails unless the model proposed a change, and returns the new file.
func wantProposal(t *testing.T, res *FixResult) string {
	t.Helper()
	if res.Proposal == nil {
		t.Fatalf("model declined (%q), want a proposal", res.Declined)
	}
	return res.Proposal.NewContent
}

func TestEvalPromotionGapFixesForward(t *testing.T) {
	// Stage runs v1.2.0 and prod is still on v1.1.0 everywhere: the fix promotes forward.
	sc := scenario{
		"test":  {Git: "v1.2.0", Running: "v1.2.0"},
		"stage": {Git: "v1.2.0", Running: "v1.2.0"},
		"prod":  {Git: "v1.1.0", Running: "v1.1.0"},
	}
	got := wantProposal(t, evalFix(t, sc))
	if !strings.Contains(got, "newTag: v1.2.0") {
		t.Fatalf("new overlay does not set newTag: v1.2.0:\n%s", got)
	}
}

func TestEvalSyncFailedPinsBack(t *testing.T) {
	// Prod's git tag cannot be pulled, so the fix pins prod back to what is running.
	sc := scenario{
		"test":  {Git: "v1.2.0", Running: "v1.2.0"},
		"stage": {Git: "v1.2.0", Running: "v1.2.0"},
		"prod": {Git: "v1.3.0", Running: "v1.2.0", Sync: "OutOfSync",
			Message: "ImagePullBackOff"},
	}
	got := wantProposal(t, evalFix(t, sc))
	if !strings.Contains(got, "newTag: v1.2.0") {
		t.Fatalf("new overlay does not pin back to newTag: v1.2.0:\n%s", got)
	}
}

func TestEvalUnhealthyKeepsTag(t *testing.T) {
	// The versions agree, so no tag change can fix a degraded pod: the model must decline
	// or leave the tag alone.
	sc := scenario{
		"test":  {Git: "v1.1.0", Running: "v1.1.0"},
		"stage": {Git: "v1.1.0", Running: "v1.1.0"},
		"prod":  {Git: "v1.1.0", Running: "v1.1.0", Health: "Degraded"},
	}
	res := evalFix(t, sc)
	if res.Declined != "" {
		return
	}
	if got := wantProposal(t, res); !strings.Contains(got, "newTag: v1.1.0") {
		t.Fatalf("a change to an unhealthy service with agreeing versions dropped newTag: v1.1.0:\n%s", got)
	}
}

func TestEvalDebugCallsItUnpromoted(t *testing.T) {
	// The standard demo state: prod was never promoted. It is not a failure.
	sc := scenario{
		"test":  {Git: "v1.2.0", Running: "v1.2.0"},
		"stage": {Git: "v1.2.0", Running: "v1.2.0"},
		"prod":  {Git: "v1.1.0", Running: "v1.1.0"},
	}
	a := evalAgent(t, sc)
	res, err := a.Debug(evalContext(t), DebugRequest{
		Service: evalService, Rows: sc.table().DisagreeingFor(evalService), Progress: &fakeProgress{},
	})
	if err != nil {
		t.Fatalf("Debug: %v", err)
	}
	if !saysUnpromoted(res.RootCause) {
		t.Errorf("root cause does not say prod is unpromoted:\n%s", res.RootCause)
	}
	if claimsFailure(res.RootCause) {
		t.Errorf("root cause calls an unpromoted service a failure:\n%s", res.RootCause)
	}
}
