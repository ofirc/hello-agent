package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/genai"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
)

// debugMaxLLMCalls is the debug agent's model-call budget. Skill loads count toward it.
const debugMaxLLMCalls = 20

type SuggestedFix struct{ Env, Summary, Why string }

type DebugRequest struct {
	Service string      // "" lets the model choose
	Rows    []drift.Row // disagreeing rows given to the model up front
	// Refreshed says the caller asked Argo CD to refresh these rows' apps just before
	// the run. The first message says so, so the skill does not spend model calls and a
	// second facts gather repeating it.
	Refreshed bool
	Progress  Progress
}

type DebugResult struct {
	Service   string
	RootCause string
	Fix       *SuggestedFix
	Evidence  []string
}

// debugSchema is the shape of the final answer, so Go can render the suggested fix
// without parsing prose. What goes in each field is the drift-triage skill's business.
var debugSchema = &genai.Schema{
	Type: genai.TypeObject,
	Properties: map[string]*genai.Schema{
		"service":    {Type: genai.TypeString, Description: "The service you triaged."},
		"root_cause": {Type: genai.TypeString, Description: "The root cause, in the drift-triage skill's format."},
		"suggested_fix": {
			Type:        genai.TypeObject,
			Description: "Only when editing an overlay would help.",
			Properties: map[string]*genai.Schema{
				"env":     {Type: genai.TypeString, Description: "The environment whose overlay to edit."},
				"summary": {Type: genai.TypeString, Description: "The edit and its direction, in one line."},
				"why":     {Type: genai.TypeString, Description: "Why that direction, and what to check first."},
			},
			Required: []string{"env", "summary", "why"},
		},
	},
	Required: []string{"service", "root_cause"},
}

const debugJob = "You debug a disagreement between git (the GitOps overlays) and the cluster (Argo CD) for an engineer in Slack. " +
	"You only read: none of your tools changes git or the cluster.\n" +
	"Before anything else, load the drift-triage skill with load_skill and follow its procedure. " +
	"It points to the gitops-fix skill for which way a fix goes; that skill is available too.\n"

const debugFinish = "Finish with the structured answer: the service you triaged, the root cause, and suggested_fix only " +
	"when the drift-triage skill says an overlay edit would help.\n"

func debugInstruction(sandbox bool) string {
	return debugJob + skillResourcesInstruction + safetyInstruction + codeInstruction(sandbox) + debugFinish
}

// promptRow adds the row's note, which the skill's kinds table matches on and which is a
// method, so plain JSON of a Row would leave it out.
type promptRow struct {
	drift.Row
	Note string
}

// factsMessage is the first user message: what the model needs to start without
// re-fetching. Task text comes first so the rows read as data under it.
func (a *Agent) factsMessage(task string, rows []drift.Row) (string, error) {
	pr := make([]promptRow, 0, len(rows))
	for _, r := range rows {
		pr = append(pr, promptRow{Row: r, Note: r.Note()})
	}
	raw, err := json.Marshal(pr)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Environments, least to most production-like: %s.\n%s\nThe disagreeing rows, as JSON:\n%s",
		strings.Join(a.cfg.Envs, ", "), task, raw), nil
}

func (a *Agent) Debug(ctx context.Context, req DebugRequest) (*DebugResult, error) {
	s, err := a.newRun(req.Progress)
	if err != nil {
		return nil, err
	}
	defer s.closeDetached(ctx)

	task := "No service was named: choose the most important disagreement, and say in your answer which service you chose."
	if req.Service != "" {
		task = "Debug the service " + req.Service + "."
	}
	if req.Refreshed {
		task += " Argo CD was asked to refresh these apps just before this run; the rows below were read before that refresh."
	}
	msg, err := a.factsMessage(task, req.Rows)
	if err != nil {
		return nil, err
	}
	tools, err := a.readTools(s)
	if err != nil {
		return nil, err
	}
	spec := loopSpec{
		name:        "drift_debug",
		instruction: debugInstruction(a.SandboxAvailable()),
		tools:       tools,
		schema:      debugSchema,
		maxLLMCalls: debugMaxLLMCalls,
	}
	if a.cfg.AnswerTool {
		submit, err := s.triageTool(req.Rows)
		if err != nil {
			return nil, err
		}
		spec.instruction += triageFinish
		spec.tools = append(spec.tools, submit)
		spec.schema = nil
	}
	final, err := a.runLoop(ctx, s, spec, msg)
	if err != nil {
		return nil, err
	}
	if a.cfg.AnswerTool {
		return s.triageResult()
	}
	return s.debugResult(ctx, final, req.Rows)
}

// debugResult reads the schema answer and checks it as submit_triage would.
func (s *runState) debugResult(ctx context.Context, final string, rows []drift.Row) (*DebugResult, error) {
	if strings.TrimSpace(final) == "" {
		return nil, fmt.Errorf("%w: the model finished without one", errBadAnswer)
	}
	var ans triageArgs
	if err := json.Unmarshal([]byte(final), &ans); err != nil {
		return nil, fmt.Errorf("%w: %v", errBadAnswer, err)
	}
	res, msg := s.checkTriage(ctx, rows, ans)
	if msg != "" {
		return nil, fmt.Errorf("%w: %s", errBadAnswer, msg)
	}
	s.mu.Lock()
	res.Evidence = slices.Clone(s.evidence)
	s.mu.Unlock()
	return res, nil
}
