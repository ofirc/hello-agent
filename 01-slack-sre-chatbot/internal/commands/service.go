package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/grafana"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/jira"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/slackio"
)

// runServiceList is the inventory command: which services does the agent know about at
// all, and where does each one live.
//
// It exists because the demo has one service and production has fifty. `version get`
// answers "what version is everything on", which stops being readable past a dozen
// rows. This answers the prior question, "what is there", and is the command someone
// runs when they do not yet know the name to type.
//
// Services are discovered, never configured. The union of what the GitOps overlays
// declare and what Argo CD reports is the inventory, so onboarding a service means
// adding it to an overlay and nothing else.
func runServiceList(ctx context.Context, d *Deps, in *Invocation) error {
	in.Rep.Step(ctx, "• enumerating services...")
	snap, err := gather(ctx, d)
	if err != nil {
		return err
	}

	services := snap.services()
	if len(services) == 0 {
		in.Rep.Done(ctx, []slack.Block{
			slackio.Section(":grey_question: *No services found.*\nEither the overlays under `" +
				d.Cfg.GitOpsPrefix + "` declare no images, or Argo CD has no applications in project `" +
				d.Cfg.ArgoCDProject + "`."),
		}, "no services")
		return nil
	}

	findings := snap.findings(d.Cfg.Environments)
	byService := map[string][]drift.Finding{}
	for _, f := range findings {
		byService[f.Service] = append(byService[f.Service], f)
	}

	prod := prodEnv(d.Cfg.Environments)
	rows := make([][]string, 0, len(services))
	drifted := 0

	for _, svc := range services {
		// Which environments actually run this service, in configured order.
		var live []string
		for _, env := range d.Cfg.Environments {
			if _, ok := snap.actualFor(env, svc); ok {
				live = append(live, env)
			}
		}

		prodVersion := "-"
		if a, ok := snap.actualFor(prod, svc); ok {
			prodVersion = dash(a.Version)
		}

		status := "ok"
		if fs := byService[svc]; len(fs) > 0 {
			drifted++
			// fs is already severity-ordered by Compute, so the first one is the worst.
			status = fmt.Sprintf("%s %s", fs[0].Emoji(), string(fs[0].Kind))
		}

		rows = append(rows, []string{
			svc,
			dash(strings.Join(live, ",")),
			prodVersion,
			status,
		})
	}

	header := fmt.Sprintf("*Services* - %d known, %d with drift\n", len(services), drifted)
	body := table([]string{"SERVICE", "ENVIRONMENTS", strings.ToUpper(prod), "STATUS"}, rows)

	blocks := []slack.Block{
		slackio.Section(header + body),
		slackio.Context(fmt.Sprintf(
			"`/sre service get <service>` for detail  |  discovered from `%s` and Argo CD project `%s`",
			d.Cfg.GitOpsPrefix, d.Cfg.ArgoCDProject)),
	}
	in.Rep.Done(ctx, blocks, fmt.Sprintf("%d services", len(services)))
	return nil
}

