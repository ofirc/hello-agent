package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func newAnswerToolAgent(t *testing.T, llm model.LLM) *Agent {
	t.Helper()
	a, _ := newTestAgent(t, llm, Providers{})
	a.cfg.AnswerTool = true
	return a
}

func triageCall(args map[string]any) *genai.Content {
	base := map[string]any{"service": "product-catalog", "root_cause": "- not promoted"}
	for k, v := range args {
		base[k] = v
	}
	return call("submit_triage", base)
}

func offeredTools(req *model.LLMRequest) []string {
	var names []string
	if req.Config == nil {
		return nil
	}
	for _, td := range req.Config.Tools {
		for _, fd := range td.FunctionDeclarations {
			names = append(names, fd.Name)
		}
	}
	return names
}

func TestDebugAnswerToolWithFix(t *testing.T) {
	llm := &scriptedLLM{turns: []*genai.Content{
		call("load_skill", map[string]any{"name": "drift-triage"}),
		triageCall(map[string]any{"service": "Product-Catalog",
			"suggested_fix": map[string]any{"env": "prod", "summary": "v1.1.0 -> v1.2.0", "why": "stage healthy"}}),
	}}
	a := newAnswerToolAgent(t, llm)
	fp := &fakeProgress{}
	res, err := a.Debug(context.Background(), debugRequest(fp))
	if err != nil {
		t.Fatal(err)
	}
	if res.Service != "product-catalog" || res.RootCause != "- not promoted" {
		t.Fatalf("result = %+v", res)
	}
	if res.Fix == nil || res.Fix.Env != "prod" || res.Fix.Summary != "v1.1.0 -> v1.2.0" || res.Fix.Why != "stage healthy" {
		t.Fatalf("fix = %+v", res.Fix)
	}
	if llm.calls != 2 {
		t.Fatalf("model calls = %d, want 2: submit_triage must end the run", llm.calls)
	}
	for i, req := range llm.reqs {
		if req.Config != nil && req.Config.ResponseSchema != nil {
			t.Fatalf("request %d sent a response schema", i)
		}
		if names := strings.Join(offeredTools(req), ","); !strings.Contains(names, "submit_triage") ||
			strings.Contains(names, "set_model_response") {
			t.Fatalf("request %d tools = %s", i, names)
		}
	}
	instr := instructionText(llm.reqs[0])
	if !strings.Contains(instr, "submit_triage exactly once") {
		t.Fatalf("instruction does not ask for submit_triage:\n%s", instr)
	}
	if strings.ContainsAny(instr, "{}") {
		t.Fatalf("instruction has braces: %q", instr)
	}
	if !hasStep(fp, "reporting the triage of product-catalog") {
		t.Fatalf("steps = %q", fp.steps)
	}
	if len(fp.threads) != 1 || !strings.Contains(fp.threads[0], `submit_triage({"service":"product-catalog","fix_env":"prod"})`) {
		t.Fatalf("trail = %q", fp.threads)
	}
	if strings.Contains(fp.threads[0], "not promoted") {
		t.Fatalf("trail carries the root cause: %q", fp.threads[0])
	}
}

func TestDebugAnswerToolWithoutFix(t *testing.T) {
	llm := &scriptedLLM{turns: []*genai.Content{triageCall(nil)}}
	res, err := newAnswerToolAgent(t, llm).Debug(context.Background(), debugRequest(&fakeProgress{}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Fix != nil {
		t.Fatalf("fix = %+v, want nil", res.Fix)
	}
	if res.Service != "product-catalog" {
		t.Fatalf("result = %+v", res)
	}
}

func TestDebugAnswerToolNeverCalled(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"service": "product-catalog", "root_cause": "x"})
	llm := &scriptedLLM{turns: []*genai.Content{text(string(raw))}}
	fp := &fakeProgress{}
	_, err := newAnswerToolAgent(t, llm).Debug(context.Background(), debugRequest(fp))
	if !errors.Is(err, errBadAnswer) || !strings.Contains(err.Error(), "the model finished without calling submit_triage") {
		t.Fatalf("err = %v", err)
	}
}

