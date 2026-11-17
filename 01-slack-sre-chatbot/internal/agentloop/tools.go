package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
)

const (
	minHours, maxHours = 1, 72
	minDays, maxDays   = 1, 90
	ticketLimit        = 10
)

// Tool arguments. The jsonschema tags are what the model reads as parameter docs.

type noArgs struct{}

type serviceArgs struct {
	Service string `json:"service" jsonschema:"service name, for example product-catalog"`
}

type envArgs struct {
	Env string `json:"env" jsonschema:"environment name, for example prod"`
}

type refreshArgs struct {
	Env     string `json:"env" jsonschema:"environment name, for example prod"`
	Service string `json:"service" jsonschema:"service name, for example product-catalog"`
}

type healthArgs struct {
	Env     string `json:"env" jsonschema:"environment name, for example prod"`
	Service string `json:"service" jsonschema:"service name, for example product-catalog"`
	Hours   int    `json:"hours" jsonschema:"look-back window in hours, 1 to 72"`
}

type ticketArgs struct {
	Service string `json:"service" jsonschema:"service name, for example product-catalog"`
	Days    int    `json:"days" jsonschema:"look-back window in days, 1 to 90"`
}

type codeArgs struct {
	Language string `json:"language" jsonschema:"python or bash"`
	Code     string `json:"code" jsonschema:"the script to run. Evidence files from earlier tool calls are in /data, read-only. There is no network."`
}

// Tool results. Failures the model can act on go in Error, not in a Go error, so it can
// try something else.

type factsResult struct {
	Rows  []drift.Row `json:"rows,omitempty"`
	Error string      `json:"error,omitempty"`
}

