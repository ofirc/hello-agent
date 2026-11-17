package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/slack-go/slack"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/slackio"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/skills"
)

func asUsage(err error, target **ErrUsage) bool { return errors.As(err, target) }

// helpText is built from the spec table, so a command cannot be added without showing
// up in help. Hand-maintained help is help that goes stale.
func (r *Router) helpText() string {
	byNoun := map[string][]spec{}
	for _, s := range r.specs {
		if s.Hidden {
			continue
		}
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
			if s.Restricted {
				line += " :lock: _restricted_"
			}
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
	modelOK, modelLabel, modelWhy := modelRow(d.Cfg.ModelProvider, d.Cfg.ModelName(), enabled["model"], d.Agent != nil)
	enabled["model"] = modelOK
	enabled["sandbox"] = d.Agent != nil && d.Agent.SandboxAvailable()
	sandboxName := ""
	if d.Agent != nil {
		sandboxName = d.Agent.SandboxName()
	}

	// The skills are embedded, so they are always there; listing them says which drift
	// knowledge this build carries.
	skillNames := "unreadable"
	if entries, err := fs.ReadDir(skills.FS, "."); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		skillNames = strings.Join(names, ", ")
		enabled["skills"] = true
	}

	// key indexes enabled; label is what the user reads, which can carry detail.
	rows := []struct {
		key, label, why string
	}{
		{"github", "github", "desired state and fix PRs"},
		{"argocd", "argocd", "actual cluster state"},
		{"grafana", "grafana", "is the drift hurting anyone"},
		{"jira", "jira", "has anyone already reported it"},
		{"model", modelLabel, modelWhy},
		{"sandbox", sandboxLabel(sandboxName), "code execution for the agent"},
		{"skills", "skills", skillNames},
	}

	var b strings.Builder
	b.WriteString("*Integrations*\n")
	for _, row := range rows {
		mark := ":x:"
		if enabled[row.key] {
			mark = ":white_check_mark:"
		}
		b.WriteString(fmt.Sprintf("%s *%s* - %s\n", mark, row.label, row.why))
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

// sandboxLabel is doctor's sandbox line. The shim gets its caveat in the label itself:
// it says the row is green while model code runs with no isolation at all.
func sandboxLabel(name string) string {
	switch name {
	case "":
		return "sandbox"
	case "shim":
		return "sandbox (shim, no isolation)"
	default:
		return "sandbox (" + name + ")"
	}
}

// modelRow is doctor's model line. Configured is not the same as working: the agent can
// fail to start with a provider set (Vertex credentials, Ollama not reachable), and drift
// debug and drift fix then send the user here, so this row has to tell the two apart.
func modelRow(provider, model string, configured, started bool) (ok bool, label, why string) {
	label = "model"
	if provider != "" {
		label = "model (" + provider + ", " + model + ")"
	}
	switch {
	case started:
		return true, label, "required for drift debug and drift fix"
	case configured:
		return false, label, "configured but failed to start, see the logs"
	default:
		return false, label, "required for drift debug and drift fix"
	}
}
