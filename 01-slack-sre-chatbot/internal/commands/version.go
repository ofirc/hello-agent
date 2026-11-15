package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/slackio"
)

// runVersionGet answers "what is actually running right now".
//
// This is the command people type most, and the one with the tightest latency budget
// (p50 under two seconds). It makes exactly two concurrent API calls and no model call.
func runVersionGet(ctx context.Context, d *Deps, in *Invocation) error {
	envs := d.Cfg.Environments
	if e := in.Arg(0); e != "" {
		if !d.Cfg.IsEnv(e) {
			return &ErrUsage{Msg: fmt.Sprintf("`%s` is not an environment. Known: %s",
				e, strings.Join(d.Cfg.Environments, ", "))}
		}
		envs = []string{e}
	}

	in.Rep.Step(ctx, "• reading git and Argo CD...")
	snap, err := gather(ctx, d)
	if err != nil {
		return err
	}

	prod := prodEnv(d.Cfg.Environments)
	headers := append([]string{"SERVICE"}, upper(envs)...)
	headers = append(headers, "GIT", "STATUS")

	var rows [][]string
	for _, svc := range snap.services() {
		row := []string{svc}
		var gitVersion string
		status := "in sync"

		for _, env := range envs {
			a, ok := snap.actualFor(env, svc)
			if !ok {
				row = append(row, "-")
				continue
			}
			row = append(row, dash(a.Version))
		}
		for _, env := range envs {
			if ds, ok := snap.desiredFor(env, svc); ok {
				gitVersion = ds.Version
				if a, ok := snap.actualFor(env, svc); ok && a.Version != "" && a.Version != ds.Version {
					status = "DRIFT in " + env
					if env == prod {
						break // prod drift outranks anything found earlier
					}
				}
			}
		}
		row = append(row, dash(gitVersion), status)
		rows = append(rows, row)
	}

	if len(rows) == 0 {
		in.Rep.Done(ctx, []slack.Block{
			slackio.Section(":grey_question: No services found. Check that the Argo CD applications carry " +
				"`app.kubernetes.io/name` and `app.kubernetes.io/part-of` labels, or are named `<service>-<env>`."),
		}, "no services found")
		return nil
	}

	title := fmt.Sprintf("*Deployed versions* - %s", strings.Join(envs, ", "))
	in.Rep.Done(ctx, []slack.Block{
		slackio.Section(title + "\n" + table(headers, rows)),
		slackio.Context(fmt.Sprintf("git = `%s` on `%s` | %d services | asked by %s",
			d.GH.Slug(), d.Cfg.GitHubBase, len(rows), in.UserTag)),
	}, "deployed versions")
	return nil
}

// runVersionDiff compares exactly two environments, which is the shape of the question
// people actually ask during a release: "what is in stage that is not in prod yet?"
func runVersionDiff(ctx context.Context, d *Deps, in *Invocation) error {
	a, b := in.Arg(0), in.Arg(1)
	if a == "" || b == "" {
		return &ErrUsage{Msg: "give me two environments, for example `/sre version diff stage prod`"}
	}
	if !d.Cfg.IsEnv(a) || !d.Cfg.IsEnv(b) {
		return &ErrUsage{Msg: fmt.Sprintf("unknown environment. Known: %s",
			strings.Join(d.Cfg.Environments, ", "))}
	}

	in.Rep.Step(ctx, "• comparing %s against %s...", a, b)
	snap, err := gather(ctx, d)
	if err != nil {
		return err
	}

	var rows [][]string
	same := 0
	for _, svc := range snap.services() {
		av, _ := snap.actualFor(a, svc)
		bv, _ := snap.actualFor(b, svc)
		if av.Version == bv.Version {
			same++
			continue
		}
		rows = append(rows, []string{svc, dash(av.Version), dash(bv.Version)})
	}

	if len(rows) == 0 {
		in.Rep.Done(ctx, []slack.Block{
			slackio.Section(fmt.Sprintf(":white_check_mark: *%s* and *%s* run identical versions across all %d services.", a, b, same)),
		}, "environments identical")
		return nil
	}

	in.Rep.Done(ctx, []slack.Block{
		slackio.Section(fmt.Sprintf("*%s vs %s* - %d differ, %d match\n", a, b, len(rows), same) +
			table([]string{"SERVICE", strings.ToUpper(a), strings.ToUpper(b)}, rows)),
		slackio.Context("`/sre drift list` to see which of these disagree with git"),
	}, "version diff")
	return nil
}

// runRolloutStatus is the Argo CD view for one service: did the sync land, and is the
// workload healthy afterwards.
func runRolloutStatus(ctx context.Context, d *Deps, in *Invocation) error {
	svc := in.Arg(0)
	if svc == "" {
		return &ErrUsage{Msg: "which service? for example `/sre rollout status product-catalog`"}
	}
	envs := d.Cfg.Environments
	if e := in.Arg(1); e != "" {
		if !d.Cfg.IsEnv(e) {
			return &ErrUsage{Msg: fmt.Sprintf("`%s` is not an environment", e)}
		}
		envs = []string{e}
	}

	in.Rep.Step(ctx, "• querying Argo CD...")
	actual, err := d.Argo.Applications(ctx)
	if err != nil {
		return err
	}

	var rows [][]string
	var notes []string
	for _, env := range envs {
		for _, a := range actual {
			if a.Service != svc || a.Env != env {
				continue
			}
			age := "-"
			if !a.LastSyncAt.IsZero() {
				age = humanAge(time.Since(a.LastSyncAt))
			}
			rows = append(rows, []string{env, dash(a.Version), dash(a.SyncStatus), dash(a.Health), age})
			if a.LastMessage != "" {
				notes = append(notes, fmt.Sprintf("*%s*: %s", env, a.LastMessage))
			}
		}
	}

	if len(rows) == 0 {
		return &ErrUsage{Msg: fmt.Sprintf("Argo CD has no application for `%s`. Try `/sre version get` to see what it does know about.", svc)}
	}

	body := fmt.Sprintf("*Rollout status* - `%s`\n", svc) +
		table([]string{"ENV", "VERSION", "SYNC", "HEALTH", "LAST SYNC"}, rows)
	blocks := []slack.Block{slackio.Section(body)}
	if len(notes) > 0 {
		blocks = append(blocks, slackio.Section(":warning: "+strings.Join(notes, "\n")))
	}
	if url := d.Grafana.DashboardURL(svc, prodEnv(envs)); url != "" {
		blocks = append(blocks, slackio.Context("<"+url+"|Grafana dashboard>"))
	}

	in.Rep.Done(ctx, blocks, "rollout status for "+svc)
	return nil
}

func upper(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.ToUpper(s)
	}
	return out
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
