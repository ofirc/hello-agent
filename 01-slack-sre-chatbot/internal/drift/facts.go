package drift

import (
	"sort"
	"strings"
	"time"
)

// Flags are comparisons between two values already in the row. They carry no judgement
// about severity or cause: deciding what a disagreement means is the caller's job.
type Flags struct {
	VersionMismatch    bool // in git and in the cluster, versions differ
	OnlyInGit          bool
	OnlyInCluster      bool
	NotSynced          bool // in the cluster, Argo CD sync status is not Synced
	NotHealthy         bool // in the cluster, Argo CD health is not Healthy
	DiffersFromPrevEnv bool // git version differs from the previous env's git version
}

// Row is everything known about one service in one environment, from both sides.
type Row struct {
	Env, Service                string
	GitVersion, RunningVersion  string
	Path, App, Namespace, Image string
	SyncStatus, Health          string
	LastSyncAt                  time.Time
	LastMessage                 string
	InGit, InCluster            bool

	// PrevEnv is the nearest earlier environment whose overlay declares this service. It
	// is empty for the first environment and when no earlier overlay declares it.
	PrevEnv, PrevGitVersion string

	Flags Flags
}

// Disagrees reports whether any flag is set.
func (r Row) Disagrees() bool { return r.Flags != Flags{} }

// maxNoteRunes keeps a row's note small enough to sit in a prompt or a Slack line. Argo
// CD sync messages can run to kilobytes of repeated resource errors.
const maxNoteRunes = 300

// Note is a one-line account of why the row disagrees, empty when it does not. It only
// restates the values behind the flags.
func (r Row) Note() string {
	var parts []string
	f := r.Flags
	if f.VersionMismatch {
		parts = append(parts, "git "+r.GitVersion+", running "+r.RunningVersion)
	}
	if f.OnlyInGit {
		parts = append(parts, "declared in git, not in the cluster")
	}
	if f.OnlyInCluster {
		parts = append(parts, "running "+r.RunningVersion+", not declared in git")
	}
	if f.NotSynced {
		s := "sync " + r.SyncStatus
		if r.LastMessage != "" {
			s += ": " + r.LastMessage
		}
		parts = append(parts, s)
	}
	if f.NotHealthy {
		parts = append(parts, "health "+r.Health)
	}
	if f.DiffersFromPrevEnv {
		parts = append(parts, "git "+r.GitVersion+", "+r.PrevEnv+" git "+r.PrevGitVersion)
	}

	note := strings.Join(parts, "; ")
	if runes := []rune(note); len(runes) > maxNoteRunes {
		note = string(runes[:maxNoteRunes-3]) + "..."
	}
	return note
}

// Table is the git-versus-cluster comparison for every environment and service.
type Table struct {
	Rows []Row
	// Envs is the order given to Build, least to most production-like.
	Envs []string
}

// Build joins desired and actual on env and service. envs must be ordered least to most
// production-like: that order defines which environment is "previous" to which, and puts
// production rows first.
//
// Rows sort by env index descending, then service ascending. Environments missing from
// envs sort after all listed ones.
func Build(desired []Desired, actual []Actual, envs []string) Table {
	envIdx := make(map[string]int, len(envs))
	for i, e := range envs {
		envIdx[e] = i
	}

	dIdx := make(map[string]Desired, len(desired))
	for _, d := range desired {
		dIdx[key(d.Env, d.Service)] = d
	}
	aIdx := make(map[string]Actual, len(actual))
	for _, a := range actual {
		aIdx[key(a.Env, a.Service)] = a
	}

	rows := make([]Row, 0, len(dIdx)+len(aIdx))
	for k, d := range dIdx {
		r := Row{
			Env: d.Env, Service: d.Service,
			GitVersion: d.Version, Path: d.Path, InGit: true,
		}
		if a, ok := aIdx[k]; ok {
			r.addActual(a)
		}
		rows = append(rows, r)
	}
	for k, a := range aIdx {
		if _, ok := dIdx[k]; ok {
			continue
		}
		r := Row{Env: a.Env, Service: a.Service}
		r.addActual(a)
		rows = append(rows, r)
	}

	for i := range rows {
		r := &rows[i]
		if idx, ok := envIdx[r.Env]; ok {
			for j := idx - 1; j >= 0; j-- {
				if d, ok := dIdx[key(envs[j], r.Service)]; ok {
					r.PrevEnv, r.PrevGitVersion = envs[j], d.Version
					break
				}
			}
		}
		r.Flags = Flags{
			VersionMismatch:    r.InGit && r.InCluster && !sameVersion(r.GitVersion, r.RunningVersion),
			OnlyInGit:          r.InGit && !r.InCluster,
			OnlyInCluster:      r.InCluster && !r.InGit,
			NotSynced:          r.InCluster && r.SyncStatus != "Synced",
			NotHealthy:         r.InCluster && r.Health != "Healthy",
			DiffersFromPrevEnv: r.InGit && r.PrevEnv != "" && !sameVersion(r.GitVersion, r.PrevGitVersion),
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		// Unlisted envs get -1 so they land last in a descending sort.
		ei, ej := indexOr(envIdx, rows[i].Env), indexOr(envIdx, rows[j].Env)
		if ei != ej {
			return ei > ej
		}
		if rows[i].Env != rows[j].Env {
			return rows[i].Env < rows[j].Env
		}
		return rows[i].Service < rows[j].Service
	})
	return Table{Rows: rows, Envs: envs}
}

func (r *Row) addActual(a Actual) {
	r.InCluster = true
	r.RunningVersion = a.Version
	r.App, r.Namespace, r.Image = a.App, a.Namespace, a.Image
	r.SyncStatus, r.Health = a.SyncStatus, a.Health
	r.LastSyncAt, r.LastMessage = a.LastSyncAt, a.LastMessage
}

func indexOr(m map[string]int, k string) int {
	if i, ok := m[k]; ok {
		return i
	}
	return -1
}

// Disagreeing returns the rows with at least one flag set, in table order.
func (t Table) Disagreeing() []Row {
	var out []Row
	for _, r := range t.Rows {
		if r.Disagrees() {
			out = append(out, r)
		}
	}
	return out
}

// ForService returns every row for a service, agreeing or not, matching case-insensitively.
func (t Table) ForService(service string) []Row {
	var out []Row
	for _, r := range t.Rows {
		if strings.EqualFold(r.Service, service) {
			out = append(out, r)
		}
	}
	return out
}

// DisagreeingFor is ForService limited to rows with at least one flag set.
func (t Table) DisagreeingFor(service string) []Row {
	var out []Row
	for _, r := range t.ForService(service) {
		if r.Disagrees() {
			out = append(out, r)
		}
	}
	return out
}

// Services lists each service name once, sorted.
func (t Table) Services() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range t.Rows {
		if !seen[r.Service] {
			seen[r.Service] = true
			out = append(out, r.Service)
		}
	}
	sort.Strings(out)
	return out
}
