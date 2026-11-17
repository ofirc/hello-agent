// Package drift holds the comparison at the heart of the agent: what git says should be
// running, against what Argo CD reports is actually running.
//
// This package is deliberately pure. It does no I/O, which is what makes it testable
// without standing up GKE. It also only states facts: what a disagreement means and
// which way a fix goes is the agent skills' business, not Go's.
package drift

import (
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

func key(env, svc string) string { return env + "/" + svc }

// sameVersion tolerates the v-prefix drifting in and out, which happens constantly
// between a chart value and an image tag.
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}
