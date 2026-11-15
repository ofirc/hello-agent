package commands

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/slack-go/slack"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/slackio"
)

func asUsage(err error, target **ErrUsage) bool { return errors.As(err, target) }

// helpText is built from the spec table, so a command cannot be added without showing
// up in help. Hand-maintained help is help that goes stale.
func (r *Router) helpText() string {
	byNoun := map[string][]spec{}
	for _, s := range r.specs {
		byNoun[s.Noun] = append(byNoun[s.Noun], s)
	}
	nouns := make([]string, 0, len(byNoun))
	for n := range byNoun {
		nouns = append(nouns, n)
	}
	sort.Strings(nouns)

	var b strings.Builder
	b.WriteString("*SRE Agent* - deployment questions, release drift, and fix PRs.\n")
	b.WriteString("Commands are `noun verb`, like gcloud.\n")

	for _, n := range nouns {
		group := byNoun[n]
		sort.Slice(group, func(i, j int) bool { return group[i].Verb < group[j].Verb })
		b.WriteString("\n")
		for _, s := range group {
			line := "`" + s.usage() + "`"
			b.WriteString(fmt.Sprintf("%s\n    %s\n", line, s.Summary))
		}
	}
	return b.String()
}

func runHelp(ctx context.Context, d *Deps, in *Invocation) error {
	r := routerFrom(ctx)
	if r == nil {
		return fmt.Errorf("internal: router missing from context")
	}

	legacy := make([]string, 0, len(r.aliases))
	for old, canon := range r.aliases {
		if strings.Contains(canon, " ") {
			legacy = append(legacy, fmt.Sprintf("`%s` -> `%s`", old, canon))
		}
	}
	sort.Strings(legacy)

	blocks := []slack.Block{
		slackio.Section(r.helpText()),
		slackio.Context("Still accepted: " + strings.Join(legacy, ", ")),
	}
	in.Rep.Done(ctx, blocks, "SRE Agent help")
	return nil
}

func runDoctor(ctx context.Context, d *Deps, in *Invocation) error {
	enabled := d.Cfg.Enabled()

	rows := []struct {
		name, why string
	}{
		{"github", "desired state and fix PRs"},
		{"argocd", "actual cluster state"},
		{"grafana", "is the drift hurting anyone"},
		{"jira", "has anyone already reported it"},
		{"vertex", "root-cause narration (optional, falls back to a deterministic summary)"},
	}

	var b strings.Builder
	b.WriteString("*Integrations*\n")
	for _, row := range rows {
		mark := ":x:"
		if enabled[row.name] {
			mark = ":white_check_mark:"
		}
		b.WriteString(fmt.Sprintf("%s *%s* - %s\n", mark, row.name, row.why))
	}

	b.WriteString(fmt.Sprintf("\n*Environments* %s", strings.Join(d.Cfg.Environments, ", ")))
	b.WriteString(fmt.Sprintf("\n*Fixable environments* %s", strings.Join(d.Cfg.AllowFixEnvs, ", ")))
	b.WriteString(fmt.Sprintf("\n*GitOps overlays* `%s/<env>/kustomization.yaml` in `%s`",
		d.Cfg.GitOpsPrefix, d.GH.Slug()))
	if d.Cfg.RequireApprove {
		b.WriteString("\n*Remediation* opens a PR, never merges")
	} else {
		b.WriteString("\n:warning: *Remediation* auto-merge is enabled")
	}

	in.Rep.Done(ctx, []slack.Block{slackio.Section(b.String())}, "integration status")
	return nil
}
