package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/gitops"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/grafana"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/jira"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/slackio"
)

// runDriftList is the fast read: what disagrees, with no interpretation.
func runDriftList(ctx context.Context, d *Deps, in *Invocation) error {
	in.Rep.Step(ctx, "• diffing git against the cluster...")
	snap, err := gather(ctx, d)
	if err != nil {
		return err
	}

	findings := snap.findings(d.Cfg.Environments)
	if env := in.Arg(0); env != "" {
		if !d.Cfg.IsEnv(env) {
			return &ErrUsage{Msg: fmt.Sprintf("`%s` is not an environment", env)}
		}
		var filtered []drift.Finding
		for _, f := range findings {
			if f.Env == env {
				filtered = append(filtered, f)
			}
		}
		findings = filtered
	}

	if len(findings) == 0 {
		in.Rep.Done(ctx, []slack.Block{
			slackio.Section(":white_check_mark: *No drift.* Every environment matches its overlay, and the overlays agree with each other."),
			slackio.Context(fmt.Sprintf("%d services checked across %s",
				len(snap.services()), strings.Join(d.Cfg.Environments, ", "))),
		}, "no drift")
		return nil
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("*%d drift finding%s*\n", len(findings), plural2(len(findings))))
	for _, f := range findings {
		b.WriteString(fmt.Sprintf("%s %s\n", f.Emoji(), f.String()))
	}

	blocks := []slack.Block{slackio.Section(b.String())}
	if top := firstFixable(findings); top != nil {
		blocks = append(blocks, slackio.Context(fmt.Sprintf(
			"`/sre drift debug %s` to triage  |  `/sre drift fix %s` to open a PR", top.Service, top.Service)))
	}
	in.Rep.Done(ctx, blocks, fmt.Sprintf("%d drift findings", len(findings)))
	return nil
}

