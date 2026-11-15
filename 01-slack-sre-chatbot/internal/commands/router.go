// Package commands owns the /sre surface: parsing, dispatch, and the handlers.
//
// # Naming convention
//
// Every command is noun-verb, the convention gcloud uses:
//
//	/sre <noun> <verb> [args]
//
// Not verb-noun. The choice matters more than it looks. As the surface grows, noun-first
// groups by the thing being operated on, so a user who knows "drift" can discover
// "drift list", "drift debug" and "drift fix" without reading docs. Verb-first scatters
// those three across the alphabet.
//
// The original command set mixed both conventions ("version get" is noun-verb,
// "debug-drift" is verb-noun) and mixed separators (space vs hyphen), which forced the
// parser to special-case each command. The old spellings survive as aliases below, so
// anything written against them keeps working.
package commands

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/config"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/llm"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/argocd"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/github"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/gitops"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/grafana"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/jira"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/slackio"
)

// Deps is everything a handler is allowed to touch. Passing one struct rather than
// eight arguments keeps handler signatures stable as providers come and go.
type Deps struct {
	Cfg     *config.Config
	Slack   *slack.Client
	Argo    *argocd.Client
	GH      *github.Client
	GitOps  *gitops.Client
	Grafana *grafana.Client
	JIRA    *jira.Client
	LLM     *llm.Client
	Log     *slog.Logger
}

// Invocation is one parsed command from one user.
type Invocation struct {
	Noun    string
	Verb    string
	Args    []string
	Raw     string
	UserID  string
	UserTag string // <@U123> for mentions in the reply
	Channel string
	Rep     *slackio.Reporter
}

func (i *Invocation) Arg(n int) string {
	if n < len(i.Args) {
		return i.Args[n]
	}
	return ""
}

type handler func(ctx context.Context, d *Deps, in *Invocation) error

type spec struct {
	Noun    string
	Verb    string // empty for single-token commands like help
	Args    string // usage fragment
	Summary string
	Timeout time.Duration
	Needs   []string // integrations that must be configured
	Run     handler
}

func (s spec) name() string {
	if s.Verb == "" {
		return s.Noun
	}
	return s.Noun + " " + s.Verb
}

func (s spec) usage() string {
	u := "/sre " + s.name()
	if s.Args != "" {
		u += " " + s.Args
	}
	return u
}

type Router struct {
	deps    *Deps
	specs   map[string]spec
	aliases map[string]string
	sem     chan struct{}
}

func NewRouter(d *Deps) *Router {
	q := d.Cfg.QuickTimeout
	r := &Router{
		deps:  d,
		specs: map[string]spec{},
		// Spellings that are not canonical but that people reach for anyway: the old
		// hyphenated command set, and the verb-first forms that anyone coming from
		// kubectl will type by reflex. Resolve() names the canonical form once, then
		// runs it, so the alias teaches rather than just tolerating.
		//
		// Keys may be one or two tokens. Two-token keys are matched first, which is
		// what lets "list services" resolve without "list" having to be a noun.
		aliases: map[string]string{
			// verb-first, the kubectl reflex
			"list services":    "service list",
			"list service":     "service list",
			"get services":     "service list",
			"get service":      "service get",
			"describe service": "service get",
			"list drift":       "drift list",
			"get version":      "version get",
			"get versions":     "version get",

			// the original hyphenated command set
			"debug-drift": "drift debug",
			"fix-drift":   "drift fix",
			"drift-debug": "drift debug",
			"drift-fix":   "drift fix",
			"get-version": "version get",

			// bare conveniences
			"services": "service list",
			"versions": "version get",
			"status":   "rollout status",
			"h":        "help",
			"-h":       "help",
			"--help":   "help",
			"?":        "help",
		},
		sem: make(chan struct{}, d.Cfg.MaxConcurrent),
	}

	for _, s := range []spec{
		{Noun: "help", Summary: "show this message", Timeout: q, Run: runHelp},
		{Noun: "doctor", Summary: "check which integrations are wired up", Timeout: q, Run: runDoctor},

		{Noun: "version", Verb: "get", Args: "[env]",
			Summary: "deployed version of every service, per environment",
			Timeout: q, Needs: []string{"github", "argocd"}, Run: runVersionGet},
		{Noun: "version", Verb: "diff", Args: "<envA> <envB>",
			Summary: "compare two environments side by side",
			Timeout: q, Needs: []string{"argocd"}, Run: runVersionDiff},

		{Noun: "drift", Verb: "list", Args: "[env]",
			Summary: "what has drifted from git, read-only and fast",
			Timeout: q, Needs: []string{"github", "argocd"}, Run: runDriftList},
		{Noun: "drift", Verb: "debug", Args: "[service]",
			Summary: "agentic triage: correlate git, Argo CD, Grafana and JIRA, then name a root cause",
			Timeout: d.Cfg.TriageTimeout, Needs: []string{"github", "argocd"}, Run: runDriftDebug},
		{Noun: "drift", Verb: "fix", Args: "<service> [env]",
			Summary: "open a PR that realigns the environment with the known-good version",
			Timeout: d.Cfg.RemedyTimeout, Needs: []string{"github", "argocd"}, Run: runDriftFix},

		{Noun: "rollout", Verb: "status", Args: "<service> [env]",
			Summary: "Argo CD sync and health for one service",
			Timeout: q, Needs: []string{"argocd"}, Run: runRolloutStatus},

		{Noun: "service", Verb: "list",
			Summary: "every service the agent knows about, and where it runs",
			Timeout: q, Needs: []string{"github", "argocd"}, Run: runServiceList},
		{Noun: "service", Verb: "get", Args: "<service>",
			Summary: "full detail for one service: versions, sync, health, metrics, tickets",
			Timeout: 2 * q, Needs: []string{"github", "argocd"}, Run: runServiceGet},
	} {
		r.specs[s.name()] = s
	}
	return r
}

