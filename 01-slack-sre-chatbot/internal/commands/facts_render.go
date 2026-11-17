package commands

import (
	"fmt"
	"strings"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
)

// The static commands (`drift list`, `service list`, `service get`) only restate what
// the rows say. They never name a kind of drift or rank one above another: that is a
// judgement, and judgements live in the agent's instructions, not in Go. Anything that
// looks like a verdict here (a severity emoji, a "promotion-gap" label) would put that
// meaning back.

// differencesTable renders one line per row with the values side by side. NOTE is last
// so a long Argo CD message pushes nothing else out of alignment.
func differencesTable(rows []drift.Row) string {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{
			r.Env,
			r.Service,
			dash(r.GitVersion),
			dash(r.RunningVersion),
			dash(r.SyncStatus),
			dash(r.Health),
			r.Note(),
		})
	}
	return table([]string{"ENV", "SERVICE", "GIT", "RUNNING", "SYNC", "HEALTH", "NOTE"}, out)
}

// maxTableBytes is the budget for the fenced table inside one Slack section. Section
// text is cut at 2900 bytes by slackio, and a cut inside a code fence drops the closing
// fence and every row after it while the header still claims the full count. Staying
// well under leaves room for the heading and the "more not shown" line.
const maxTableBytes = 2800

// differencesView is differencesTable limited to the rows that fit the section budget.
// It renders whole rows only, closes the fence, and says how many were left out, so the
// reader is never shown a count that disagrees with the table.
func differencesView(rows []drift.Row) string {
	shown := len(rows)
	out := differencesTable(rows)
	// Always show at least one row: a table with none would hide the problem entirely.
	for len(out) > maxTableBytes && shown > 1 {
		shown--
		out = differencesTable(rows[:shown])
	}
	if shown < len(rows) {
		out += fmt.Sprintf("\n_%d more not shown, narrow with /sre drift list <env>_", len(rows)-shown)
	}
	return out
}

// serviceStatus summarises one service for the inventory. rows must already be limited
// to that service. It lists environments in row order, which is production first.
func serviceStatus(rows []drift.Row) string {
	var envs []string
	for _, r := range rows {
		if r.Disagrees() {
			envs = append(envs, r.Env)
		}
	}
	if len(envs) == 0 {
		return "in sync"
	}
	return "differs: " + strings.Join(envs, ",")
}

// differenceLines is the detail-view form: one bullet per disagreeing row, agreeing
// rows omitted.
func differenceLines(rows []drift.Row) string {
	var b strings.Builder
	for _, r := range rows {
		if r.Disagrees() {
			b.WriteString("• " + r.Env + ": " + r.Note() + "\n")
		}
	}
	return b.String()
}