// runDriftDebug is the agentic command.
//
// The shape matters: every fact is gathered deterministically by the providers, and the
// model is handed the assembled evidence to rank and explain. The model never decides
// what to fetch and never calls a tool, so a bad generation produces a confusing
// paragraph rather than an unexpected API call against production.
func runDriftDebug(ctx context.Context, d *Deps, in *Invocation) error {
	in.Rep.Step(ctx, "• diffing git against the cluster...")
	snap, err := gather(ctx, d)
	if err != nil {
		return err
	}

	findings := snap.findings(d.Cfg.Environments)
	if len(findings) == 0 {
		in.Rep.Done(ctx, []slack.Block{
			slackio.Section(":white_check_mark: *Nothing to triage.* No drift between git and any environment."),
		}, "no drift")
		return nil
	}

	// Narrow to one service: either the one asked for, or the worst one found.
	target := in.Arg(0)
	if target != "" {
		findings = drift.ForService(findings, target)
		if len(findings) == 0 {
			return &ErrUsage{Msg: fmt.Sprintf("no drift found for `%s`. `/sre drift list` shows what has drifted.", target)}
		}
	}
	primary := findings[0]
	target = primary.Service
	in.Rep.Step(ctx, "• worst finding: %s %s", primary.Emoji(), primary.String())

	// Rule out the boring explanation before building a theory: Argo may simply not
	// have looked at the repo recently.
	if a, ok := snap.actualFor(primary.Env, target); ok && a.App != "" {
		in.Rep.Step(ctx, "• asking Argo CD to refresh `%s`...", a.App)
		if err := d.Argo.Refresh(ctx, a.App); err != nil {
			in.Rep.Step(ctx, "   (refresh failed: %v, continuing with cached state)", err)
		}
	}

	// Symptoms and reports, concurrently. Neither is required: a triage with no
	// Grafana is less useful, not useless.
	type evidence struct {
		health  grafana.Health
		hErr    error
		tickets []jira.Issue
		jErr    error
	}
	var ev evidence
	done := make(chan struct{}, 2)

	in.Rep.Step(ctx, "• pulling Grafana metrics and JIRA tickets...")
	go func() {
		defer func() { done <- struct{}{} }()
		if d.Grafana.Configured() {
			ev.health, ev.hErr = d.Grafana.ServiceHealth(ctx, target, primary.Env, 6*time.Hour)
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		if d.JIRA.Configured() {
			ev.tickets, ev.jErr = d.JIRA.RecentForService(ctx, target, 30, 5)
		}
	}()
	<-done
	<-done

	// Assemble the evidence block. This same text is both what the model sees and what
	// gets posted, so a reader can always check the model's work against its inputs.
	var facts strings.Builder
	facts.WriteString("*Evidence*\n")
	for _, f := range findings {
		facts.WriteString(fmt.Sprintf("• git vs cluster: %s\n", f.String()))
	}
	if a, ok := snap.actualFor(primary.Env, target); ok {
		facts.WriteString(fmt.Sprintf("• Argo CD: sync=%s health=%s", dash(a.SyncStatus), dash(a.Health)))
		if !a.LastSyncAt.IsZero() {
			facts.WriteString(", last sync " + humanAge(time.Since(a.LastSyncAt)))
		}
		facts.WriteString("\n")
		if a.LastMessage != "" {
			facts.WriteString(fmt.Sprintf("• Argo CD message: %s\n", a.LastMessage))
		}
	}
	switch {
	case ev.hErr != nil:
		facts.WriteString(fmt.Sprintf("• Grafana: unavailable (%v)\n", ev.hErr))
	case d.Grafana.Configured():
		facts.WriteString(fmt.Sprintf("• Grafana (%s, 6h): %s\n", primary.Env, ev.health.String()))
	}
	switch {
	case ev.jErr != nil:
		facts.WriteString(fmt.Sprintf("• JIRA: unavailable (%v)\n", ev.jErr))
	case len(ev.tickets) > 0:
		facts.WriteString(fmt.Sprintf("• JIRA, last 30 days (%d):\n", len(ev.tickets)))
		for _, t := range ev.tickets {
			facts.WriteString("    - " + t.String() + "\n")
		}
	case d.JIRA.Configured():
		facts.WriteString("• JIRA: no tickets mentioning this service in 30 days\n")
	}

	// The narrative. Vertex if available, deterministic otherwise, and the fallback is
	// good enough that nobody watching a demo can tell which one ran.
	in.Rep.Step(ctx, "• correlating...")
	narrative := deterministicNarrative(primary, ev.health, ev.tickets, d.Grafana.Configured())
	if d.LLM != nil {
		if text, err := d.LLM.Generate(ctx, triageSystemPrompt, facts.String()); err != nil {
			d.Log.Warn("vertex unavailable, using deterministic narrative", "err", err)
		} else {
			narrative = text
		}
	}

	blocks := []slack.Block{
		slackio.Section(fmt.Sprintf("%s *Triage: `%s` in `%s`*", primary.Emoji(), target, primary.Env)),
		slackio.Section(facts.String()),
		slackio.Section(":brain: *Root cause*\n" + narrative),
	}

	if plan := primary.Plan(); plan != nil {
		blocks = append(blocks, slackio.Section(
			fmt.Sprintf(":wrench: *Suggested fix*\n`%s`: `%s` -> `%s`\n_%s_",
				plan.Path, plan.From, plan.To, plan.Rationale)))
		blocks = append(blocks, actionBar(target, primary.Env, d.Grafana.DashboardURL(target, primary.Env)))
	} else {
		blocks = append(blocks, slackio.Context(
			"No pull request can fix this one. It is a workload problem, not a version problem."))
	}

	in.Rep.Done(ctx, blocks, "triage for "+target)
	return nil
}

const triageSystemPrompt = `You are an SRE triaging a release problem. You are given evidence already gathered from git, Argo CD, Grafana and JIRA. Do not speculate beyond it and do not invent versions, ticket IDs or metrics.

Write at most four short lines of Slack mrkdwn:
1. What is wrong, in one sentence a sleepy on-call engineer understands.
2. The most likely cause, and the single piece of evidence that points to it.
3. Whether users are affected right now. If the error rate is near zero, say the old version is serving correctly and this is a staleness problem, not an outage.
4. The next action.

No preamble, no headings, no bullet characters other than a leading dash.`

// deterministicNarrative is the fallback, and the reason a Vertex outage cannot break
// the demo. It encodes the same reasoning the prompt asks the model for.
func deterministicNarrative(f drift.Finding, h grafana.Health, tickets []jira.Issue, haveMetrics bool) string {
	var b strings.Builder

	switch f.Kind {
	case drift.KindSyncFailed:
		b.WriteString(fmt.Sprintf("- `%s` in `%s` is stuck on %s. Argo CD tried to move it to %s and failed: %s\n",
			f.Service, f.Env, f.Actual, f.Desired, f.Detail))
		b.WriteString("- The rollout was attempted and lost, so this is a deploy failure rather than a forgotten promotion.\n")
	case drift.KindVersionSkew:
		b.WriteString(fmt.Sprintf("- `%s` in `%s` runs %s while git declares %s.\n", f.Service, f.Env, f.Actual, f.Desired))
		b.WriteString("- Argo CD reports no sync error, so the likely cause is that the sync never started: a paused application, a missing image, or auto-sync turned off.\n")
	case drift.KindPromotionGap:
		b.WriteString(fmt.Sprintf("- `%s` in `%s` is on %s while `%s` already runs %s.\n", f.Service, f.Env, f.Actual, f.Detail, f.Desired))
		b.WriteString("- Nothing failed. The release was simply never promoted, so the overlay still pins the older tag.\n")
	case drift.KindUnhealthy:
		b.WriteString(fmt.Sprintf("- `%s` in `%s` runs the right version (%s) but is %s.\n", f.Service, f.Env, f.Actual, f.Detail))
	default:
		b.WriteString("- " + f.String() + "\n")
	}

	switch {
	case !haveMetrics:
		b.WriteString("- User impact unknown, Grafana is not wired up.\n")
	case h.Symptomatic():
		b.WriteString(fmt.Sprintf("- Users are affected: %s.\n", h.String()))
	case h.RPS > 0:
		b.WriteString(fmt.Sprintf("- No user-visible errors (%s). The old version is serving correctly, this is staleness rather than an outage.\n", h.String()))
	default:
		b.WriteString("- No traffic recorded, so no measurable user impact.\n")
	}

	if len(tickets) > 0 {
		b.WriteString(fmt.Sprintf("- %s was already reported: %s\n", tickets[0].Key, tickets[0].Summary))
	}

	if plan := f.Plan(); plan != nil {
		b.WriteString(fmt.Sprintf("- Next: `/sre drift fix %s`", f.Service))
	} else {
		b.WriteString("- Next: this needs a human, a version bump will not fix it.")
	}
	return b.String()
}

// runDriftFix turns a confirmed finding into a pull request. It never merges.
func runDriftFix(ctx context.Context, d *Deps, in *Invocation) error {
	service := in.Arg(0)
	if service == "" {
		return &ErrUsage{Msg: "which service? for example `/sre drift fix product-catalog`"}
	}

	in.Rep.Step(ctx, "• confirming the drift is still there...")
	snap, err := gather(ctx, d)
	if err != nil {
		return err
	}

	findings := drift.ForService(snap.findings(d.Cfg.Environments), service)
	if env := in.Arg(1); env != "" {
		var filtered []drift.Finding
		for _, f := range findings {
			if f.Env == env {
				filtered = append(filtered, f)
			}
		}
		findings = filtered
	}
	if len(findings) == 0 {
		in.Rep.Done(ctx, []slack.Block{
			slackio.Section(fmt.Sprintf(":white_check_mark: `%s` has no drift to fix.", service)),
		}, "nothing to fix")
		return nil
	}

	target := firstFixable(findings)
	if target == nil {
		return &ErrUsage{Msg: fmt.Sprintf(
			"`%s` has drift, but not the kind a PR fixes:\n%s\nRun `/sre drift debug %s`.",
			service, findings[0].String(), service)}
	}
	if !d.Cfg.CanFix(target.Env) {
		return &ErrUsage{Msg: fmt.Sprintf(
			"I am not allowed to open fix PRs against `%s`. Allowed: %s",
			target.Env, strings.Join(d.Cfg.AllowFixEnvs, ", "))}
	}

	plan := target.Plan()
	in.Rep.Step(ctx, "• plan: `%s` %s -> %s", plan.Path, plan.From, plan.To)

	// Read the overlay from the base branch, not from a cached copy: someone may have
	// fixed it by hand in the time it took to run the triage.
	file, err := d.GH.GetFile(ctx, plan.Path, d.Cfg.GitHubBase)
	if err != nil {
		return fmt.Errorf("reading %s: %w", plan.Path, err)
	}
	if file == nil {
		return fmt.Errorf("%s does not exist on %s", plan.Path, d.Cfg.GitHubBase)
	}

	updated, err := gitops.SetTag(file.Content, service, plan.To)
	if err != nil {
		return fmt.Errorf("editing %s: %w", plan.Path, err)
	}
	if updated == file.Content {
		in.Rep.Done(ctx, []slack.Block{
			slackio.Section(fmt.Sprintf(
				":information_source: `%s` already pins `%s` to `%s` on `%s`. Someone fixed it already, or Argo CD has not caught up yet.",
				plan.Path, service, plan.To, d.Cfg.GitHubBase)),
		}, "already fixed")
		return nil
	}

	branch := fmt.Sprintf("sre-agent/%s-%s-%s", target.Env, service, strings.ReplaceAll(plan.To, "/", "-"))
	in.Rep.Step(ctx, "• branching `%s`...", branch)

	head, err := d.GH.HeadSHA(ctx, d.Cfg.GitHubBase)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", d.Cfg.GitHubBase, err)
	}
	if err := d.GH.CreateBranch(ctx, branch, head); err != nil {
		return fmt.Errorf("creating branch: %w", err)
	}

	in.Rep.Step(ctx, "• committing...")
	if err := d.GH.PutFile(ctx, branch, plan.Path, updated, file.SHA, plan.Title); err != nil {
		return fmt.Errorf("committing: %w", err)
	}

	in.Rep.Step(ctx, "• opening the pull request...")
	body := prBody(d, in, *target, plan)
	pr, err := d.GH.CreatePR(ctx, branch, d.Cfg.GitHubBase, plan.Title, body)
	if err != nil {
		return fmt.Errorf("opening PR: %w", err)
	}
	d.GH.AddLabels(ctx, pr.Number, "sre-agent", "drift")

	d.Log.Info("fix PR opened", "pr", pr.HTMLURL, "service", service, "env", target.Env,
		"from", plan.From, "to", plan.To, "requested_by", in.UserID)

	blocks := []slack.Block{
		slackio.Section(fmt.Sprintf(":white_check_mark: *PR opened* <%s|#%d>\n*%s*", pr.HTMLURL, pr.Number, plan.Title)),
		slackio.Section(fmt.Sprintf("```\n%s\n  %s -> %s\n```", plan.Path, plan.From, plan.To)),
		slackio.Section("_" + plan.Rationale + "_"),
		prActionBar(pr.HTMLURL, service, target.Env),
		slackio.Context(fmt.Sprintf("requested by %s | the agent does not merge, that stays with a human", in.UserTag)),
	}
	in.Rep.Done(ctx, blocks, "PR opened: "+pr.HTMLURL)
	return nil
}

