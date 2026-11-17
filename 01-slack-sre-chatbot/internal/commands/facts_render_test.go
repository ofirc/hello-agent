package commands

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
)

// prodGapRow is prod pinned to v1.1.0 while stage already pins v1.2.0, with the cluster
// matching git. The only disagreement is against the previous environment.
func prodGapRow() drift.Row {
	return drift.Row{
		Env: "prod", Service: "product-catalog",
		GitVersion: "v1.1.0", RunningVersion: "v1.1.0",
		SyncStatus: "Synced", Health: "Healthy",
		InGit: true, InCluster: true,
		PrevEnv: "stage", PrevGitVersion: "v1.2.0",
		Flags: drift.Flags{DiffersFromPrevEnv: true},
	}
}

func TestDifferencesTableColumns(t *testing.T) {
	out := differencesTable([]drift.Row{prodGapRow()})

	lines := strings.Split(out, "\n")
	if len(lines) < 3 {
		t.Fatalf("want fence, header and a row, got %q", out)
	}
	header := lines[1]
	last := -1
	for _, col := range []string{"ENV", "SERVICE", "GIT", "RUNNING", "SYNC", "HEALTH", "NOTE"} {
		i := strings.Index(header, col)
		if i <= last {
			t.Fatalf("column %s missing or out of order in header %q", col, header)
		}
		last = i
	}

	row := lines[2]
	for _, want := range []string{"prod", "product-catalog", "v1.1.0", "stage git v1.2.0"} {
		if !strings.Contains(row, want) {
			t.Errorf("row %q missing %q", row, want)
		}
	}
}

func TestServiceStatus(t *testing.T) {
	if got := serviceStatus(nil); got != "in sync" {
		t.Errorf("no rows: got %q", got)
	}

	agree := prodGapRow()
	agree.Flags = drift.Flags{}
	if got := serviceStatus([]drift.Row{agree}); got != "in sync" {
		t.Errorf("agreeing row: got %q", got)
	}

	stage := prodGapRow()
	stage.Env = "stage"
	stage.Flags = drift.Flags{NotSynced: true}
	if got := serviceStatus([]drift.Row{prodGapRow(), stage, agree}); got != "differs: prod,stage" {
		t.Errorf("prod and stage disagree: got %q", got)
	}
}

func TestDifferenceLines(t *testing.T) {
	agree := prodGapRow()
	agree.Env = "dev"
	agree.Flags = drift.Flags{}

	got := differenceLines([]drift.Row{prodGapRow(), agree})
	want := "• prod: git v1.1.0, stage git v1.2.0\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStaticRenderHasNoKindLabels(t *testing.T) {
	row := prodGapRow()
	row.RunningVersion = "v1.0.0"
	row.Flags = drift.Flags{VersionMismatch: true, DiffersFromPrevEnv: true}
	rows := []drift.Row{row}

	out := differencesTable(rows) + serviceStatus(rows) + differenceLines(rows)
	for _, banned := range []string{"promotion-gap", "version-skew", "sync-failed", ":red_circle:", ":large_orange_circle:"} {
		if strings.Contains(out, banned) {
			t.Errorf("static output contains judgement label %q:\n%s", banned, out)
		}
	}
}

// longNoteRows returns n disagreeing rows whose note hits the 300-rune cap, the worst
// case an Argo CD sync message can produce.
func longNoteRows(n int) []drift.Row {
	rows := make([]drift.Row, n)
	for i := range rows {
		r := prodGapRow()
		r.Service = fmt.Sprintf("service-%02d", i)
		r.LastMessage = strings.Repeat("resource apps/Deployment is invalid; ", 20)
		r.SyncStatus = "OutOfSync"
		r.Flags = drift.Flags{NotSynced: true}
		rows[i] = r
	}
	return rows
}

func TestTableDoesNotPadLastColumn(t *testing.T) {
	out := differencesTable(longNoteRows(1))
	if len(strings.Split(out, "\n")) < 4 {
		t.Fatalf("want fence, header, row and fence, got %q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if line != strings.TrimRight(line, " ") {
			t.Errorf("trailing spaces on line %q", line)
		}
	}
}

func TestTableKeepsEarlierColumnsAligned(t *testing.T) {
	out := table([]string{"A", "LONGER", "LAST"}, [][]string{{"xx", "y", "z"}, {"x", "yyyy", "zzzzzz"}})
	want := "```\nA   LONGER  LAST\nxx  y       z\nx   yyyy    zzzzzz\n```"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestDifferencesViewStaysWithinSectionBudget(t *testing.T) {
	rows := longNoteRows(40)
	out := differencesView(rows)

	if len(out) >= 2900-len("*40 differences*\n") {
		t.Errorf("view is %d bytes, too close to the 2900 byte section cut", len(out))
	}
	lines := strings.Split(out, "\n")
	more := lines[len(lines)-1]
	if !strings.HasSuffix(more, " more not shown, narrow with /sre drift list <env>_") || !strings.HasPrefix(more, "_") {
		t.Fatalf("last line is not the more-not-shown line: %q", more)
	}
	if lines[len(lines)-2] != "```" {
		t.Errorf("fence is not closed before the more line: %q", lines[len(lines)-2])
	}
	if n := strings.Count(out, "```"); n != 2 {
		t.Errorf("want one opened and one closed fence, got %d markers", n)
	}

	// Rows shown plus rows reported missing must account for every row.
	shown := strings.Count(out, "service-")
	var missing int
	if _, err := fmt.Sscanf(strings.TrimPrefix(more, "_"), "%d more not shown", &missing); err != nil {
		t.Fatalf("parsing %q: %v", more, err)
	}
	if shown+missing != len(rows) || missing == 0 {
		t.Errorf("shown %d + missing %d != %d rows", shown, missing, len(rows))
	}
}

func TestDifferencesViewShowsEverythingWhenItFits(t *testing.T) {
	out := differencesView(longNoteRows(2))
	if strings.Contains(out, "more not shown") {
		t.Errorf("unexpected truncation line in %q", out)
	}
}
