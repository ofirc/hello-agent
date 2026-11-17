package agentloop

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
)

// triageArgs is the debug agent's answer, whether it arrives as submit_triage arguments
// or as JSON text against debugSchema. The descriptions match debugSchema's.
type triageArgs struct {
	Service      string        `json:"service" jsonschema:"the service you triaged"`
	RootCause    string        `json:"root_cause" jsonschema:"the root cause, in the drift-triage skill's format"`
	SuggestedFix *triageFixArg `json:"suggested_fix,omitempty" jsonschema:"only when editing an overlay would help"`
}

type triageFixArg struct {
	Env     string `json:"env" jsonschema:"the environment whose overlay to edit"`
	Summary string `json:"summary" jsonschema:"the edit and its direction, in one line"`
	Why     string `json:"why" jsonschema:"why that direction, and what to check first"`
}

const triageFinish = "Give that answer by calling submit_triage exactly once; it ends the run. " +
	"If submit_triage returns an error, correct the answer and call it again.\n"

// checkTriage checks an answer against the facts. The service and fix env reach Slack
// and the fix command, so a value the facts do not know is refused like a bad tool
// argument, and the service gets the facts' spelling. The returned message does not
// quote the model's value: it ends up in Slack. Evidence is left for the caller.
func (s *runState) checkTriage(ctx context.Context, rows []drift.Row, ans triageArgs) (*DebugResult, string) {
	if strings.TrimSpace(ans.RootCause) == "" {
		return nil, "no root cause"
	}
	if f := ans.SuggestedFix; f != nil && (strings.TrimSpace(f.Summary) == "" || strings.TrimSpace(f.Why) == "") {
		return nil, "incomplete suggested fix"
	}
	service := ""
	for _, r := range rows {
		if strings.EqualFold(r.Service, ans.Service) {
			service = r.Service
		}
	}
	if service == "" {
		_, canonical, msg := s.resolveService(ctx, ans.Service)
		if msg != "" {
			return nil, "it names a service the facts do not have"
		}
		service = canonical
	}
	res := &DebugResult{Service: service, RootCause: ans.RootCause}
	if f := ans.SuggestedFix; f != nil {
		if msg := s.checkEnv(f.Env); msg != "" {
			return nil, "its suggested fix names an unknown environment"
		}
		res.Fix = &SuggestedFix{Env: f.Env, Summary: f.Summary, Why: f.Why}
	}
	return res, ""
}

// submitTriage checks the answer and records it. Rejections go back to the model; only
// the canonical service and a checked env reach the trail and the step. The root cause
// is shown to the user as the run's answer, so the trail leaves it out.
func (s *runState) submitTriage(ctx context.Context, rows []drift.Row, args triageArgs) finishResult {
	reject := func(msg string) finishResult {
		s.reject("submit_triage")
		return finishResult{Error: msg}
	}
	if s.alreadyFinished() {
		return reject(errFinished)
	}
	res, msg := s.checkTriage(ctx, rows, args)
	if msg != "" {
		return reject(msg)
	}
	if !s.finishFirst(func() { s.triage = res }) {
		return reject(errFinished)
	}
	trail := struct {
		Service string `json:"service"`
		FixEnv  string `json:"fix_env,omitempty"`
	}{Service: res.Service}
	if res.Fix != nil {
		trail.FixEnv = res.Fix.Env
	}
	s.begin(ctx, "submit_triage", trail, "Reporting the triage of "+res.Service)
	return finishResult{Status: "submitted"}
}

// triageTool is how a debug run ends when Config.AnswerTool is set. Like the fix tools,
// a successful call sets SkipSummarization so ADK ends the invocation without calling
// the model again; a rejected call does not, so the model can correct its answer.
func (s *runState) triageTool(rows []drift.Row) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name: "submit_triage",
		Description: "Report your triage: the service, the root cause in the drift-triage skill's format, and suggested_fix " +
			"only when the skill says an overlay edit would help. Ends the run.",
	}, func(ctx agent.Context, args triageArgs) (finishResult, error) {
		r := s.submitTriage(ctx, rows, args)
		if r.Error == "" {
			ctx.Actions().SkipSummarization = true
		}
		return r, nil
	})
}

// triageResult is the recorded triage with the run's evidence.
func (s *runState) triageResult() (*DebugResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.triage == nil {
		return nil, fmt.Errorf("%w: the model finished without calling submit_triage", errBadAnswer)
	}
	res := *s.triage
	res.Evidence = slices.Clone(s.evidence)
	return &res, nil
}