func prBody(d *Deps, in *Invocation, f drift.Finding, plan *drift.FixPlan) string {
	var b strings.Builder
	b.WriteString("Opened by the SRE Agent from Slack.\n\n")
	b.WriteString("## What drifted\n\n")
	b.WriteString(f.String() + "\n\n")
	b.WriteString("## What this PR changes\n\n")
	b.WriteString(fmt.Sprintf("`%s`: `%s` -> `%s`\n\n", plan.Path, plan.From, plan.To))
	b.WriteString("## Why\n\n")
	b.WriteString(plan.Rationale + "\n\n")
	b.WriteString("## Before merging\n\n")
	switch f.Kind {
	case drift.KindPromotionGap:
		b.WriteString(fmt.Sprintf("- [ ] `%s` has been running `%s` long enough to trust it\n", f.Detail, plan.To))
		b.WriteString("- [ ] No open incident on this service\n")
	default:
		b.WriteString(fmt.Sprintf("- [ ] Understood why `%s` could not be deployed\n", plan.From))
		b.WriteString(fmt.Sprintf("- [ ] A follow-up exists to roll forward past `%s`\n", plan.To))
	}
	b.WriteString(fmt.Sprintf("\n---\nRequested by <@%s> in Slack. Generated from live Argo CD state, not from a template.\n", in.UserID))
	return b.String()
}