type ctxKey int

const routerKey ctxKey = iota

// routerFrom lets the help handler introspect the spec table without the Deps struct
// having to carry a pointer back to the Router that owns it.
func routerFrom(ctx context.Context) *Router {
	r, _ := ctx.Value(routerKey).(*Router)
	return r
}

// ErrUsage means the input was not understood. It is reported to the user as help
// rather than as a failure, because a typo is not an incident.
type ErrUsage struct{ Msg string }

func (e *ErrUsage) Error() string { return e.Msg }

// dealias rewrites a non-canonical spelling into the canonical one and returns a note
// naming the canonical form, so an alias teaches rather than merely tolerating.
//
// Two-token keys are tried before single-token ones. Longest match has to win, or
// "get service product-catalog" would resolve on "get" alone and lose its noun.
func (r *Router) dealias(fields []string) ([]string, string) {
	if len(fields) >= 2 {
		two := strings.ToLower(fields[0] + " " + fields[1])
		if canon, ok := r.aliases[two]; ok {
			return append(strings.Fields(canon), fields[2:]...), aliasNote(two, canon)
		}
	}
	one := strings.ToLower(fields[0])
	if canon, ok := r.aliases[one]; ok {
		return append(strings.Fields(canon), fields[1:]...), aliasNote(one, canon)
	}
	return fields, ""
}

func aliasNote(from, canon string) string {
	if from == canon {
		return ""
	}
	return fmt.Sprintf("_`%s` works, the canonical form is `/sre %s`_", from, canon)
}

// Resolve parses raw command text into an Invocation. Exported so the app-mention path
// can reuse exactly the same grammar as the slash command.
func (r *Router) Resolve(text string) (spec, []string, string, error) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return r.specs["help"], nil, "", nil
	}

	fields, note := r.dealias(fields)
	noun := strings.ToLower(fields[0])

	// Single-token commands.
	if s, ok := r.specs[noun]; ok && s.Verb == "" {
		return s, fields[1:], note, nil
	}

	if len(fields) == 1 {
		verbs := r.verbsFor(noun)
		if len(verbs) == 0 {
			return spec{}, nil, "", &ErrUsage{Msg: fmt.Sprintf("I do not know `%s`.", noun)}
		}
		return spec{}, nil, "", &ErrUsage{
			Msg: fmt.Sprintf("`/sre %s` needs a verb: %s", noun, strings.Join(verbs, ", ")),
		}
	}

	verb := strings.ToLower(fields[1])
	s, ok := r.specs[noun+" "+verb]
	if !ok {
		verbs := r.verbsFor(noun)
		if len(verbs) == 0 {
			return spec{}, nil, "", &ErrUsage{Msg: fmt.Sprintf("I do not know `%s`.", noun)}
		}
		return spec{}, nil, "", &ErrUsage{
			Msg: fmt.Sprintf("`%s` is not a verb for `%s`. Try: %s", verb, noun, strings.Join(verbs, ", ")),
		}
	}
	return s, fields[2:], note, nil
}