func TestSubmitTriageRejections(t *testing.T) {
	fix := func(env, summary string) map[string]any {
		return map[string]any{"suggested_fix": map[string]any{"env": env, "summary": summary, "why": "w"}}
	}
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"unknown service", map[string]any{"service": "<!channel>"}, "names a service the facts do not have"},
		{"empty root cause", map[string]any{"root_cause": "  "}, "no root cause"},
		{"incomplete fix", fix("prod", ""), "incomplete suggested fix"},
		{"unknown env", fix("<!channel>", "s"), "names an unknown environment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			llm := &scriptedLLM{turns: []*genai.Content{triageCall(tc.args), text("giving up")}}
			fp := &fakeProgress{}
			_, err := newAnswerToolAgent(t, llm).Debug(context.Background(), debugRequest(fp))
			if !errors.Is(err, errBadAnswer) {
				t.Fatalf("err = %v", err)
			}
			resp := toolResponse(llm.reqs[len(llm.reqs)-1], "submit_triage")
			msg, _ := resp["error"].(string)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("submit_triage response = %v, want error containing %q", resp, tc.want)
			}
			if strings.Contains(msg, "channel") {
				t.Fatalf("error quotes the model's value: %q", msg)
			}
			if hasStep(fp, "reporting") {
				t.Fatalf("rejected call posted a step: %q", fp.steps)
			}
			if len(fp.threads) != 1 || !strings.Contains(fp.threads[0], "submit_triage(rejected)") {
				t.Fatalf("trail = %q", fp.threads)
			}
			for _, s := range append(fp.steps, fp.threads...) {
				if strings.Contains(s, "channel") {
					t.Fatalf("model's value echoed to Slack: %q", s)
				}
			}
		})
	}

	t.Run("second call", func(t *testing.T) {
		a := newAnswerToolAgent(t, nil)
		s, err := a.newRun(&fakeProgress{})
		if err != nil {
			t.Fatal(err)
		}
		defer s.close(context.Background())
		rows := testTable().Disagreeing()
		args := triageArgs{Service: "product-catalog", RootCause: "first"}
		if r := s.submitTriage(context.Background(), rows, args); r.Error != "" {
			t.Fatalf("first submit: %q", r.Error)
		}
		args.RootCause = "second"
		if r := s.submitTriage(context.Background(), rows, args); !strings.Contains(r.Error, "already finished") {
			t.Fatalf("second submit = %+v", r)
		}
		if s.triage == nil || s.triage.RootCause != "first" {
			t.Fatalf("triage = %+v", s.triage)
		}
	})
}

// Vertex keeps the native output schema; the answer tool is only for models that need it.
func TestDebugSchemaPathUnchanged(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"service": "product-catalog", "root_cause": "- skew"})
	llm := vertexLLM{&scriptedLLM{turns: []*genai.Content{text(string(raw))}}}
	a, _ := newTestAgent(t, llm, Providers{})
	res, err := a.Debug(context.Background(), debugRequest(&fakeProgress{}))
	if err != nil {
		t.Fatal(err)
	}
	if res.RootCause != "- skew" {
		t.Fatalf("result = %+v", res)
	}
	req := llm.reqs[0]
	if req.Config.ResponseSchema != debugSchema {
		t.Fatalf("schema = %p, want debugSchema", req.Config.ResponseSchema)
	}
	if names := strings.Join(offeredTools(req), ","); strings.Contains(names, "submit_triage") {
		t.Fatalf("submit_triage offered without AnswerTool: %s", names)
	}
	if strings.Contains(instructionText(req), "submit_triage") {
		t.Fatal("instruction names submit_triage without AnswerTool")
	}
}