func actionBar(service, env, dashboardURL string) slack.Block {
	fixBtn := slack.NewButtonBlockElement("drift_fix", service+" "+env,
		slack.NewTextBlockObject(slack.PlainTextType, "Open fix PR", true, false))
	fixBtn.Style = slack.StylePrimary

	elems := []slack.BlockElement{fixBtn,
		slack.NewButtonBlockElement("drift_recheck", service,
			slack.NewTextBlockObject(slack.PlainTextType, "Re-check", true, false)),
	}
	if dashboardURL != "" {
		b := slack.NewButtonBlockElement("open_grafana", service,
			slack.NewTextBlockObject(slack.PlainTextType, "Grafana", true, false))
		b.URL = dashboardURL
		elems = append(elems, b)
	}
	return slack.NewActionBlock("drift_actions", elems...)
}

func prActionBar(prURL, service, env string) slack.Block {
	view := slack.NewButtonBlockElement("view_pr", prURL,
		slack.NewTextBlockObject(slack.PlainTextType, "Review PR", true, false))
	view.URL = prURL
	view.Style = slack.StylePrimary

	return slack.NewActionBlock("pr_actions", view,
		slack.NewButtonBlockElement("drift_recheck", service,
			slack.NewTextBlockObject(slack.PlainTextType, "Re-check drift", true, false)),
	)
}

func firstFixable(fs []drift.Finding) *drift.Finding {
	for i := range fs {
		if fs[i].Fixable() {
			return &fs[i]
		}
	}
	return nil
}

func plural2(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
