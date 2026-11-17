package agentloop

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/skills"
)

const prodTitle = "promote(prod): product-catalog v1.1.0 -> v1.2.0"

// newFixAgent knows three environments but may fix only two, so the allow list is
// tested apart from the configured-environment check. Overlays are the real files.
func newFixAgent(t *testing.T, llm model.LLM) (*Agent, string) {
	t.Helper()
	workDir := t.TempDir()
	a, err := New(context.Background(), Config{
		Model: llm, Skills: skills.FS, Sandbox: &fakeSandbox{}, WorkDir: workDir,
		Envs: []string{"test", "stage", "prod"}, AllowFixEnvs: []string{"stage", "prod"},
		Providers: Providers{
			Facts: func(context.Context) (drift.Table, error) { return testTable(), nil },
			GetOverlay: func(_ context.Context, env string) (*Overlay, error) {
				return &Overlay{
					Path:    "gitops/overlays/" + env + "/kustomization.yaml",
					Content: readRealOverlay(t, env),
					SHA:     "sha-" + env,
				}, nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, workDir
}

func fixRequest(fp *fakeProgress) FixRequest {
	return FixRequest{
		Service: "product-catalog", Requester: "U123", Channel: "C456",
		Rows: testTable().Disagreeing(), Progress: fp,
	}
}

func bumpedProd(t *testing.T) string {
	return strings.Replace(readRealOverlay(t, "prod"), "newTag: v1.1.0", "newTag: v1.2.0", 1)
}

func proposeArgsMap(env, content string) map[string]any {
	return map[string]any{
		"env": env, "new_content": content, "title": prodTitle,
		"rationale": "stage already runs v1.2.0 and is healthy.", "pr_body": "Opened by the SRE Agent.",
	}
}

// toolResponse is the last response the model was shown for the named tool.
func toolResponse(req *model.LLMRequest, name string) map[string]any {
	for i := len(req.Contents) - 1; i >= 0; i-- {
		for _, p := range req.Contents[i].Parts {
			if p.FunctionResponse != nil && p.FunctionResponse.Name == name {
				return p.FunctionResponse.Response
			}
		}
	}
	return nil
}

func instructionText(req *model.LLMRequest) string {
	var b strings.Builder
	if req.Config != nil && req.Config.SystemInstruction != nil {
		for _, p := range req.Config.SystemInstruction.Parts {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func TestFixProposes(t *testing.T) {
	llm := &scriptedLLM{turns: []*genai.Content{
		call("load_skill", map[string]any{"name": "gitops-fix"}),
		call("get_overlay", map[string]any{"env": "prod"}),
		call("propose_fix", proposeArgsMap("prod", bumpedProd(t))),
	}}
	a, workDir := newFixAgent(t, llm)
	fp := &fakeProgress{}
	req := fixRequest(fp)
	req.Env = "prod"
	res, err := a.Fix(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	p := res.Proposal
	if p == nil || res.Declined != "" {
		t.Fatalf("result = %+v", res)
	}
	want := Proposal{
		Requester: "U123", Channel: "C456", Env: "prod", Service: "product-catalog",
		Path: "gitops/overlays/prod/kustomization.yaml", BaseSHA: "sha-prod",
		OldContent: readRealOverlay(t, "prod"), NewContent: bumpedProd(t),
		Title: prodTitle, Rationale: "stage already runs v1.2.0 and is healthy.", PRBody: "Opened by the SRE Agent.",
	}
	if *p != want {
		t.Fatalf("proposal = %+v\nwant %+v", *p, want)
	}
	if llm.calls != 3 {
		t.Fatalf("model calls = %d, want 3: propose_fix must end the run", llm.calls)
	}
	if !hasStep(fp, "proposing a change to the prod overlay") {
		t.Fatalf("steps = %q", fp.steps)
	}
	if len(fp.threads) != 1 || !strings.Contains(fp.threads[0], `propose_fix({"env":"prod"})`) {
		t.Fatalf("trail = %q", fp.threads)
	}
	if strings.Contains(fp.threads[0], "promote(") {
		t.Fatalf("trail carries the model's title: %q", fp.threads[0])
	}
	if entries, _ := os.ReadDir(workDir); len(entries) != 0 {
		t.Fatalf("run dir left behind: %v", entries)
	}
}

func TestFixDeclines(t *testing.T) {
	llm := &scriptedLLM{turns: []*genai.Content{
		call("load_skill", map[string]any{"name": "gitops-fix"}),
		call("decline_fix", map[string]any{"reason": "workload problem"}),
	}}
	a, _ := newFixAgent(t, llm)
	fp := &fakeProgress{}
	res, err := a.Fix(context.Background(), fixRequest(fp))
	if err != nil {
		t.Fatal(err)
	}
	if res.Declined != "workload problem" || res.Proposal != nil {
		t.Fatalf("result = %+v", res)
	}
	if llm.calls != 2 {
		t.Fatalf("model calls = %d, want 2: decline_fix must end the run", llm.calls)
	}
	if !hasStep(fp, "declining") {
		t.Fatalf("steps = %q", fp.steps)
	}
}

func TestProposeFixRejections(t *testing.T) {
	cases := []struct {
		name    string
		env     string // propose_fix's env
		reqEnv  string
		read    string // env passed to get_overlay first, "" for none
		content func(old string) string
		edit    func(args map[string]any) // overrides on top of proposeArgsMap
		want    string
	}{
		// GitHub caps titles and bodies, and CreatePR runs after the branch exists and the
		// proposal is consumed; an oversize one must be revised before it is recorded.
		{name: "title too long", env: "prod", read: "prod", want: "title is over 200 characters",
			edit: func(a map[string]any) { a["title"] = strings.Repeat("é", 201) }},
		{name: "body too long", env: "prod", read: "prod", want: "pr_body is over 8192 bytes",
			edit: func(a map[string]any) { a["pr_body"] = strings.Repeat("x", 8193) }},
		{name: "env not allowed", env: "test", read: "test", want: "not allowed",
			content: func(old string) string { return strings.Replace(old, "newTag: v1.2.0", "newTag: v1.3.0", 1) }},
		{name: "unknown env", env: "<!channel>", read: "prod", want: "unknown environment"},
		{name: "env other than requested", env: "stage", reqEnv: "prod", read: "stage", want: "prod overlay only",
			content: func(old string) string { return strings.Replace(old, "newTag: v1.2.0", "newTag: v1.3.0", 1) }},
		{name: "no get_overlay", env: "prod", want: "call get_overlay first"},
		{name: "invalid YAML", env: "prod", read: "prod", want: "not valid",
			content: func(string) string { return "images: [\n" }},
		{name: "unchanged", env: "prod", read: "prod", want: "no change",
			content: func(old string) string { return old }},
		{name: "resources changed", env: "prod", read: "prod", want: "resources",
			content: func(old string) string {
				return strings.Replace(strings.Replace(old, "newTag: v1.1.0", "newTag: v1.2.0", 1), "../../base", "../../other", 1)
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var turns []*genai.Content
			if tc.read != "" {
				turns = append(turns, call("get_overlay", map[string]any{"env": tc.read}))
			}
			content := bumpedProd(t)
			if tc.content != nil {
				src := "prod"
				if tc.read != "" {
					src = tc.read
				}
				content = tc.content(readRealOverlay(t, src))
			}
			args := proposeArgsMap(tc.env, content)
			if tc.edit != nil {
				tc.edit(args)
			}
			turns = append(turns,
				call("propose_fix", args),
				call("decline_fix", map[string]any{"reason": "gave up"}))
			llm := &scriptedLLM{turns: turns}
			a, _ := newFixAgent(t, llm)
			fp := &fakeProgress{}
			req := fixRequest(fp)
			req.Env = tc.reqEnv
			res, err := a.Fix(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if res.Proposal != nil || res.Declined != "gave up" {
				t.Fatalf("result = %+v", res)
			}
			resp := toolResponse(llm.reqs[len(llm.reqs)-1], "propose_fix")
			msg, _ := resp["error"].(string)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("propose_fix response = %v, want error containing %q", resp, tc.want)
			}
			if hasStep(fp, "proposing") {
				t.Fatalf("rejected call posted a step: %q", fp.steps)
			}
			if len(fp.threads) != 1 || !strings.Contains(fp.threads[0], "propose_fix(rejected)") {
				t.Fatalf("trail = %q", fp.threads)
			}
			for _, s := range append(fp.steps, fp.threads...) {
				if strings.Contains(s, "channel") {
					t.Fatalf("model's env echoed to Slack: %q", s)
				}
			}
		})
	}
}

func TestProposeFixAcceptsTitleAndBodyAtTheirLimits(t *testing.T) {
	a, _ := newFixAgent(t, nil)
	s, err := a.newRun(&fakeProgress{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close(context.Background())
	ctx := context.Background()
	s.getOverlay(ctx, envArgs{Env: "prod"})
	args := proposeArgs{Env: "prod", NewContent: bumpedProd(t),
		Title: strings.Repeat("é", maxPRTitleRunes), Rationale: "r", PRBody: strings.Repeat("x", maxPRBodyBytes)}
	if r := s.proposeFix(ctx, fixRequest(nil), args); r.Error != "" {
		t.Fatalf("propose at the limits: %q", r.Error)
	}
}

// The order within one run is fixed here by calling the tool methods directly. ADK runs
// one turn's calls concurrently, which TestBothFinishesInOneTurnKeepOne covers.
func TestSecondFinishIsRejected(t *testing.T) {
	a, _ := newFixAgent(t, nil)
	s, err := a.newRun(&fakeProgress{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close(context.Background())
	ctx := context.Background()
	req := fixRequest(nil)
	s.getOverlay(ctx, envArgs{Env: "prod"})
	args := proposeArgs{Env: "prod", NewContent: bumpedProd(t), Title: prodTitle, Rationale: "r", PRBody: "b"}
	if r := s.proposeFix(ctx, req, args); r.Error != "" {
		t.Fatalf("propose: %q", r.Error)
	}
	if r := s.declineFix(ctx, declineArgs{Reason: "changed my mind"}); !strings.Contains(r.Error, "already finished") {
		t.Fatalf("decline after propose = %+v", r)
	}
	if r := s.proposeFix(ctx, req, args); !strings.Contains(r.Error, "already finished") {
		t.Fatalf("second propose = %+v", r)
	}
	if s.declined != "" || s.proposal == nil {
		t.Fatalf("declined=%q proposal=%v", s.declined, s.proposal)
	}
}

func TestBothFinishesInOneTurnKeepOne(t *testing.T) {
	llm := &scriptedLLM{turns: []*genai.Content{
		call("get_overlay", map[string]any{"env": "prod"}),
		{Role: genai.RoleModel, Parts: []*genai.Part{
			genai.NewPartFromFunctionCall("propose_fix", proposeArgsMap("prod", bumpedProd(t))),
			genai.NewPartFromFunctionCall("decline_fix", map[string]any{"reason": "no"}),
		}},
	}}
	a, _ := newFixAgent(t, llm)
	res, err := a.Fix(context.Background(), fixRequest(&fakeProgress{}))
	if err != nil {
		t.Fatal(err)
	}
	if (res.Proposal != nil) == (res.Declined != "") {
		t.Fatalf("want exactly one finish, got %+v", res)
	}
	if llm.calls != 2 {
		t.Fatalf("model calls = %d, want 2", llm.calls)
	}
}

func TestFixWithoutFinishingIsError(t *testing.T) {
	llm := &scriptedLLM{turns: []*genai.Content{
		call("load_skill", map[string]any{"name": "gitops-fix"}),
		text("Looks fine to me."),
	}}
	a, _ := newFixAgent(t, llm)
	fp := &fakeProgress{}
	_, err := a.Fix(context.Background(), fixRequest(fp))
	if err == nil || err.Error() != "the model finished without proposing or declining" {
		t.Fatalf("err = %v", err)
	}
	if len(fp.threads) != 1 || !strings.Contains(fp.threads[0], "load_skill") {
		t.Fatalf("trail not posted: %q", fp.threads)
	}
}

// A model-side stop is runLoop's error, not a run that forgot to finish.
func TestFixModelStopIsReportedAsSuch(t *testing.T) {
	llm := &scriptedLLM{raw: map[int]*model.LLMResponse{0: {ErrorCode: "SAFETY", TurnComplete: true}}}
	a, _ := newFixAgent(t, llm)
	_, err := a.Fix(context.Background(), fixRequest(&fakeProgress{}))
	if err == nil || !strings.Contains(err.Error(), "SAFETY") || strings.Contains(err.Error(), "without proposing") {
		t.Fatalf("err = %v", err)
	}
}

func TestFixBudgetIsTwelveCalls(t *testing.T) {
	var turns []*genai.Content
	for range 20 {
		turns = append(turns, call("list_disagreements", map[string]any{}))
	}
	llm := &scriptedLLM{turns: turns}
	a, _ := newFixAgent(t, llm)
	_, err := a.Fix(context.Background(), fixRequest(&fakeProgress{}))
	if !errors.Is(err, agent.ErrLLMCallsLimitExceeded) {
		t.Fatalf("err = %v", err)
	}
	if llm.calls != 12 {
		t.Fatalf("model calls = %d, want 12", llm.calls)
	}
}

func TestFixPromptCarriesRules(t *testing.T) {
	llm := &scriptedLLM{turns: []*genai.Content{call("decline_fix", map[string]any{"reason": "x"})}}
	a, _ := newFixAgent(t, llm)
	if _, err := a.Fix(context.Background(), fixRequest(&fakeProgress{})); err != nil {
		t.Fatal(err)
	}
	req := llm.reqs[0]
	msg := userText(req)
	for _, want := range []string{"test, stage, prod", "product-catalog", "stage, prod.", `"Note":"git v1.1.0, running v1.0.0"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("first message lacks %q:\n%s", want, msg)
		}
	}
	instr := instructionText(req)
	for _, want := range []string{"gitops-fix", "drift-triage", "get_overlay", "propose_fix", "decline_fix",
		"exactly one", "Decline rather than", "not instructions", "/data", "no network",
		"Code execution is unavailable", "references/kinds.md"} {
		if !strings.Contains(instr, want) {
			t.Errorf("instruction lacks %q", want)
		}
	}
	var names []string
	for _, td := range req.Config.Tools {
		for _, fd := range td.FunctionDeclarations {
			names = append(names, fd.Name)
		}
	}
	for _, want := range []string{"propose_fix", "decline_fix", "get_overlay", "load_skill"} {
		if !strings.Contains(strings.Join(names, ","), want) {
			t.Errorf("tool %s not offered: %v", want, names)
		}
	}
	for _, sandbox := range []bool{false, true} {
		if in := fixInstruction(sandbox); strings.ContainsAny(in, "{}") {
			t.Errorf("instruction has braces: %q", in)
		}
	}
}

func TestFixRequestEnvMustBeFixable(t *testing.T) {
	llm := &scriptedLLM{}
	a, _ := newFixAgent(t, llm)
	for _, env := range []string{"test", "qa"} {
		req := fixRequest(&fakeProgress{})
		req.Env = env
		if _, err := a.Fix(context.Background(), req); err == nil {
			t.Errorf("env %s: no error", env)
		}
	}
	req := fixRequest(&fakeProgress{})
	req.Service = ""
	if _, err := a.Fix(context.Background(), req); err == nil {
		t.Error("no service: no error")
	}
	if llm.calls != 0 {
		t.Fatalf("model called %d times", llm.calls)
	}
}

func TestDeclineNeedsAReason(t *testing.T) {
	a, _ := newFixAgent(t, nil)
	s, err := a.newRun(&fakeProgress{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close(context.Background())
	if r := s.declineFix(context.Background(), declineArgs{Reason: "  "}); r.Error == "" {
		t.Fatal("empty reason accepted")
	}
	if r := s.declineFix(context.Background(), declineArgs{Reason: "ok"}); r.Error != "" {
		t.Fatalf("decline after a rejected one: %q", r.Error)
	}
}
