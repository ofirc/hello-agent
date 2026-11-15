// Package drift holds the comparison at the heart of the agent: what git says should be
// running, against what Argo CD reports is actually running.
//
// This package is deliberately pure. It does no I/O, which is what makes the interesting
// logic testable without standing up GKE.
package drift

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Desired is the state declared in the GitOps repo.
type Desired struct {
	Service string
	Env     string
	Version string
	Path    string // file the version was read from, so the fix knows what to edit
}

// Actual is the state Argo CD reports from the cluster.
type Actual struct {
	Service     string
	Env         string
	Version     string
	Image       string // image repository without the tag, for the service detail view
	App         string // Argo CD application name
	Namespace   string
	SyncStatus  string // Synced | OutOfSync | Unknown
	Health      string // Healthy | Progressing | Degraded | Missing
	LastSyncAt  time.Time
	LastMessage string // operationState message, usually the failure reason
}

// Kind classifies a finding so the renderer and the remediator can branch on it without
// re-parsing strings.
type Kind string

const (
	// KindVersionSkew: git and the cluster disagree on the version. The headline case.
	KindVersionSkew Kind = "version-skew"
	// KindSyncFailed: Argo tried and could not land the desired version.
	KindSyncFailed Kind = "sync-failed"
	// KindUnhealthy: right version, unhealthy workload.
	KindUnhealthy Kind = "unhealthy"
	// KindMissing: declared in git, absent from the cluster.
	KindMissing Kind = "missing"
	// KindUndeclared: running in the cluster, absent from git. Someone kubectl'd.
	KindUndeclared Kind = "undeclared"
	// KindPromotionGap: every environment matches its own overlay, but the overlays
	// disagree with each other. Nothing is broken, prod is simply behind stage because
	// nobody promoted. This is the common case and the one a PR genuinely fixes.
	KindPromotionGap Kind = "promotion-gap"
)

type Finding struct {
	Kind     Kind
	Service  string
	Env      string
	Desired  string
	Actual   string
	Path     string
	Detail   string
	Severity int // 3 high, 2 medium, 1 low. Drives ordering and the emoji.
}

func (f Finding) Emoji() string {
	switch f.Severity {
	case 3:
		return ":red_circle:"
	case 2:
		return ":large_orange_circle:"
	default:
		return ":large_blue_circle:"
	}
}

func (f Finding) String() string {
	switch f.Kind {
	case KindVersionSkew:
		return fmt.Sprintf("%s/%s runs %s, git declares %s", f.Env, f.Service, f.Actual, f.Desired)
	case KindSyncFailed:
		return fmt.Sprintf("%s/%s sync failed: %s", f.Env, f.Service, f.Detail)
	case KindUnhealthy:
		return fmt.Sprintf("%s/%s is %s", f.Env, f.Service, f.Detail)
	case KindMissing:
		return fmt.Sprintf("%s/%s declared in git but absent from the cluster", f.Env, f.Service)
	case KindUndeclared:
		return fmt.Sprintf("%s/%s runs %s but is not declared in git", f.Env, f.Service, f.Actual)
	case KindPromotionGap:
		return fmt.Sprintf("%s/%s is on %s, %s already has %s", f.Env, f.Service, f.Actual, f.Detail, f.Desired)
	}
	return fmt.Sprintf("%s/%s: %s", f.Env, f.Service, f.Detail)
}