func (r *Router) verbsFor(noun string) []string {
	var out []string
	for _, s := range r.specs {
		if s.Noun == noun && s.Verb != "" {
			out = append(out, "`"+s.Verb+"`")
		}
	}
	sort.Strings(out)
	return out
}

// Dispatch runs a command. It assumes the caller already acked Slack: everything here
// is allowed to take minutes.
func (r *Router) Dispatch(ctx context.Context, channel, userID, userName, text string) {
	s, args, note, err := r.Resolve(text)
	if err != nil {
		r.replyUsage(ctx, channel, err)
		return
	}

	// Backpressure. Without this, ten people typing drift debug during an incident
	// would open thirty concurrent provider calls and get everyone rate-limited.
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-time.After(2 * time.Second):
		r.replyText(ctx, channel, ":hourglass: I am at capacity on long-running jobs, try again in a moment.")
		return
	}

	if missing := r.missingIntegrations(s); len(missing) > 0 {
		r.replyText(ctx, channel, fmt.Sprintf(
			":warning: `%s` needs %s, which %s not configured. Run `/sre doctor` to see what is wired up.",
			s.usage(), strings.Join(missing, " and "), plural(len(missing))))
		return
	}

	in := &Invocation{
		Noun: s.Noun, Verb: s.Verb, Args: args, Raw: text,
		UserID: userID, UserTag: "<@" + userID + ">", Channel: channel,
		Rep: slackio.NewReporter(r.deps.Slack, channel, userID),
	}

	title := s.name()
	if len(args) > 0 {
		title += " " + strings.Join(args, " ")
	}
	if err := in.Rep.Start(ctx, title); err != nil {
		r.deps.Log.Error("cannot post anchor message", "err", err, "channel", channel)
		return
	}
	if note != "" {
		in.Rep.Step(ctx, "%s", note)
	}

	ctx = context.WithValue(ctx, routerKey, r)
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	start := time.Now()
	r.deps.Log.Info("command start", "cmd", s.name(), "user", userID, "channel", channel, "args", args)

	err = s.Run(ctx, r.deps, in)

	took := time.Since(start)
	if err != nil {
		var ue *ErrUsage
		if ok := asUsage(err, &ue); ok {
			in.Rep.Done(ctx, []slack.Block{slackio.Section(":information_source: " + ue.Msg),
				slackio.Context(s.usage())}, ue.Msg)
		} else if ctx.Err() == context.DeadlineExceeded {
			in.Rep.Fail(ctx, fmt.Errorf("timed out after %s, the slowest integration is usually the cause", s.Timeout))
		} else {
			in.Rep.Fail(ctx, err)
		}
		r.deps.Log.Error("command failed", "cmd", s.name(), "user", userID, "took", took, "err", err)
		return
	}
	r.deps.Log.Info("command ok", "cmd", s.name(), "user", userID, "took", took)
}

func (r *Router) missingIntegrations(s spec) []string {
	enabled := r.deps.Cfg.Enabled()
	var missing []string
	for _, need := range s.Needs {
		if !enabled[need] {
			missing = append(missing, need)
		}
	}
	return missing
}

func (r *Router) replyUsage(ctx context.Context, channel string, err error) {
	var ue *ErrUsage
	msg := err.Error()
	if ok := asUsage(err, &ue); ok {
		msg = ue.Msg
	}
	r.replyText(ctx, channel, ":information_source: "+msg+"\nRun `/sre help` for the full list.")
}

func (r *Router) replyText(ctx context.Context, channel, text string) {
	_, _, _ = r.deps.Slack.PostMessageContext(ctx, channel, slack.MsgOptionText(text, false))
}

func plural(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}
