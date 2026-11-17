package agentloop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/grafana"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/jira"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/sandbox"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/slackio"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/skills"
)

// The Reporter must satisfy Progress. The assertion lives here, not in production code,
// so agentloop does not import slackio.
var _ Progress = (*slackio.Reporter)(nil)

// fakeProgress and fakeSandbox lock because ADK runs the tool calls of one model turn
// concurrently. Tests read the fields directly once the run has returned.
type fakeProgress struct {
	mu         sync.Mutex
	steps      []string
	threads    []string
	threadErrs []error     // ctx.Err() at each Thread call
	snippets   [][3]string // filename, title, content
}

func (f *fakeProgress) Step(_ context.Context, format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, strings.TrimSpace(fmt.Sprintf(format, args...)))
}
func (f *fakeProgress) Thread(ctx context.Context, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.threads = append(f.threads, text)
	f.threadErrs = append(f.threadErrs, ctx.Err())
}
func (f *fakeProgress) Snippet(_ context.Context, filename, title, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snippets = append(f.snippets, [3]string{filename, title, content})
}

type fakeSandbox struct {
	mu        sync.Mutex
	available bool
	calls     int
	result    sandbox.Result
	lastDir   string
}

func (f *fakeSandbox) Available(context.Context) bool { return f.available }
func (f *fakeSandbox) Name() string                   { return "fake" }
func (f *fakeSandbox) Run(_ context.Context, _, _, dir string) (sandbox.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastDir = dir
	return f.result, nil
}

func testTable() drift.Table {
	return drift.Table{
		Envs: []string{"stage", "prod"},
		Rows: []drift.Row{
			{Env: "prod", Service: "product-catalog", App: "prod-product-catalog", InGit: true, InCluster: true,
				GitVersion: "v1.1.0", RunningVersion: "v1.0.0", SyncStatus: "Synced", Health: "Healthy",
				Flags: drift.Flags{VersionMismatch: true}},
			{Env: "stage", Service: "product-catalog", App: "stage-product-catalog", InGit: true, InCluster: true,
				GitVersion: "v1.1.0", RunningVersion: "v1.1.0", SyncStatus: "Synced", Health: "Healthy"},
		},
	}
}

