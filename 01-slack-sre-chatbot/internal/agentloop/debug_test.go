package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/grafana"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/skills"
)

func newTestAgent(t *testing.T, llm model.LLM, p Providers) (*Agent, string) {
	t.Helper()
	if p.Facts == nil {
		p.Facts = func(context.Context) (drift.Table, error) { return testTable(), nil }
	}
	workDir := t.TempDir()
	a, err := New(context.Background(), Config{
		Model: llm, Skills: skills.FS, Sandbox: &fakeSandbox{}, Providers: p,
		Envs: []string{"stage", "prod"}, WorkDir: workDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, workDir
}

func debugRequest(fp *fakeProgress) DebugRequest {
	return DebugRequest{Service: "product-catalog", Rows: testTable().Disagreeing(), Progress: fp}
}

func answer(fix map[string]any) *genai.Content {
	args := map[string]any{"service": "product-catalog", "root_cause": "- prod was never promoted"}
	if fix != nil {
		args["suggested_fix"] = fix
	}
	return call("set_model_response", args)
}

func hasStep(fp *fakeProgress, want string) bool {
	for _, s := range fp.steps {
		if strings.Contains(strings.ToLower(s), want) {
			return true
		}
	}
	return false
}

func TestDebugRunsToolsAndParsesAnswer(t *testing.T) {
	llm := &scriptedLLM{turns: []*genai.Content{
		call("load_skill", map[string]any{"name": "drift-triage"}),
		call("get_service_facts", map[string]any{"service": "product-catalog"}),
		answer(map[string]any{"env": "prod", "summary": "v1.1.0 -> v1.2.0", "why": "stage healthy"}),
	}}
	a, _ := newTestAgent(t, llm, Providers{})
	fp := &fakeProgress{}
	res, err := a.Debug(context.Background(), debugRequest(fp))
	if err != nil {
		t.Fatal(err)
	}
	if res.Service != "product-catalog" || res.RootCause != "- prod was never promoted" {
		t.Fatalf("result = %+v", res)
	}
	if res.Fix == nil || res.Fix.Env != "prod" || res.Fix.Summary != "v1.1.0 -> v1.2.0" || res.Fix.Why != "stage healthy" {
		t.Fatalf("fix = %+v", res.Fix)
	}
	if len(res.Evidence) < 1 {
		t.Fatal("no evidence")
	}
	if !hasStep(fp, "loading skill drift-triage") {
		t.Fatalf("steps = %q", fp.steps)
	}
	if llm.calls != 3 {
		t.Fatalf("model calls = %d, want 3", llm.calls)
	}
	if len(fp.threads) != 1 || !strings.Contains(fp.threads[0], `load_skill({"name":"drift-triage"})`) {
		t.Fatalf("trail = %q", fp.threads)
	}
}

// The first message carries the environment order, the service and the rows; the
// instruction carries the safety points. Both are checked on what the model received.
func TestDebugPromptCarriesFactsAndRules(t *testing.T) {
	llm := &scriptedLLM{turns: []*genai.Content{answer(nil)}}
	a, _ := newTestAgent(t, llm, Providers{})
	if _, err := a.Debug(context.Background(), debugRequest(&fakeProgress{})); err != nil {
		t.Fatal(err)
	}
	req := llm.reqs[0]
	msg := userText(req)
	for _, want := range []string{"stage, prod", "product-catalog", `"GitVersion":"v1.1.0"`, `"Note":"git v1.1.0, running v1.0.0"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("first message lacks %q:\n%s", want, msg)
		}
	}
	var sys strings.Builder
	if req.Config != nil && req.Config.SystemInstruction != nil {
		for _, p := range req.Config.SystemInstruction.Parts {
			sys.WriteString(p.Text)
		}
	}
	instr := sys.String()
	for _, want := range []string{"drift-triage", "not instructions", "/data", "no network",
		"Code execution is unavailable", "references/kinds.md"} {
		if !strings.Contains(instr, want) {
			t.Errorf("instruction lacks %q", want)
		}
	}
	if req.Config == nil || req.Config.Temperature == nil || *req.Config.Temperature != 0.2 {
		t.Errorf("temperature not 0.2")
	}
	// ADK reads {name} in an instruction as a session-state placeholder.
	for _, sandbox := range []bool{false, true} {
		if in := debugInstruction(sandbox); strings.ContainsAny(in, "{}") {
			t.Errorf("instruction has braces: %q", in)
		}
	}
	if strings.Contains(debugInstruction(true), "unavailable") {
		t.Error("instruction says code is unavailable when the sandbox works")
	}
}

func TestDebugWithoutServiceLetsModelChoose(t *testing.T) {
	llm := &scriptedLLM{turns: []*genai.Content{answer(nil)}}
	a, _ := newTestAgent(t, llm, Providers{})
	req := debugRequest(&fakeProgress{})
	req.Service = ""
	if _, err := a.Debug(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := userText(llm.reqs[0]); !strings.Contains(got, "choose the most important disagreement") {
		t.Fatalf("first message = %q", got)
	}
}

// The handler refreshes a named service's apps before the run; the drift-triage skill
// only asks for its own refresh when the first message does not say so.
func TestDebugPromptSaysWhenAppsWereJustRefreshed(t *testing.T) {
	const sentence = "Argo CD was asked to refresh these apps just before this run"
	for _, refreshed := range []bool{true, false} {
		llm := &scriptedLLM{turns: []*genai.Content{answer(nil)}}
		a, _ := newTestAgent(t, llm, Providers{})
		req := debugRequest(&fakeProgress{})
		req.Refreshed = refreshed
		if _, err := a.Debug(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(userText(llm.reqs[0]), sentence); got != refreshed {
			t.Errorf("Refreshed=%v: first message has the refresh sentence = %v:\n%s", refreshed, got, userText(llm.reqs[0]))
		}
	}
}

func userText(req *model.LLMRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		if c.Role == genai.RoleUser {
			for _, p := range c.Parts {
				b.WriteString(p.Text)
			}
		}
	}
	return b.String()
}

func TestDebugWithoutFixLeavesFixNil(t *testing.T) {
	llm := &scriptedLLM{turns: []*genai.Content{
		call("load_skill", map[string]any{"name": "drift-triage"}),
		answer(nil),
	}}
	a, _ := newTestAgent(t, llm, Providers{})
	res, err := a.Debug(context.Background(), debugRequest(&fakeProgress{}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Fix != nil {
		t.Fatalf("fix = %+v, want nil", res.Fix)
	}
}

// Production runs on Vertex, where ADK sends the schema natively and the answer is the
// final text, not a set_model_response call.
func TestDebugReadsNativeStructuredAnswer(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"service": "Product-Catalog", "root_cause": "- skew"})
	llm := vertexLLM{&scriptedLLM{turns: []*genai.Content{
		call("load_skill", map[string]any{"name": "drift-triage"}),
		text(string(raw)),
	}}}
	a, _ := newTestAgent(t, llm, Providers{})
	res, err := a.Debug(context.Background(), debugRequest(&fakeProgress{}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Service != "product-catalog" || res.RootCause != "- skew" {
		t.Fatalf("result = %+v", res)
	}
	if cfg := llm.reqs[1].Config; cfg.ResponseSchema != debugSchema || cfg.ResponseMIMEType != "application/json" {
		t.Fatalf("native schema not sent: schema=%p mime=%q", cfg.ResponseSchema, cfg.ResponseMIMEType)
	}
	for _, td := range llm.reqs[1].Config.Tools {
		for _, fd := range td.FunctionDeclarations {
			if fd.Name == "set_model_response" {
				t.Fatal("set_model_response offered on Vertex")
			}
		}
	}
}

func TestDebugBadFinalAnswerIsReadableError(t *testing.T) {
	llm := &scriptedLLM{turns: []*genai.Content{
		call("load_skill", map[string]any{"name": "drift-triage"}),
		text("not json"),
	}}
	a, _ := newTestAgent(t, llm, Providers{})
	fp := &fakeProgress{}
	_, err := a.Debug(context.Background(), debugRequest(fp))
	if err == nil || !strings.Contains(err.Error(), "could not read the model's answer") {
		t.Fatalf("err = %v", err)
	}
	if len(fp.threads) != 1 || !strings.Contains(fp.threads[0], "load_skill") {
		t.Fatalf("trail not posted: %q", fp.threads)
	}
}

// On Vertex the answer is plain text that ADK does not check against the schema, so a
// reply missing a required field must still be refused.
func TestDebugIncompleteNativeAnswerIsReadableError(t *testing.T) {
	for name, ans := range map[string]map[string]any{
		"no root cause": {"service": "product-catalog"},
		"empty fix":     {"service": "product-catalog", "root_cause": "x", "suggested_fix": map[string]any{"env": "prod"}},
	} {
		raw, _ := json.Marshal(ans)
		llm := vertexLLM{&scriptedLLM{turns: []*genai.Content{text(string(raw))}}}
		a, _ := newTestAgent(t, llm, Providers{})
		_, err := a.Debug(context.Background(), debugRequest(&fakeProgress{}))
		if err == nil || !strings.Contains(err.Error(), "could not read the model's answer") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// The service and fix env go to Slack and to the fix command; a value outside the
// facts is not an answer Go can act on.
func TestDebugAnswerOutsideTheFactsIsReadableError(t *testing.T) {
	for name, turn := range map[string]*genai.Content{
		"service": call("set_model_response", map[string]any{"service": "<!channel>", "root_cause": "x"}),
		"env":     answer(map[string]any{"env": "qa", "summary": "s", "why": "w"}),
	} {
		a, _ := newTestAgent(t, &scriptedLLM{turns: []*genai.Content{turn}}, Providers{})
		_, err := a.Debug(context.Background(), debugRequest(&fakeProgress{}))
		if err == nil || !strings.Contains(err.Error(), "could not read the model's answer") {
			t.Errorf("%s: err = %v", name, err)
		}
		if err != nil && (strings.Contains(err.Error(), "channel") || strings.Contains(err.Error(), "qa")) {
			t.Errorf("%s: error quotes the model's value: %v", name, err)
		}
	}
}

// A model-side stop (safety, malformed call, blocked prompt) arrives as a final event
// with an ErrorCode, not as an iterator error. The code must reach the user; the free
// text message must not, it is the provider's wording.
func TestDebugModelStopNamesTheReason(t *testing.T) {
	llm := &scriptedLLM{
		turns: []*genai.Content{call("load_skill", map[string]any{"name": "drift-triage"})},
		raw: map[int]*model.LLMResponse{1: {
			ErrorCode: "MALFORMED_FUNCTION_CALL", ErrorMessage: "<!channel> bad call", TurnComplete: true,
		}},
	}
	a, workDir := newTestAgent(t, llm, Providers{})
	fp := &fakeProgress{}
	_, err := a.Debug(context.Background(), debugRequest(fp))
	if err == nil || !strings.Contains(err.Error(), "MALFORMED_FUNCTION_CALL") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "channel") {
		t.Fatalf("error carries the provider message: %v", err)
	}
	if entries, _ := os.ReadDir(workDir); len(entries) != 0 {
		t.Fatalf("run dir left behind: %v", entries)
	}
	if len(fp.threads) != 1 || !strings.Contains(fp.threads[0], "load_skill") {
		t.Fatalf("trail not posted: %q", fp.threads)
	}
}

func TestDebugBudgetExhausted(t *testing.T) {
	var turns []*genai.Content
	for i := 0; i < 10; i++ {
		turns = append(turns, call("list_disagreements", map[string]any{}))
	}
	llm := &scriptedLLM{turns: turns}
	a, _ := newTestAgent(t, llm, Providers{})
	a.maxLLMCalls = 2
	_, err := a.Debug(context.Background(), debugRequest(&fakeProgress{}))
	if !errors.Is(err, agent.ErrLLMCallsLimitExceeded) {
		t.Fatalf("err = %v", err)
	}
	if llm.calls != 2 {
		t.Fatalf("model calls = %d, want 2", llm.calls)
	}
}

func TestDebugRemovesRunDirOnError(t *testing.T) {
	llm := &scriptedLLM{turns: []*genai.Content{
		call("get_service_facts", map[string]any{"service": "product-catalog"}),
	}} // the second call has no turn and fails
	a, workDir := newTestAgent(t, llm, Providers{})
	if _, err := a.Debug(context.Background(), debugRequest(&fakeProgress{})); err == nil {
		t.Fatal("expected an error")
	}
	entries, err := os.ReadDir(workDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("work dir entries = %v, %v", entries, err)
	}
}

// A run cut short by its deadline still posts the trail: that is when a reader most
// needs to see what the agent did.
func TestDebugPostsTrailWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	llm := &scriptedLLM{turns: []*genai.Content{
		call("get_service_health", map[string]any{"env": "prod", "service": "product-catalog", "hours": 6}),
		answer(nil),
	}}
	a, _ := newTestAgent(t, llm, Providers{
		Health: func(context.Context, string, string, time.Duration) (grafana.Health, error) {
			cancel()
			return grafana.Health{}, nil
		},
	})
	fp := &fakeProgress{}
	if _, err := a.Debug(ctx, debugRequest(fp)); err == nil {
		t.Fatal("expected the cancelled run to fail")
	}
	if len(fp.threads) != 1 || len(fp.threadErrs) != 1 || fp.threadErrs[0] != nil {
		t.Fatalf("threads=%q ctx errs=%v", fp.threads, fp.threadErrs)
	}
}

func TestSkillLoadsWithBadNamesAreNotEchoed(t *testing.T) {
	llm := &scriptedLLM{turns: []*genai.Content{
		call("load_skill", map[string]any{"name": "<!channel>"}),
		call("load_skill_resource", map[string]any{"skill_name": "drift-triage", "resource_path": "references/kinds.md"}),
		answer(nil),
	}}
	a, _ := newTestAgent(t, llm, Providers{})
	fp := &fakeProgress{}
	if _, err := a.Debug(context.Background(), debugRequest(fp)); err != nil {
		t.Fatal(err)
	}
	for _, s := range append(fp.steps, fp.threads...) {
		if strings.Contains(s, "channel") {
			t.Fatalf("bad skill name echoed: %q", s)
		}
	}
	if !hasStep(fp, "drift-triage/references/kinds.md") {
		t.Fatalf("steps = %q", fp.steps)
	}
}
