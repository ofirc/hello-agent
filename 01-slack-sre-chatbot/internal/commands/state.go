package commands

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
)

// snapshot is the pair every command needs: what git declares, and what the cluster
// runs. Fetched together, concurrently, once per command.
type snapshot struct {
	Desired []drift.Desired
	Actual  []drift.Actual
}

func gather(ctx context.Context, d *Deps) (*snapshot, error) {
	var (
		wg         sync.WaitGroup
		desired    []drift.Desired
		actual     []drift.Actual
		dErr, aErr error
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		desired, dErr = d.GitOps.Desired(ctx, d.Cfg.Environments)
	}()
	go func() {
		defer wg.Done()
		actual, aErr = d.Argo.Applications(ctx)
	}()
	wg.Wait()

	// Both sides are required. Reporting drift from half the picture would be worse
	// than reporting nothing, because it would look authoritative.
	if dErr != nil {
		return nil, fmt.Errorf("reading desired state from %s: %w", d.GH.Slug(), dErr)
	}
	if aErr != nil {
		return nil, fmt.Errorf("reading actual state from Argo CD: %w", aErr)
	}
	return &snapshot{Desired: desired, Actual: actual}, nil
}

func (s *snapshot) findings(envs []string) []drift.Finding {
	return drift.Compute(s.Desired, s.Actual, envs)
}

func (s *snapshot) services() []string {
	seen := map[string]bool{}
	for _, d := range s.Desired {
		seen[d.Service] = true
	}
	for _, a := range s.Actual {
		seen[a.Service] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *snapshot) desiredFor(env, service string) (drift.Desired, bool) {
	for _, d := range s.Desired {
		if d.Env == env && d.Service == service {
			return d, true
		}
	}
	return drift.Desired{}, false
}

func (s *snapshot) actualFor(env, service string) (drift.Actual, bool) {
	for _, a := range s.Actual {
		if a.Env == env && a.Service == service {
			return a, true
		}
	}
	return drift.Actual{}, false
}

// prodEnv is the last configured environment. ENVIRONMENTS is documented as ordered
// least to most production-like, which makes "the one that matters most" positional
// rather than another setting to keep in sync.
func prodEnv(envs []string) string {
	if len(envs) == 0 {
		return "prod"
	}
	return envs[len(envs)-1]
}

// table renders fixed-width rows inside a code fence. Slack has no table block, and
// monospace alignment reads far better than a bulleted list when you are scanning
// versions across three environments.
func table(headers []string, rows [][]string) string {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if i < len(widths) && len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}

	var b strings.Builder
	b.WriteString("```\n")
	for i, h := range headers {
		b.WriteString(pad(h, widths[i]))
		if i < len(headers)-1 {
			b.WriteString("  ")
		}
	}
	b.WriteString("\n")
	for _, r := range rows {
		for i, c := range r {
			if i >= len(widths) {
				break
			}
			b.WriteString(pad(c, widths[i]))
			if i < len(r)-1 {
				b.WriteString("  ")
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("```")
	return b.String()
}

func pad(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