func newTestRun(t *testing.T, p Providers, sb *fakeSandbox) (*Agent, *runState, *fakeProgress) {
	t.Helper()
	if p.Facts == nil {
		p.Facts = func(context.Context) (drift.Table, error) { return testTable(), nil }
	}
	if sb == nil {
		sb = &fakeSandbox{available: true}
	}
	a, err := New(context.Background(), Config{Sandbox: sb, Skills: skills.FS, Providers: p, Envs: []string{"stage", "prod"}, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	fp := &fakeProgress{}
	s, err := a.newRun(fp)
	if err != nil {
		t.Fatal(err)
	}
	return a, s, fp
}

func TestGetOverlayRecordsSHAAndWritesFile(t *testing.T) {
	_, s, _ := newTestRun(t, Providers{
		GetOverlay: func(context.Context, string) (*Overlay, error) {
			return &Overlay{Path: "overlays/prod/kustomization.yaml", Content: "x", SHA: "abc"}, nil
		},
	}, nil)
	res := s.getOverlay(context.Background(), envArgs{Env: "prod"})
	if res.Error != "" {
		t.Fatalf("unexpected error %q", res.Error)
	}
	if s.overlays["prod"] == nil || s.overlays["prod"].SHA != "abc" {
		t.Fatalf("overlay not recorded: %+v", s.overlays["prod"])
	}
	got, err := os.ReadFile(filepath.Join(s.dir, "overlay-prod.yaml"))
	if err != nil || string(got) != "x" {
		t.Fatalf("overlay file = %q, %v", got, err)
	}
}

func TestGetOverlayMissingEnvIsToolError(t *testing.T) {
	_, s, _ := newTestRun(t, Providers{
		GetOverlay: func(context.Context, string) (*Overlay, error) { return nil, nil },
	}, nil)
	res := s.getOverlay(context.Background(), envArgs{Env: "prod"})
	if res.Error != "no overlay for prod" {
		t.Fatalf("error = %q", res.Error)
	}
	if len(s.overlays) != 0 {
		t.Fatalf("nothing should be recorded, got %v", s.overlays)
	}
}

func TestUnknownServiceIsToolError(t *testing.T) {
	_, s, _ := newTestRun(t, Providers{}, nil)
	res := s.serviceFacts(context.Background(), serviceArgs{Service: "nope"})
	if res.Error != "unknown service nope" {
		t.Fatalf("error = %q", res.Error)
	}
	ref := s.refreshApp(context.Background(), refreshArgs{Env: "prod", Service: "nope"})
	if ref.Error != "unknown service nope" {
		t.Fatalf("refresh error = %q", ref.Error)
	}
	if env := s.getOverlay(context.Background(), envArgs{Env: "qa"}); env.Error != "unknown environment qa" {
		t.Fatalf("overlay error = %q", env.Error)
	}
}

func TestHealthNotConfigured(t *testing.T) {
	_, s, _ := newTestRun(t, Providers{}, nil)
	res := s.serviceHealth(context.Background(), healthArgs{Env: "prod", Service: "product-catalog", Hours: 6})
	if res.Status != "not configured" {
		t.Fatalf("status = %q", res.Status)
	}
	tr := s.searchTickets(context.Background(), ticketArgs{Service: "product-catalog", Days: 30})
	if tr.Status != "not configured" {
		t.Fatalf("tickets status = %q", tr.Status)
	}
}

func TestHoursAndDaysAreClamped(t *testing.T) {
	var window time.Duration
	var days, limit int
	_, s, _ := newTestRun(t, Providers{
		Health: func(_ context.Context, _, _ string, w time.Duration) (grafana.Health, error) {
			window = w
			return grafana.Health{Window: w}, nil
		},
		Tickets: func(_ context.Context, _ string, d, l int) ([]jira.Issue, error) {
			days, limit = d, l
			return nil, nil
		},
	}, nil)
	s.serviceHealth(context.Background(), healthArgs{Env: "prod", Service: "product-catalog", Hours: 500})
	if window != 72*time.Hour {
		t.Fatalf("window = %s, want 72h", window)
	}
	s.serviceHealth(context.Background(), healthArgs{Env: "prod", Service: "product-catalog", Hours: 0})
	if window != time.Hour {
		t.Fatalf("window = %s, want 1h", window)
	}
	s.searchTickets(context.Background(), ticketArgs{Service: "product-catalog", Days: 0})
	if days != 1 || limit != 10 {
		t.Fatalf("days=%d limit=%d, want 1 and 10", days, limit)
	}
	s.searchTickets(context.Background(), ticketArgs{Service: "product-catalog", Days: 1000})
	if days != 90 {
		t.Fatalf("days = %d, want 90", days)
	}
}

func TestEvidenceLinesAreBounded(t *testing.T) {
	long := strings.Repeat("é", 5000)
	_, s, _ := newTestRun(t, Providers{
		Facts: func(context.Context) (drift.Table, error) {
			return drift.Table{Rows: []drift.Row{{
				Env: "prod", Service: "svc", App: "prod-svc", InGit: true, InCluster: true,
				SyncStatus: "OutOfSync", Health: "Healthy", LastMessage: long,
				Flags: drift.Flags{NotSynced: true},
			}}}, nil
		},
	}, nil)
	s.listDisagreements(context.Background())
	if len(s.evidence) == 0 {
		t.Fatal("no evidence recorded")
	}
	for _, e := range s.evidence {
		if n := len([]rune(e)); n > 300 {
			t.Fatalf("evidence line has %d runes", n)
		}
	}
	if !strings.HasPrefix(s.evidence[0], "• Argo CD prod/svc: sync=OutOfSync health=Healthy") {
		t.Fatalf("evidence = %q", s.evidence[0])
	}
}

func TestRunCodeLimit(t *testing.T) {
	sb := &fakeSandbox{available: true, result: sandbox.Result{Stdout: "ok"}}
	_, s, fp := newTestRun(t, Providers{}, sb)
	for i := 1; i <= 5; i++ {
		res := s.runCode(context.Background(), codeArgs{Language: "python", Code: "print(1)"})
		if res.Error != "" {
			t.Fatalf("call %d: %q", i, res.Error)
		}
	}
	res := s.runCode(context.Background(), codeArgs{Language: "python", Code: "print(1)"})
	if res.Error != "code run limit reached, conclude with what you have" {
		t.Fatalf("6th call error = %q", res.Error)
	}
	// The trail is the audit record: a refused run must not read as one that happened.
	if len(fp.steps) != 5 {
		t.Fatalf("progress steps = %d, want 5 (none for the refused call): %q", len(fp.steps), fp.steps)
	}
	if n := len(s.trail); n != 6 || s.trail[5] != "run_code(rejected)" {
		t.Fatalf("trail = %q, want 5 runs then run_code(rejected)", s.trail)
	}
	if sb.calls != 5 {
		t.Fatalf("sandbox calls = %d, want 5", sb.calls)
	}
	if sb.lastDir != s.dir {
		t.Fatalf("sandbox dataDir = %q, want run dir %q", sb.lastDir, s.dir)
	}
	if len(fp.snippets) != 5 || fp.snippets[0][0] != "code-1.txt" || fp.snippets[0][1] != "python run 1" ||
		fp.snippets[0][2] != "print(1)\n--- output ---\nok" {
		t.Fatalf("snippets = %q", fp.snippets)
	}
}

func TestRunCodeNonZeroExitIsAResult(t *testing.T) {
	sb := &fakeSandbox{available: true, result: sandbox.Result{ExitCode: 2, Stderr: "boom"}}
	_, s, _ := newTestRun(t, Providers{}, sb)
	res := s.runCode(context.Background(), codeArgs{Language: "bash", Code: "exit 2"})
	if res.ExitCode != 2 || res.Error != "" || res.Stderr != "boom" {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunCodeNotRegisteredWithoutSandbox(t *testing.T) {
	for _, avail := range []bool{false, true} {
		a, s, _ := newTestRun(t, Providers{}, &fakeSandbox{available: avail})
		tools, err := a.readTools(s)
		if err != nil {
			t.Fatal(err)
		}
		has := false
		for _, tl := range tools {
			if tl.Name() == "run_code" {
				has = true
			}
		}
		if has != avail {
			t.Fatalf("available=%v but run_code registered=%v", avail, has)
		}
		if len(tools) < 6 {
			t.Fatalf("expected the six read tools, got %d", len(tools))
		}
	}
}

// Inputs are validated before they name a file; this is the second line of defence.
func TestFileSafeKeepsNamesInsideTheRunDir(t *testing.T) {
	got := fileSafe("../Prod,a/b c")
	if strings.ContainsAny(got, "/ ,ABCDEFGHIJKLMNOPQRSTUVWXYZ") || got != "..-prod-a-b-c" {
		t.Fatalf("fileSafe = %q", got)
	}
}

func TestCloseRemovesDirAndPostsTrail(t *testing.T) {
	_, s, fp := newTestRun(t, Providers{}, nil)
	ctx := context.Background()
	s.listDisagreements(ctx)
	s.serviceFacts(ctx, serviceArgs{Service: "product-catalog"})
	s.close(ctx)
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Fatalf("dir still exists: %v", err)
	}
	if len(fp.threads) != 1 {
		t.Fatalf("thread posts = %d", len(fp.threads))
	}
	i1 := strings.Index(fp.threads[0], "list_disagreements({})")
	i2 := strings.Index(fp.threads[0], `get_service_facts({"service":"product-catalog"})`)
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Fatalf("trail = %q", fp.threads[0])
	}
}

func TestCloseUsesSnippetForLongTrail(t *testing.T) {
	_, s, fp := newTestRun(t, Providers{}, nil)
	for i := 0; i < 60; i++ {
		s.trailAdd(strings.Repeat("x", 100))
	}
	s.close(context.Background())
	if len(fp.threads) != 0 || len(fp.snippets) != 1 || fp.snippets[0][0] != "trail.txt" {
		t.Fatalf("threads=%d snippets=%d", len(fp.threads), len(fp.snippets))
	}
}

// The model chooses env, service and language after reading untrusted text. A bad value
// must be refused before any provider sees it, any file is written, or Slack hears it.
func TestRejectedInputsNeverReachProvidersFilesOrSlack(t *testing.T) {
	const badEnv = "../x<!channel>"
	const badSvc = `a"}<!channel>`
	var providerCalls int
	hit := func() { providerCalls++ }
	_, s, fp := newTestRun(t, Providers{
		Refresh: func(context.Context, string) error { hit(); return nil },
		GetOverlay: func(context.Context, string) (*Overlay, error) {
			hit()
			return &Overlay{Path: "p", Content: "c", SHA: "s"}, nil
		},
		Health: func(context.Context, string, string, time.Duration) (grafana.Health, error) {
			hit()
			return grafana.Health{}, nil
		},
		Tickets: func(context.Context, string, int, int) ([]jira.Issue, error) { hit(); return nil, nil },
	}, nil)
	ctx := context.Background()

	cases := []struct {
		name    string
		call    func() string // returns the result's Error
		wantErr string
	}{
		{"get_overlay env", func() string { return s.getOverlay(ctx, envArgs{Env: badEnv}).Error }, "unknown environment " + badEnv},
		{"get_service_health env", func() string {
			return s.serviceHealth(ctx, healthArgs{Env: badEnv, Service: "product-catalog", Hours: 1}).Error
		}, "unknown environment " + badEnv},
		{"get_service_health service", func() string {
			return s.serviceHealth(ctx, healthArgs{Env: "prod", Service: badSvc, Hours: 1}).Error
		}, "unknown service " + badSvc},
		{"search_tickets service", func() string { return s.searchTickets(ctx, ticketArgs{Service: badSvc, Days: 1}).Error }, "unknown service " + badSvc},
		{"refresh_app env", func() string { return s.refreshApp(ctx, refreshArgs{Env: badEnv, Service: "product-catalog"}).Error }, "unknown environment " + badEnv},
		{"refresh_app service", func() string { return s.refreshApp(ctx, refreshArgs{Env: "prod", Service: badSvc}).Error }, "unknown service " + badSvc},
		{"get_service_facts service", func() string { return s.serviceFacts(ctx, serviceArgs{Service: badSvc}).Error }, "unknown service " + badSvc},
	}
	for _, c := range cases {
		if got := c.call(); got != c.wantErr {
			t.Errorf("%s: error = %q, want %q", c.name, got, c.wantErr)
		}
	}
	if providerCalls != 0 {
		t.Errorf("providers were called %d times", providerCalls)
	}
	if entries, _ := os.ReadDir(s.dir); len(entries) != 0 {
		t.Errorf("evidence files written: %v", entries)
	}
	if len(s.evidence) != 0 {
		t.Errorf("evidence recorded: %v", s.evidence)
	}
	if len(fp.steps) != 0 {
		t.Errorf("progress steps for rejected calls: %q", fp.steps)
	}
	for _, line := range s.trail {
		if strings.Contains(line, "channel") || strings.Contains(line, "../x") {
			t.Errorf("trail echoes a rejected value: %q", line)
		}
	}
}

func TestServiceIsCanonicalizedAndEnvIsExact(t *testing.T) {
	var gotService string
	_, s, fp := newTestRun(t, Providers{
		Health: func(_ context.Context, service, _ string, _ time.Duration) (grafana.Health, error) {
			gotService = service
			return grafana.Health{}, nil
		},
	}, nil)
	res := s.serviceHealth(context.Background(), healthArgs{Env: "prod", Service: "PRODUCT-Catalog", Hours: 1})
	if res.Error != "" || gotService != "product-catalog" {
		t.Fatalf("error=%q service=%q", res.Error, gotService)
	}
	if len(fp.steps) != 1 || !strings.Contains(fp.steps[0], "prod/product-catalog") {
		t.Fatalf("steps = %q", fp.steps)
	}
	if got := s.getOverlay(context.Background(), envArgs{Env: "Prod"}).Error; got != "unknown environment Prod" {
		t.Fatalf("env match must be exact, got %q", got)
	}
}

// Every service-taking tool resolves the service against the facts, and fetching them is
// a GitHub and Argo CD round trip. Only refresh_app makes the cached table stale.
func TestFactsAreCachedUntilRefresh(t *testing.T) {
	var facts int
	_, s, _ := newTestRun(t, Providers{
		Facts:   func(context.Context) (drift.Table, error) { facts++; return testTable(), nil },
		Refresh: func(context.Context, string) error { return nil },
	}, nil)
	ctx := context.Background()
	s.serviceFacts(ctx, serviceArgs{Service: "product-catalog"})
	s.serviceHealth(ctx, healthArgs{Env: "prod", Service: "product-catalog", Hours: 1})
	if facts != 1 {
		t.Fatalf("facts calls = %d after two service tools, want 1", facts)
	}
	if res := s.refreshApp(ctx, refreshArgs{Env: "prod", Service: "product-catalog"}); res.Error != "" {
		t.Fatal(res.Error)
	}
	s.serviceFacts(ctx, serviceArgs{Service: "product-catalog"})
	s.listDisagreements(ctx)
	if facts != 2 {
		t.Fatalf("facts calls = %d after a refresh, want 2", facts)
	}
}

func TestFactsErrorIsNotCached(t *testing.T) {
	var facts int
	_, s, _ := newTestRun(t, Providers{
		Facts: func(context.Context) (drift.Table, error) {
			facts++
			if facts == 1 {
				return drift.Table{}, fmt.Errorf("argo down")
			}
			return testTable(), nil
		},
	}, nil)
	ctx := context.Background()
	if res := s.serviceFacts(ctx, serviceArgs{Service: "product-catalog"}); res.Error != "argo down" {
		t.Fatalf("error = %q", res.Error)
	}
	if res := s.serviceFacts(ctx, serviceArgs{Service: "product-catalog"}); res.Error != "" {
		t.Fatalf("retry error = %q", res.Error)
	}
}

func TestRunCodeRejectsUnsupportedLanguage(t *testing.T) {
	sb := &fakeSandbox{available: true}
	_, s, fp := newTestRun(t, Providers{}, sb)
	res := s.runCode(context.Background(), codeArgs{Language: "ruby<!channel>", Code: "puts 1"})
	if res.Error != "unsupported language ruby<!channel>" {
		t.Fatalf("error = %q", res.Error)
	}
	if sb.calls != 0 || s.codeRuns != 0 || len(fp.steps) != 0 || len(fp.snippets) != 0 {
		t.Fatalf("calls=%d codeRuns=%d steps=%q snippets=%d", sb.calls, s.codeRuns, fp.steps, len(fp.snippets))
	}
	if ok := s.runCode(context.Background(), codeArgs{Language: "bash", Code: "true"}); ok.Error != "" {
		t.Fatalf("bash should be accepted: %q", ok.Error)
	}
	if s.codeRuns != 1 {
		t.Fatalf("codeRuns = %d", s.codeRuns)
	}
}

// doctor labels its sandbox row with the backend, and an agent built without a runner
// has none to name.
func TestSandboxName(t *testing.T) {
	a, _ := newTestAgent(t, nil, Providers{})
	if got := a.SandboxName(); got != "fake" {
		t.Errorf("SandboxName = %q, want fake", got)
	}
	bare, err := New(context.Background(), Config{Skills: skills.FS, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got := bare.SandboxName(); got != "" {
		t.Errorf("SandboxName without a runner = %q, want empty", got)
	}
}
