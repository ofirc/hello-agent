package agentloop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
)

// maxEvidenceRunes keeps one evidence line small enough to sit in a prompt or a Slack
// bullet. Argo CD sync messages can run to kilobytes.
const maxEvidenceRunes = 300

// maxCodeRuns bounds how much sandbox time one run can spend. It is a count, not a
// timeout, because the model decides how often to retry.
const maxCodeRuns = 5

// maxThreadTrail is under Slack's 3000-character limit for one message, leaving room for
// the heading.
const maxThreadTrail = 2900

// closeTimeout bounds posting the trail after the run's own context is gone.
const closeTimeout = 10 * time.Second

// runState is one agent run: a scratch directory the sandbox can read, and what the run
// learned. Tools may be called concurrently, so everything mutable sits behind mu.
type runState struct {
	a        *Agent
	dir      string
	progress Progress

	mu       sync.Mutex
	evidence []string
	trail    []string
	overlays map[string]*Overlay // by env, what get_overlay returned; the fix agent edits from these
	codeRuns int
	closed   bool

	// The run's outcome. finished is set by whichever finish succeeds first: propose_fix
	// or decline_fix for the fix agent, submit_triage for the debug agent. Every later
	// finish is refused.
	finished bool
	proposal *Proposal
	declined string
	triage   *DebugResult // without Evidence, which is taken when the run ends

	// facts is the table fetched on first use. Building it is a GitHub and Argo CD round
	// trip, and every service-taking tool needs it to validate the service. factsMu is
	// held across the fetch so concurrent tool calls wait for one fetch, not start their
	// own.
	factsMu sync.Mutex
	facts   *drift.Table
}

// newRun creates the scratch directory. Its name comes from MkdirTemp so it never
// contains a comma (the sandbox mount spec breaks on one).
func (a *Agent) newRun(progress Progress) (*runState, error) {
	if progress == nil {
		progress = nopProgress{}
	}
	if err := os.MkdirAll(a.cfg.WorkDir, 0o755); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(a.cfg.WorkDir, "run-*")
	if err != nil {
		return nil, err
	}
	return &runState{a: a, dir: dir, progress: progress, overlays: map[string]*Overlay{}}, nil
}

// close removes the scratch directory and posts the tool trail, so a reader can see how
// the answer was reached. A long trail goes up as a file instead of a message.
func (s *runState) close(ctx context.Context) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	trail := strings.Join(s.trail, "\n")
	s.mu.Unlock()

	_ = os.RemoveAll(s.dir)
	if trail == "" {
		return
	}
	if len([]rune(trail)) > maxThreadTrail {
		s.progress.Snippet(ctx, "trail.txt", "Tool trail", trail)
		return
	}
	s.progress.Thread(ctx, "Tool trail:\n"+trail)
}

// closeDetached is close for a defer: the trail is posted even when the run's context
// was cancelled or timed out, which is when a reader most needs it. The post gets its
// own short deadline so a stuck Slack call cannot hold the command forever.
func (s *runState) closeDetached(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()
	s.close(ctx)
}

// factsTable returns the cached facts, fetching them on first use. A failed fetch is not
// cached, so the next tool call tries again.
func (s *runState) factsTable(ctx context.Context) (drift.Table, error) {
	s.factsMu.Lock()
	defer s.factsMu.Unlock()
	if s.facts != nil {
		return *s.facts, nil
	}
	table, err := s.a.cfg.Providers.Facts(ctx)
	if err != nil {
		return table, err
	}
	s.facts = &table
	return table, nil
}

// forgetFacts drops the cached table after something may have changed what Argo CD
// reports.
func (s *runState) forgetFacts() {
	s.factsMu.Lock()
	s.facts = nil
	s.factsMu.Unlock()
}

// trailAdd records a tool call.
func (s *runState) trailAdd(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trail = append(s.trail, truncate(line, maxEvidenceRunes))
}

// evidenceAdd records what a tool found. Repeated calls often return the same facts, so
// a line already present is not added twice.
func (s *runState) evidenceAdd(line string) {
	line = truncate(line, maxEvidenceRunes)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.evidence {
		if e == line {
			return
		}
	}
	s.evidence = append(s.evidence, line)
}

// writeEvidence puts a tool's full output in the run directory, where run_code can read
// it. Failing to write is not fatal to the run: the model still has the tool result.
func (s *runState) writeEvidence(name string, content []byte) {
	_ = os.WriteFile(filepath.Join(s.dir, name), content, 0o644)
}

// truncate cuts s to at most max runes, marking the cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-3]) + "..."
}

// fileSafe reduces a name from the model to [a-z0-9._-] so it cannot escape the run
// directory or break the sandbox mount spec.
func fileSafe(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

type nopProgress struct{}

func (nopProgress) Step(context.Context, string, ...any)            {}
func (nopProgress) Thread(context.Context, string)                  {}
func (nopProgress) Snippet(context.Context, string, string, string) {}
