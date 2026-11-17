package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/slack-go/slack"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/agentloop"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/slackio"
)

// runDriftList is the fast read: what disagrees, with no interpretation.
func runDriftList(ctx context.Context, d *Deps, in *Invocation) error {
	in.Rep.Step(ctx, "• diffing git against the cluster...")
	snap, err := gather(ctx, d)
	if err != nil {
		return err
	}

	rows := snap.table(d.Cfg.Environments).Disagreeing()
	if env := in.Arg(0); env != "" {
		if !d.Cfg.IsEnv(env) {
			return &ErrUsage{Msg: fmt.Sprintf("`%s` is not an environment", env)}
		}
		var filtered []drift.Row
		for _, r := range rows {
			if r.Env == env {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}

	if len(rows) == 0 {
		in.Rep.Done(ctx, []slack.Block{
			slackio.Section(":white_check_mark: *No differences.* Every environment matches its overlay, and the overlays agree with each other."),
			slackio.Context(fmt.Sprintf("%d services checked across %s",
				len(snap.services()), strings.Join(d.Cfg.Environments, ", "))),
		}, "no differences")
		return nil
	}

	blocks := []slack.Block{
		slackio.Section(fmt.Sprintf("*%d difference%s*\n%s", len(rows), plural2(len(rows)), differencesView(rows))),
		// Rows are production first, so the first one is the best place to start.
		slackio.Context(fmt.Sprintf("`/sre drift debug %s` to triage", rows[0].Service)),
	}
	in.Rep.Done(ctx, blocks, fmt.Sprintf("%d differences", len(rows)))
	return nil
}

// runDriftDebug hands the disagreeing rows to the debug agent and renders its answer.
//
// Go gathers the facts and renders the result; what a disagreement means, and what to
// look at next, is the agent's job, guided by the drift-triage skill. Every tool the
// agent has only reads, so a bad generation costs a wrong paragraph, never a change to
// git or the cluster.
func runDriftDebug(ctx context.Context, d *Deps, in *Invocation) error {
	// Enabled()["model"] only says a model is configured. The agent can still have
	// failed to start, and the router cannot tell.
	if d.Agent == nil {
		return &ErrUsage{Msg: "drift debug needs the model, which failed to start. Check the logs and /sre doctor."}
	}

	in.Rep.Step(ctx, "• diffing git against the cluster...")
	snap, err := gather(ctx, d)
	if err != nil {
		return err
	}
	table := snap.table(d.Cfg.Environments)

	service := in.Arg(0)
	var rows []drift.Row
	var refreshed bool
	if service != "" {
		if service, err = knownService(table.Services(), service); err != nil {
			return err
		}
		rows = table.DisagreeingFor(service)
		if len(rows) == 0 {
			in.Rep.Done(ctx, []slack.Block{
				slackio.Section(fmt.Sprintf(":white_check_mark: `%s` has no differences to triage.", service)),
			}, "nothing to triage")
			return nil
		}
		// Rule out the boring explanation before the model builds a theory: Argo CD may
		// simply not have looked at the repo recently. The model is told only when every
		// app was refreshed; after a failure it refreshes for itself.
		attempted, failed := 0, false
		for _, r := range rows {
			if r.App == "" {
				continue
			}
			attempted++
			in.Rep.Step(ctx, "• asking Argo CD to refresh `%s`...", r.App)
			if err := d.Argo.Refresh(ctx, r.App); err != nil {
				failed = true
				in.Rep.Step(ctx, "   (refresh failed: %v, continuing with cached state)", err)
			}
		}
		refreshed = attempted > 0 && !failed
	} else {
		rows = table.Disagreeing()
		if len(rows) == 0 {
			in.Rep.Done(ctx, []slack.Block{
				slackio.Section(":white_check_mark: *Nothing to triage.* No differences between git and any environment."),
			}, "nothing to triage")
			return nil
		}
	}

	res, err := d.Agent.Debug(ctx, agentloop.DebugRequest{
		Service: service, Rows: rows, Refreshed: refreshed, Progress: in.Rep,
	})
	if err != nil {
		return err
	}

	dashboard := ""
	if res.Fix != nil {
		dashboard = d.Grafana.DashboardURL(res.Service, res.Fix.Env)
	}
	in.Rep.Done(ctx, debugBlocks(res, rows, dashboard), "triage for "+res.Service)
	return nil
}

// runDriftFix asks the fix agent for an overlay edit and shows it with confirm and cancel
// buttons. Nothing is written here: the PR is opened by drift apply, and only when the
// requester clicks.
func runDriftFix(ctx context.Context, d *Deps, in *Invocation) error {
	service := in.Arg(0)
	if service == "" {
		return &ErrUsage{Msg: "which service? for example `/sre drift fix product-catalog`"}
	}
	if d.Agent == nil {
		return &ErrUsage{Msg: "drift fix needs the model, which failed to start. Check the logs and /sre doctor."}
	}
	if d.Proposals == nil {
		return fmt.Errorf("internal: proposal store is not configured")
	}
	env := in.Arg(1)
	if env != "" {
		if !d.Cfg.IsEnv(env) {
			return &ErrUsage{Msg: fmt.Sprintf("`%s` is not an environment", env)}
		}
		if !d.Cfg.CanFix(env) {
			return &ErrUsage{Msg: fmt.Sprintf(
				"I am not allowed to open fix PRs against `%s`. Allowed: %s",
				env, strings.Join(d.Cfg.AllowFixEnvs, ", "))}
		}
	}

	in.Rep.Step(ctx, "• diffing git against the cluster...")
	snap, err := gather(ctx, d)
	if err != nil {
		return err
	}
	table := snap.table(d.Cfg.Environments)

	// The facts' spelling, not the user's: it goes into the prompt, the proposal and
	// from there the branch name.
	if service, err = knownService(table.Services(), service); err != nil {
		return err
	}
	var rows []drift.Row
	for _, r := range table.DisagreeingFor(service) {
		if env == "" || r.Env == env {
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		in.Rep.Done(ctx, []slack.Block{
			slackio.Section(fmt.Sprintf(":white_check_mark: `%s` has no differences to fix.", service)),
		}, "nothing to fix")
		return nil
	}

	res, err := d.Agent.Fix(ctx, agentloop.FixRequest{
		Service: service, Env: env,
		Requester: in.UserID, Channel: in.Channel,
		Rows: rows, Progress: in.Rep,
	})
	if err != nil {
		return err
	}

	if res.Proposal == nil {
		d.Log.Info("fix declined", "service", service, "env", env, "requested_by", in.UserID)
		in.Rep.Done(ctx, fixDeclinedBlocks(service, res.Declined, rows), "no PR proposed for "+service)
		return nil
	}

	var id string
	blocks, err := presentProposal(*res.Proposal, in.UserTag,
		func(filename, title, content string) error { return in.Rep.SnippetErr(ctx, filename, title, content) },
		func(p agentloop.Proposal) string { id = d.Proposals.Put(p); return id })
	if err != nil {
		return err
	}
	d.Log.Info("fix proposed", "proposal", id, "service", service, "env", res.Proposal.Env, "requested_by", in.UserID)
	in.Rep.Done(ctx, blocks, "proposed change for "+service)
	return nil
}

// presentProposal stores the proposal and renders it, uploading the diff and PR body
// first when they cannot be shown inline.
//
// The order is the point. The confirm button is only safe if the requester can see the
// whole change and the PR text that goes out under their confirmation, so a snippet must
// be uploaded before the proposal is stored, and a failed upload leaves nothing in the
// store for a button to confirm.
func presentProposal(p agentloop.Proposal, requesterTag string,
	upload func(filename, title, content string) error, put func(agentloop.Proposal) string,
) ([]slack.Block, error) {
	if proposalNeedsSnippet(p) {
		content := agentloop.UnifiedDiff(p.Path, p.OldContent, p.NewContent)
		if hasPRBody(p) {
			content += prBodySeparator + p.PRBody
		}
		err := upload(p.Env+"-"+p.Service+".diff", "Proposed change to "+p.Path, content)
		if err != nil {
			return nil, fmt.Errorf("the proposed diff could not be shown (uploading it failed: %v), so there is nothing to confirm. "+
				"Re-run /sre drift fix %s", err, p.Service)
		}
	}
	p.ID = put(p)
	return fixProposalBlocks(p, requesterTag), nil
}

// Model text is capped before it is rendered, so one long answer cannot push a block
// past Slack's limits and lose the buttons after it.
const (
	maxTitleRunes     = 200
	maxProseRunes     = 1500 // summary, why, rationale, decline reason
	maxRootCauseRunes = 2000
	maxEnvRunes       = 40
)

// maxInlineDiff is the budget for a fenced diff inside one section, kept under slackio's
// 2900-byte cut for the same reason as maxTableBytes: a cut inside the fence would show
// half a diff as if it were the whole change.
const maxInlineDiff = 2800

// slackText caps model-written text and escapes it for mrkdwn. The model reads JIRA
// tickets and Argo CD messages written by anyone, so its output is untrusted: unescaped,
// it could ping the channel, mention someone or render a link. Capping first means an
// entity is never cut in half.
func slackText(s string, maxRunes int) string {
	if r := []rune(s); len(r) > maxRunes {
		s = string(r[:maxRunes]) + "…"
	}
	return slackEscape(s)
}

// slackEscape is Slack's own escaping: the three characters that start control
// sequences. Everything else, including mrkdwn emphasis, passes through.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// debugBlocks renders the debug agent's answer. The facts come first and are shown as
// fetched, so a reader can check the model's root cause against its inputs.
func debugBlocks(res *agentloop.DebugResult, rows []drift.Row, dashboardURL string) []slack.Block {
	var svcRows []drift.Row
	for _, r := range rows {
		if strings.EqualFold(r.Service, res.Service) {
			svcRows = append(svcRows, r)
		}
	}
	facts := differenceLines(svcRows)
	for _, e := range res.Evidence {
		facts += e + "\n"
	}

	blocks := []slack.Block{slackio.Section(fmt.Sprintf(":mag: *Triage: `%s`*", res.Service))}
	// Facts carry Argo CD messages and JIRA summaries, which are no more trusted than the
	// model's text.
	if facts != "" {
		blocks = append(blocks, slackio.Section("*Facts*\n"+slackEscape(facts)))
	}
	blocks = append(blocks, slackio.Section(":brain: *Root cause*\n"+slackText(res.RootCause, maxRootCauseRunes)))

	if f := res.Fix; f != nil {
		blocks = append(blocks,
			slackio.Section(fmt.Sprintf(":wrench: *Suggested fix* (%s)\n%s\n_%s_",
				slackText(f.Env, maxEnvRunes), slackText(f.Summary, maxProseRunes), slackText(f.Why, maxProseRunes))),
			actionBar(res.Service, f.Env, dashboardURL))
	} else {
		blocks = append(blocks, slackio.Context("No fix suggested."))
	}
	return blocks
}

// inlineDiff is the proposal's diff escaped for a code fence, and whether it can be shown
// there. The diff is model-written content, so it is escaped like any other model text.
// It cannot be shown inline when it is over one section's budget, or when it contains a
// fence of its own, which would end the code block early and show a garbled change as
// if it were the whole one.
func inlineDiff(p agentloop.Proposal) (string, bool) {
	diff := slackEscape(agentloop.UnifiedDiff(p.Path, p.OldContent, p.NewContent))
	return diff, len(diff) <= maxInlineDiff && !strings.Contains(diff, "```")
}

// diffNeedsSnippet reports whether the diff has to be uploaded as a snippet.
func diffNeedsSnippet(p agentloop.Proposal) bool {
	_, ok := inlineDiff(p)
	return !ok
}

// maxInlineBody is the PR body section's budget, under slackio's 2900-byte cut for the
// same reason as maxInlineDiff: a cut would show part of the body as if it were all.
const maxInlineBody = 2800

// prBodySeparator follows the diff in the snippet when the PR body goes there too.
const prBodySeparator = "\n--- PR description ---\n"

func hasPRBody(p agentloop.Proposal) bool { return strings.TrimSpace(p.PRBody) != "" }

// inlinePRBody is the PR body section, escaped like any other model text, and whether
// it fits one section. proposeFix has already capped the body's size.
func inlinePRBody(p agentloop.Proposal) (string, bool) {
	s := "*PR description*\n" + slackEscape(p.PRBody)
	return s, len(s) <= maxInlineBody
}

// proposalNeedsSnippet reports whether a snippet must be uploaded. The PR body rides
// with the diff: when either one cannot be shown inline, both go in the snippet.
func proposalNeedsSnippet(p agentloop.Proposal) bool {
	if diffNeedsSnippet(p) {
		return true
	}
	_, ok := inlinePRBody(p)
	return hasPRBody(p) && !ok
}

// fixProposalBlocks shows the exact change and the buttons that decide it. The buttons
// carry only the proposal ID; drift apply checks who clicked.
func fixProposalBlocks(p agentloop.Proposal, requesterTag string) []slack.Block {
	blocks := []slack.Block{slackio.Section(fmt.Sprintf(":memo: *Proposed change* for `%s` in `%s`\n*%s*",
		p.Service, p.Env, slackText(p.Title, maxTitleRunes)))}

	if diff, ok := inlineDiff(p); ok {
		blocks = append(blocks, slackio.Section("```\n"+diff+"```"))
	} else {
		blocks = append(blocks, slackio.Context(fmt.Sprintf(
			"The diff of `%s` cannot be shown in a message, it is attached in the thread.", p.Path)))
	}
	if strings.TrimSpace(p.Rationale) != "" {
		blocks = append(blocks, slackio.Section("_"+slackText(p.Rationale, maxProseRunes)+"_"))
	}
	if hasPRBody(p) {
		if proposalNeedsSnippet(p) {
			blocks = append(blocks, slackio.Context(
				"The PR description is attached in the thread, after the diff."))
		} else {
			body, _ := inlinePRBody(p)
			blocks = append(blocks, slackio.Section(body))
		}
	}

	apply := slack.NewButtonBlockElement("drift_apply", p.ID,
		slack.NewTextBlockObject(slack.PlainTextType, "Open this PR", true, false))
	apply.Style = slack.StylePrimary
	cancel := slack.NewButtonBlockElement("drift_cancel", p.ID,
		slack.NewTextBlockObject(slack.PlainTextType, "Cancel", true, false))

	return append(blocks,
		slack.NewActionBlock("fix_actions", apply, cancel),
		slackio.Context(fmt.Sprintf("requested by %s | only they can confirm | the agent does not merge", requesterTag)),
	)
}

// fixDeclinedBlocks is the answer when the model decided an overlay edit would not help.
// The facts are repeated so the reader can judge that decision without another command.
func fixDeclinedBlocks(service, reason string, rows []drift.Row) []slack.Block {
	blocks := []slack.Block{slackio.Section(fmt.Sprintf(":information_source: *No PR proposed* for `%s`\n%s",
		service, slackText(reason, maxProseRunes)))}
	if lines := differenceLines(rows); lines != "" {
		blocks = append(blocks, slackio.Section("*Facts*\n"+slackEscape(lines)))
	}
	return blocks
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

func plural2(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
