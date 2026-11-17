// Package agentloop runs the model-driven drift debug and fix agents on ADK for Go.
//
// This file holds the shared configuration. The tools the model calls live in tools.go and
// the per-run bookkeeping (scratch directory, evidence, trail) in run.go. Every tool is a
// thin wrapper over a runState method, so the logic is testable without ADK.
package agentloop

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/grafana"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/jira"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/sandbox"
)

// Progress is the slice of the Slack reporter the agents need. It is declared here so
// agentloop does not import slackio; *slackio.Reporter satisfies it.
type Progress interface {
	Step(ctx context.Context, format string, args ...any)
	Thread(ctx context.Context, text string)
	Snippet(ctx context.Context, filename, title, content string)
}

// Overlay is a kustomization file as read from the GitOps repo. SHA is the blob SHA the
// fix agent must send back when updating the file, so a concurrent edit is rejected
// rather than overwritten.
type Overlay struct{ Path, Content, SHA string }

// Providers are the data sources behind the tools. They are functions, not clients, so
// the agents never see credentials and tests can fake them.
type Providers struct {
	Facts      func(ctx context.Context) (drift.Table, error)
	Refresh    func(ctx context.Context, app string) error
	GetOverlay func(ctx context.Context, env string) (*Overlay, error) // nil, nil when the env has no overlay
	// Health and Tickets are nil when Grafana or JIRA is not configured; the tools then
	// report "not configured" instead of failing.
	Health  func(ctx context.Context, service, env string, window time.Duration) (grafana.Health, error)
	Tickets func(ctx context.Context, service string, days, limit int) ([]jira.Issue, error)
}

type Config struct {
	Model        model.LLM
	Sandbox      sandbox.Runner
	Skills       fs.FS // skills.FS; required
	Providers    Providers
	Envs         []string
	AllowFixEnvs []string
	WorkDir      string // default filepath.Join(os.TempDir(), "sre-runs")
	// AnswerTool makes the debug agent finish by calling submit_triage instead of
	// answering to an output schema. Non-Gemini models need it: ADK sends them the
	// schema in strict mode, which makes every field required, suggested_fix included.
	AnswerTool bool
	// ModelEndpoint, for example "ollama at http://localhost:11434/v1", prefixes model
	// errors so an unreachable model is named. "" leaves them as they are.
	ModelEndpoint string
}

type Agent struct {
	cfg Config
	// sandboxOK is probed once in New. run_code is only offered to the model when the
	// sandbox exists, otherwise the model would burn its code budget on failures.
	sandboxOK bool
	// skills is built once: it holds both skills, because drift-triage reads
	// gitops-fix's references for fix direction and the fix agent may read drift-triage.
	skills *skilltoolset.SkillToolset
	// maxLLMCalls, when non-zero, replaces every agent's model-call budget. Tests only.
	maxLLMCalls int
}

// requiredSkills are the skills the instructions name. Checking them in New turns a
// broken embed into a startup error instead of a model wandering without its procedure.
var requiredSkills = []string{"drift-triage", "gitops-fix"}

func New(ctx context.Context, cfg Config) (*Agent, error) {
	if cfg.Skills == nil {
		return nil, errors.New("agentloop: Skills is required")
	}
	src, _, err := skill.WithCompletePreloadSource(ctx, skill.NewFileSystemSource(cfg.Skills))
	if err != nil {
		return nil, fmt.Errorf("agentloop: load skills: %w", err)
	}
	for _, name := range requiredSkills {
		if _, err := src.LoadFrontmatter(ctx, name); err != nil {
			return nil, fmt.Errorf("agentloop: skill %s: %w", name, err)
		}
	}
	skills, err := skilltoolset.New(ctx, skilltoolset.Config{Source: src})
	if err != nil {
		return nil, fmt.Errorf("agentloop: skills toolset: %w", err)
	}

	if cfg.WorkDir == "" {
		cfg.WorkDir = filepath.Join(os.TempDir(), "sre-runs")
	}
	// The sandbox mounts the run directory with a comma-separated spec, so a comma in
	// the path would split it. Run directory names are generated without commas; the
	// parent is the only part we do not control.
	abs, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return nil, err
	}
	if strings.Contains(abs, ",") {
		return nil, errors.New("agentloop: WorkDir must not contain a comma")
	}
	cfg.WorkDir = abs

	a := &Agent{cfg: cfg, skills: skills}
	if cfg.Sandbox != nil {
		a.sandboxOK = cfg.Sandbox.Available(ctx)
	}
	return a, nil
}

// SandboxAvailable reports whether run_code is offered to the model.
func (a *Agent) SandboxAvailable() bool { return a.sandboxOK }

// SandboxName is the runner's backend name, "" when the agent has no runner.
func (a *Agent) SandboxName() string {
	if a.cfg.Sandbox == nil {
		return ""
	}
	return a.cfg.Sandbox.Name()
}