// runServiceGet is the detail view for one service: every environment, what git wants,
// what the cluster runs, how Argo CD feels about it, plus symptoms and reports.
//
// Think kubectl describe rather than kubectl get. It is the command to run after
// `service list` names something interesting, and it deliberately shows all four
// signals side by side, because the useful judgement is almost always a correlation
// rather than any single number.
func runServiceGet(ctx context.Context, d *Deps, in *Invocation) error {
	service := in.Arg(0)
	if service == "" {
		return &ErrUsage{Msg: "which service? for example `/sre service get product-catalog`. `/sre service list` shows the options."}
	}

	in.Rep.Step(ctx, "• gathering state for `%s`...", service)
	snap, err := gather(ctx, d)
	if err != nil {
		return err
	}

	// Be forgiving about the name. Someone reading a dashboard will type a prefix or
	// get the hyphens wrong, and an unhelpful "not found" wastes a round trip during
	// an incident.
	resolved, suggestions := resolveService(snap.services(), service)
	if resolved == "" {
		msg := fmt.Sprintf("I do not know a service called `%s`.", service)
		if len(suggestions) > 0 {
			msg += " Did you mean " + strings.Join(backtick(suggestions), " or ") + "?"
		} else {
			msg += " Run `/sre service list` to see what I do know about."
		}
		return &ErrUsage{Msg: msg}
	}
	service = resolved

	var rows [][]string
	var image string
	for _, env := range d.Cfg.Environments {
		des, hasDes := snap.desiredFor(env, service)
		act, hasAct := snap.actualFor(env, service)
		if !hasDes && !hasAct {
			continue
		}
		if act.Image != "" && image == "" {
			image = act.Image
		}
		age := "-"
		if !act.LastSyncAt.IsZero() {
			age = humanAge(time.Since(act.LastSyncAt))
		}
		rows = append(rows, []string{
			env,
			dash(act.Version),
			dash(des.Version),
			dash(act.SyncStatus),
			dash(act.Health),
			age,
		})
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("*%s*\n", service))
	if image != "" {
		b.WriteString("`" + image + "`\n")
	}
	b.WriteString(table([]string{"ENV", "RUNNING", "GIT", "SYNC", "HEALTH", "LAST SYNC"}, rows))

	blocks := []slack.Block{slackio.Section(b.String())}

	// Findings for this service only.
	findings := drift.ForService(snap.findings(d.Cfg.Environments), service)
	if len(findings) == 0 {
		blocks = append(blocks, slackio.Section(":white_check_mark: No drift. Every environment matches its overlay."))
	} else {
		var f strings.Builder
		f.WriteString("*Findings*\n")
		for _, x := range findings {
			f.WriteString(fmt.Sprintf("%s %s\n", x.Emoji(), x.String()))
		}
		blocks = append(blocks, slackio.Section(f.String()))
	}

	// Symptoms and reports, concurrently and best-effort. A detail view missing its
	// Grafana line is still a useful detail view.
	worstEnv := prodEnv(d.Cfg.Environments)
	if len(findings) > 0 {
		worstEnv = findings[0].Env
	}

	var (
		health  grafana.Health
		hErr    error
		tickets []jira.Issue
		jErr    error
	)
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		if d.Grafana.Configured() {
			health, hErr = d.Grafana.ServiceHealth(ctx, service, worstEnv, 6*time.Hour)
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		if d.JIRA.Configured() {
			tickets, jErr = d.JIRA.RecentForService(ctx, service, 30, 3)
		}
	}()
	<-done
	<-done

	var side strings.Builder
	if d.Grafana.Configured() {
		if hErr != nil {
			side.WriteString(fmt.Sprintf("*Grafana* unavailable (%v)\n", hErr))
		} else {
			side.WriteString(fmt.Sprintf("*Grafana* (%s, 6h) %s\n", worstEnv, health.String()))
		}
	}
	if d.JIRA.Configured() {
		switch {
		case jErr != nil:
			side.WriteString(fmt.Sprintf("*JIRA* unavailable (%v)\n", jErr))
		case len(tickets) == 0:
			side.WriteString("*JIRA* no tickets in the last 30 days\n")
		default:
			side.WriteString("*JIRA* last 30 days\n")
			for _, t := range tickets {
				side.WriteString("    - " + t.String() + "\n")
			}
		}
	}
	if side.Len() > 0 {
		blocks = append(blocks, slackio.Section(side.String()))
	}

	next := fmt.Sprintf("`/sre rollout status %s`", service)
	if len(findings) > 0 {
		next = fmt.Sprintf("`/sre drift debug %s` to triage", service)
		if findings[0].Fixable() {
			next += fmt.Sprintf("  |  `/sre drift fix %s` to open a PR", service)
		}
	}
	blocks = append(blocks, slackio.Context(next))

	in.Rep.Done(ctx, blocks, "service detail for "+service)
	return nil
}

// resolveService matches exactly, then case-insensitively, then by prefix. It returns
// the resolved name, or candidates when the input was ambiguous or simply wrong.
func resolveService(known []string, want string) (resolved string, suggestions []string) {
	for _, s := range known {
		if s == want {
			return s, nil
		}
	}
	lower := strings.ToLower(want)
	for _, s := range known {
		if strings.ToLower(s) == lower {
			return s, nil
		}
	}

	var prefix []string
	for _, s := range known {
		if strings.HasPrefix(strings.ToLower(s), lower) || strings.Contains(strings.ToLower(s), lower) {
			prefix = append(prefix, s)
		}
	}
	// Exactly one candidate is a typo worth resolving silently. Several is ambiguous,
	// and guessing which service someone meant during an incident is not a kindness.
	if len(prefix) == 1 {
		return prefix[0], nil
	}
	if len(prefix) > 1 {
		return "", prefix
	}
	if len(known) <= 5 {
		return "", known
	}
	return "", nil
}

func backtick(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = "`" + s + "`"
	}
	return out
}