// Compute diffs desired against actual and returns findings, most severe first.
//
// Severity rules, in plain terms:
//   - a failed sync outranks plain skew, because it tells you the rollout was attempted
//     and lost, rather than never started
//   - prod outranks every other environment
//   - a service present in git and missing from the cluster is always high
//
// envs must be ordered least to most production-like. The order is what lets the
// function tell a promotion gap (stage ahead of prod, both internally consistent) from
// a genuine deploy failure.
func Compute(desired []Desired, actual []Actual, envs []string) []Finding {
	prodEnv := ""
	if len(envs) > 0 {
		prodEnv = envs[len(envs)-1]
	}

	dIdx := make(map[string]Desired, len(desired))
	for _, d := range desired {
		dIdx[key(d.Env, d.Service)] = d
	}
	aIdx := make(map[string]Actual, len(actual))
	for _, a := range actual {
		aIdx[key(a.Env, a.Service)] = a
	}

	var out []Finding
	for k, d := range dIdx {
		a, ok := aIdx[k]
		if !ok {
			out = append(out, Finding{
				Kind: KindMissing, Service: d.Service, Env: d.Env,
				Desired: d.Version, Path: d.Path, Severity: 3,
			})
			continue
		}

		switch {
		case !sameVersion(d.Version, a.Version):
			sev := 2
			detail := ""
			kind := KindVersionSkew
			// Argo knows it is behind and failed to catch up. That is a different bug
			// from nobody having tried, and it is the one worth paging about.
			if a.SyncStatus == "OutOfSync" && a.LastMessage != "" {
				kind, sev, detail = KindSyncFailed, 3, a.LastMessage
			}
			if d.Env == prodEnv {
				sev = 3
			}
			out = append(out, Finding{
				Kind: kind, Service: d.Service, Env: d.Env,
				Desired: d.Version, Actual: a.Version, Path: d.Path,
				Detail: detail, Severity: sev,
			})

		case a.Health == "Degraded" || a.Health == "Missing":
			sev := 2
			if d.Env == prodEnv {
				sev = 3
			}
			out = append(out, Finding{
				Kind: KindUnhealthy, Service: d.Service, Env: d.Env,
				Desired: d.Version, Actual: a.Version, Path: d.Path,
				Detail: a.Health, Severity: sev,
			})
		}
	}

	for k, a := range aIdx {
		if _, ok := dIdx[k]; !ok {
			out = append(out, Finding{
				Kind: KindUndeclared, Service: a.Service, Env: a.Env,
				Actual: a.Version, Severity: 2,
			})
		}
	}

	// Promotion gaps: the overlays disagree with each other even though each
	// environment matches its own overlay. Only reported where there is no louder
	// finding already, since "prod failed to deploy" subsumes "prod is behind".
	noisy := map[string]bool{}
	for _, f := range out {
		noisy[key(f.Env, f.Service)] = true
	}
	for i := 0; i+1 < len(envs); i++ {
		lead, lag := envs[i], envs[i+1]
		for _, d := range desired {
			if d.Env != lead {
				continue
			}
			lagD, ok := dIdx[key(lag, d.Service)]
			if !ok || sameVersion(d.Version, lagD.Version) {
				continue
			}
			if noisy[key(lag, d.Service)] {
				continue
			}
			sev := 1
			if lag == prodEnv {
				sev = 2
			}
			out = append(out, Finding{
				Kind: KindPromotionGap, Service: d.Service, Env: lag,
				Desired: d.Version, Actual: lagD.Version, Path: lagD.Path,
				Detail: lead, Severity: sev,
			})
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity > out[j].Severity
		}
		if out[i].Env != out[j].Env {
			return out[i].Env > out[j].Env // prod sorts before stage before test
		}
		return out[i].Service < out[j].Service
	})
	return out
}

// ForService narrows findings to one service, which is what drift fix operates on.
func ForService(f []Finding, service string) []Finding {
	var out []Finding
	for _, x := range f {
		if strings.EqualFold(x.Service, service) {
			out = append(out, x)
		}
	}
	return out
}

// FixPlan is the edit that would remediate a finding: one tag, in one file.
type FixPlan struct {
	Path      string
	From      string
	To        string
	Title     string
	Rationale string
}

// Plan returns the remediation for a finding, or nil when editing git cannot help.
//
// The direction of the edit depends on the finding, and getting it backwards is the
// easiest mistake in this whole program:
//
//   - A promotion gap means git itself encodes the lag. The lagging overlay moves
//     forward to the version the leading environment already runs. This is a promotion.
//
//   - A version skew or failed sync means git is ahead and the cluster could not get
//     there. Moving git forward again would change nothing, because it is already
//     forward. The useful edit is backwards: pin the overlay to the version actually
//     running, which stops Argo retrying a release that does not work and makes git
//     tell the truth again. Rolling forward is a human decision that needs the broken
//     image fixed first.
//
//   - Unhealthy, missing and undeclared workloads are not version problems at all. No
//     plan.
func (f Finding) Plan() *FixPlan {
	if f.Path == "" || f.Desired == "" || f.Actual == "" {
		return nil
	}
	switch f.Kind {
	case KindPromotionGap:
		return &FixPlan{
			Path: f.Path, From: f.Actual, To: f.Desired,
			Title: fmt.Sprintf("promote(%s): %s %s -> %s", f.Env, f.Service, f.Actual, f.Desired),
			Rationale: fmt.Sprintf(
				"%s already runs %s and is healthy. This promotes the same version to %s.",
				f.Detail, f.Desired, f.Env),
		}
	case KindVersionSkew, KindSyncFailed:
		return &FixPlan{
			Path: f.Path, From: f.Desired, To: f.Actual,
			Title: fmt.Sprintf("revert(%s): pin %s to the running %s", f.Env, f.Service, f.Actual),
			Rationale: fmt.Sprintf(
				"git declares %s but %s has been running %s. Argo CD cannot land %s, so git is "+
					"pinned back to what is actually serving. Roll forward again once %s is known good.",
				f.Desired, f.Env, f.Actual, f.Desired, f.Desired),
		}
	}
	return nil
}

// Fixable reports whether a pull request can repair this finding.
func (f Finding) Fixable() bool { return f.Plan() != nil }

func key(env, svc string) string { return env + "/" + svc }

// sameVersion tolerates the v-prefix drifting in and out, which happens constantly
// between a chart value and an image tag.
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}