type refreshResult struct {
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

type overlayResult struct {
	Path    string `json:"path,omitempty"`
	SHA     string `json:"sha,omitempty"`
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

type healthResult struct {
	Status       string  `json:"status,omitempty"` // "ok" or "not configured"
	Summary      string  `json:"summary,omitempty"`
	ErrorRate    float64 `json:"error_rate,omitempty"`
	LatencyP99Ms float64 `json:"latency_p99_ms,omitempty"`
	RPS          float64 `json:"rps,omitempty"`
	Partial      bool    `json:"partial,omitempty"`
	Error        string  `json:"error,omitempty"`
}

type ticket struct {
	Key      string `json:"key"`
	Summary  string `json:"summary"`
	Status   string `json:"status"`
	Priority string `json:"priority,omitempty"`
	Created  string `json:"created,omitempty"`
	URL      string `json:"url,omitempty"`
}

type ticketsResult struct {
	Status  string   `json:"status,omitempty"` // "ok" or "not configured"
	Tickets []ticket `json:"tickets,omitempty"`
	Error   string   `json:"error,omitempty"`
}

type codeResult struct {
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Error    string `json:"error,omitempty"`
}

// begin records a tool call in the trail and tells the user what the agent is doing.
func (s *runState) begin(ctx context.Context, name string, args any, step string) {
	raw, err := json.Marshal(args)
	if err != nil {
		raw = []byte("{}")
	}
	s.trailAdd(name + "(" + string(raw) + ")")
	s.progress.Step(ctx, "%s", step)
}

// reject records a refused call in the trail without its arguments: they are what failed
// validation, and the trail is posted to Slack. No progress step is emitted for the same
// reason.
func (s *runState) reject(name string) { s.trailAdd(name + "(rejected)") }

// checkEnv accepts only an environment from Config.Envs, exact match. The name reaches
// PromQL and a git path, so the model's text must never get there unchecked.
func (s *runState) checkEnv(env string) string {
	if slices.Contains(s.a.cfg.Envs, env) {
		return ""
	}
	return "unknown environment " + truncate(env, 80)
}

// resolveService matches a name against the services in the run's facts table, case
// insensitively, and returns the table's spelling. That canonical name, not the model's
// text, is what providers, files and Slack steps get.
func (s *runState) resolveService(ctx context.Context, service string) (drift.Table, string, string) {
	table, err := s.factsTable(ctx)
	if err != nil {
		return table, "", err.Error()
	}
	for _, name := range table.Services() {
		if strings.EqualFold(name, service) {
			return table, name, ""
		}
	}
	return table, "", "unknown service " + truncate(service, 80)
}

func argoLine(r drift.Row) string {
	line := fmt.Sprintf("• Argo CD %s/%s: sync=%s health=%s", r.Env, r.Service, r.SyncStatus, r.Health)
	if !r.InCluster {
		line = fmt.Sprintf("• Argo CD %s/%s: not in the cluster", r.Env, r.Service)
	}
	if r.InGit && r.InCluster && r.GitVersion != r.RunningVersion {
		line += fmt.Sprintf(" git=%s running=%s", r.GitVersion, r.RunningVersion)
	}
	if r.LastMessage != "" && r.SyncStatus != "Synced" {
		line += " (" + r.LastMessage + ")"
	}
	return line
}

func (s *runState) listDisagreements(ctx context.Context) factsResult {
	s.begin(ctx, "list_disagreements", noArgs{}, "Checking Argo CD and git for disagreements")
	table, err := s.factsTable(ctx)
	if err != nil {
		return factsResult{Error: err.Error()}
	}
	rows := table.Disagreeing()
	for _, r := range rows {
		s.evidenceAdd(argoLine(r))
	}
	s.writeJSON("facts.json", rows)
	return factsResult{Rows: rows}
}

func (s *runState) serviceFacts(ctx context.Context, args serviceArgs) factsResult {
	table, service, msg := s.resolveService(ctx, args.Service)
	if msg != "" {
		s.reject("get_service_facts")
		return factsResult{Error: msg}
	}
	args.Service = service
	s.begin(ctx, "get_service_facts", args, fmt.Sprintf("Reading facts for %s", service))
	rows := table.ForService(service)
	for _, r := range rows {
		s.evidenceAdd(argoLine(r))
	}
	s.writeJSON("facts-"+fileSafe(service)+".json", rows)
	return factsResult{Rows: rows}
}

func (s *runState) refreshApp(ctx context.Context, args refreshArgs) refreshResult {
	if msg := s.checkEnv(args.Env); msg != "" {
		s.reject("refresh_app")
		return refreshResult{Error: msg}
	}
	table, service, msg := s.resolveService(ctx, args.Service)
	if msg != "" {
		s.reject("refresh_app")
		return refreshResult{Error: msg}
	}
	args.Service = service
	s.begin(ctx, "refresh_app", args, fmt.Sprintf("Refreshing %s/%s in Argo CD", args.Env, service))
	var app string
	for _, r := range table.ForService(service) {
		if r.Env == args.Env {
			app = r.App
		}
	}
	if app == "" {
		return refreshResult{Error: fmt.Sprintf("%s/%s has no Argo CD app", args.Env, service)}
	}
	if s.a.cfg.Providers.Refresh == nil {
		return refreshResult{Error: "refresh is not available"}
	}
	if err := s.a.cfg.Providers.Refresh(ctx, app); err != nil {
		return refreshResult{Error: err.Error()}
	}
	// The point of a refresh is a fresh look, so the next tool call must not answer from
	// the table read before it.
	s.forgetFacts()
	return refreshResult{Status: "refreshed " + app}
}

func (s *runState) getOverlay(ctx context.Context, args envArgs) overlayResult {
	if msg := s.checkEnv(args.Env); msg != "" {
		s.reject("get_overlay")
		return overlayResult{Error: msg}
	}
	s.begin(ctx, "get_overlay", args, fmt.Sprintf("Reading the %s overlay from git", args.Env))
	if s.a.cfg.Providers.GetOverlay == nil {
		return overlayResult{Error: "overlays are not available"}
	}
	o, err := s.a.cfg.Providers.GetOverlay(ctx, args.Env)
	if err != nil {
		return overlayResult{Error: err.Error()}
	}
	if o == nil {
		return overlayResult{Error: "no overlay for " + args.Env}
	}
	s.mu.Lock()
	s.overlays[args.Env] = o
	s.mu.Unlock()
	s.writeEvidence("overlay-"+fileSafe(args.Env)+".yaml", []byte(o.Content))
	s.evidenceAdd(fmt.Sprintf("• Git overlay %s: %s", args.Env, o.Path))
	return overlayResult{Path: o.Path, SHA: o.SHA, Content: o.Content}
}

func (s *runState) serviceHealth(ctx context.Context, args healthArgs) healthResult {
	hours := min(max(args.Hours, minHours), maxHours)
	if msg := s.checkEnv(args.Env); msg != "" {
		s.reject("get_service_health")
		return healthResult{Error: msg}
	}
	_, service, msg := s.resolveService(ctx, args.Service)
	if msg != "" {
		s.reject("get_service_health")
		return healthResult{Error: msg}
	}
	args.Service = service
	s.begin(ctx, "get_service_health", args, fmt.Sprintf("Checking Grafana for %s/%s", args.Env, service))
	if s.a.cfg.Providers.Health == nil {
		return healthResult{Status: "not configured"}
	}
	h, err := s.a.cfg.Providers.Health(ctx, service, args.Env, time.Duration(hours)*time.Hour)
	if err != nil {
		return healthResult{Error: err.Error()}
	}
	s.writeJSON("grafana-"+fileSafe(args.Env)+"-"+fileSafe(service)+".json", h)
	s.evidenceAdd(fmt.Sprintf("• Grafana (%s, %dh): %s", args.Env, hours, h.String()))
	return healthResult{
		Status: "ok", Summary: h.String(), ErrorRate: h.ErrorRate,
		LatencyP99Ms: h.LatencyP99Ms, RPS: h.RPS, Partial: h.Partial,
	}
}

func (s *runState) searchTickets(ctx context.Context, args ticketArgs) ticketsResult {
	days := min(max(args.Days, minDays), maxDays)
	_, service, msg := s.resolveService(ctx, args.Service)
	if msg != "" {
		s.reject("search_tickets")
		return ticketsResult{Error: msg}
	}
	args.Service = service
	s.begin(ctx, "search_tickets", args, fmt.Sprintf("Searching JIRA for %s", service))
	if s.a.cfg.Providers.Tickets == nil {
		return ticketsResult{Status: "not configured"}
	}
	issues, err := s.a.cfg.Providers.Tickets(ctx, service, days, ticketLimit)
	if err != nil {
		return ticketsResult{Error: err.Error()}
	}
	out := make([]ticket, 0, len(issues))
	parts := make([]string, 0, len(issues))
	for _, i := range issues {
		t := ticket{Key: i.Key, Summary: i.Summary, Status: i.Status, Priority: i.Priority, URL: i.URL}
		if !i.Created.IsZero() {
			t.Created = i.Created.Format(time.DateOnly)
		}
		out = append(out, t)
		parts = append(parts, i.Key+" "+i.Summary)
	}
	s.writeJSON("jira-"+fileSafe(service)+".json", out)
	line := fmt.Sprintf("• JIRA, last %d days (%d)", days, len(issues))
	if len(parts) > 0 {
		line += ": " + strings.Join(parts, "; ")
	}
	s.evidenceAdd(line)
	return ticketsResult{Status: "ok", Tickets: out}
}

func (s *runState) runCode(ctx context.Context, args codeArgs) codeResult {
	// Only the two languages the sandbox supports, checked before the step and before a
	// limit slot is spent. The code itself stays out of the trail: it is already posted
	// as a snippet, and the trail is posted to Slack.
	if args.Language != "python" && args.Language != "bash" {
		s.reject("run_code")
		return codeResult{Error: "unsupported language " + truncate(args.Language, 80)}
	}
	// The limit is checked and the slot claimed in one critical section, before the trail
	// line: concurrent calls must not overrun it, and a refused call must not read as a run.
	s.mu.Lock()
	if s.codeRuns >= maxCodeRuns {
		s.mu.Unlock()
		s.reject("run_code")
		return codeResult{Error: "code run limit reached, conclude with what you have"}
	}
	s.codeRuns++
	n := s.codeRuns
	s.mu.Unlock()
	s.begin(ctx, "run_code", struct {
		Language string `json:"language"`
	}{args.Language}, fmt.Sprintf("Running %s in the sandbox", args.Language))

	res, err := s.a.cfg.Sandbox.Run(ctx, args.Language, args.Code, s.dir)
	out := res.Stdout + res.Stderr
	if err != nil {
		out += "\nsandbox error: " + err.Error()
	}
	s.progress.Snippet(ctx, fmt.Sprintf("code-%d.txt", n), fmt.Sprintf("%s run %d", args.Language, n),
		args.Code+"\n--- output ---\n"+out)
	if err != nil {
		return codeResult{Error: err.Error()}
	}
	return codeResult{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode, TimedOut: res.TimedOut}
}

func (s *runState) writeJSON(name string, v any) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	s.writeEvidence(name, raw)
}

// readTools returns the tools that only read: they never change git or the cluster.
// run_code is included only when the sandbox exists.
func (a *Agent) readTools(s *runState) ([]tool.Tool, error) {
	var tools []tool.Tool
	var firstErr error
	add := func(t tool.Tool, err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if err == nil {
			tools = append(tools, t)
		}
	}

	add(functiontool.New(functiontool.Config{
		Name:        "list_disagreements",
		Description: "List every service and environment where git and the cluster disagree: version mismatch, missing on either side, not synced, not healthy, or different from the previous environment's git version.",
	}, func(ctx agent.Context, _ noArgs) (factsResult, error) { return s.listDisagreements(ctx), nil }))

	add(functiontool.New(functiontool.Config{
		Name:        "get_service_facts",
		Description: "Everything known about one service in every environment, agreeing or not: git and running versions, Argo CD sync and health, last sync message.",
	}, func(ctx agent.Context, args serviceArgs) (factsResult, error) { return s.serviceFacts(ctx, args), nil }))

	add(functiontool.New(functiontool.Config{
		Name:        "refresh_app",
		Description: "Ask Argo CD to re-read git for one service in one environment. Use it when facts look stale; it does not deploy anything.",
	}, func(ctx agent.Context, args refreshArgs) (refreshResult, error) { return s.refreshApp(ctx, args), nil }))

	add(functiontool.New(functiontool.Config{
		Name:        "get_overlay",
		Description: "Read an environment's kustomization overlay from the GitOps repo, with its path and blob SHA.",
	}, func(ctx agent.Context, args envArgs) (overlayResult, error) { return s.getOverlay(ctx, args), nil }))

	add(functiontool.New(functiontool.Config{
		Name:        "get_service_health",
		Description: "Error rate, p99 latency and request rate for a service in an environment from Grafana, to tell whether a drift is hurting users. Returns status \"not configured\" when Grafana is off.",
	}, func(ctx agent.Context, args healthArgs) (healthResult, error) { return s.serviceHealth(ctx, args), nil }))

	add(functiontool.New(functiontool.Config{
		Name:        "search_tickets",
		Description: "Recent JIRA tickets mentioning a service, up to 10. Returns status \"not configured\" when JIRA is off.",
	}, func(ctx agent.Context, args ticketArgs) (ticketsResult, error) {
		return s.searchTickets(ctx, args), nil
	}))

	if a.SandboxAvailable() {
		add(functiontool.New(functiontool.Config{
			Name:        "run_code",
			Description: "Run a short Python or bash script in an isolated sandbox with no network, for analysis the other tools cannot do. Evidence from earlier tool calls is in /data as files. At most 5 runs.",
		}, func(ctx agent.Context, args codeArgs) (codeResult, error) { return s.runCode(ctx, args), nil }))
	}
	return tools, firstErr
}
