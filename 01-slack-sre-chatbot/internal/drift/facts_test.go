package drift

import (
	"strings"
	"testing"
)

var envs = []string{"test", "stage", "prod"}

func desired(env, svc, ver string) Desired {
	return Desired{Service: svc, Env: env, Version: ver, Path: "gitops/overlays/" + env + "/kustomization.yaml"}
}

func actual(env, svc, ver string) Actual {
	return Actual{Service: svc, Env: env, Version: ver, App: svc + "-" + env, SyncStatus: "Synced", Health: "Healthy"}
}

// The standard demo: stage was promoted, prod was not, and the cluster matches git.
func TestBuildStandardDemo(t *testing.T) {
	d := []Desired{
		desired("test", "product-catalog", "v1.2.0"),
		desired("stage", "product-catalog", "v1.2.0"),
		desired("prod", "product-catalog", "v1.1.0"),
	}
	a := []Actual{
		actual("test", "product-catalog", "v1.2.0"),
		actual("stage", "product-catalog", "v1.2.0"),
		actual("prod", "product-catalog", "v1.1.0"),
	}

	got := Build(d, a, envs).Disagreeing()
	if len(got) != 1 {
		t.Fatalf("want 1 disagreeing row, got %d: %+v", len(got), got)
	}
	r := got[0]
	if r.Env != "prod" {
		t.Errorf("Env = %q, want prod", r.Env)
	}
	if r.Flags != (Flags{DiffersFromPrevEnv: true}) {
		t.Errorf("Flags = %+v, want only DiffersFromPrevEnv", r.Flags)
	}
	if r.PrevEnv != "stage" || r.PrevGitVersion != "v1.2.0" {
		t.Errorf("prev = %s %s, want stage v1.2.0", r.PrevEnv, r.PrevGitVersion)
	}
	if want := "git v1.1.0, stage git v1.2.0"; r.Note() != want {
		t.Errorf("Note() = %q, want %q", r.Note(), want)
	}
}

func TestBuildVersionMismatchWithSyncMessage(t *testing.T) {
	d := []Desired{desired("stage", "api", "v1.3.0"), desired("prod", "api", "v1.3.0")}
	prod := actual("prod", "api", "v1.2.0")
	prod.SyncStatus = "OutOfSync"
	prod.LastMessage = "ImagePullBackOff"
	a := []Actual{actual("stage", "api", "v1.3.0"), prod}

	got := Build(d, a, envs).Disagreeing()
	if len(got) != 1 {
		t.Fatalf("want 1 disagreeing row, got %d: %+v", len(got), got)
	}
	r := got[0]
	if r.Flags != (Flags{VersionMismatch: true, NotSynced: true}) {
		t.Errorf("Flags = %+v, want VersionMismatch and NotSynced only", r.Flags)
	}
	if want := "git v1.3.0, running v1.2.0; sync OutOfSync: ImagePullBackOff"; r.Note() != want {
		t.Errorf("Note() = %q, want %q", r.Note(), want)
	}
}

func TestBuildOneSidedRows(t *testing.T) {
	d := []Desired{desired("stage", "only-git", "v1.0.0")}
	a := []Actual{actual("prod", "only-cluster", "v0.9.3")}

	tbl := Build(d, a, envs)

	git := tbl.ForService("only-git")
	if len(git) != 1 {
		t.Fatalf("want 1 row for only-git, got %d", len(git))
	}
	if git[0].Flags != (Flags{OnlyInGit: true}) {
		t.Errorf("Flags = %+v, want only OnlyInGit", git[0].Flags)
	}
	if want := "declared in git, not in the cluster"; git[0].Note() != want {
		t.Errorf("Note() = %q, want %q", git[0].Note(), want)
	}

	cl := tbl.ForService("only-cluster")
	if len(cl) != 1 {
		t.Fatalf("want 1 row for only-cluster, got %d", len(cl))
	}
	if cl[0].Flags != (Flags{OnlyInCluster: true}) {
		t.Errorf("Flags = %+v, want only OnlyInCluster", cl[0].Flags)
	}
	if want := "running v0.9.3, not declared in git"; cl[0].Note() != want {
		t.Errorf("Note() = %q, want %q", cl[0].Note(), want)
	}
}

func TestBuildUnhealthy(t *testing.T) {
	d := []Desired{desired("prod", "api", "v1.0.0")}
	a := actual("prod", "api", "v1.0.0")
	a.Health = "Degraded"

	got := Build(d, []Actual{a}, envs).Disagreeing()
	if len(got) != 1 {
		t.Fatalf("want 1 disagreeing row, got %d: %+v", len(got), got)
	}
	if got[0].Flags != (Flags{NotHealthy: true}) {
		t.Errorf("Flags = %+v, want only NotHealthy", got[0].Flags)
	}
	if want := "health Degraded"; got[0].Note() != want {
		t.Errorf("Note() = %q, want %q", got[0].Note(), want)
	}
}

func TestVPrefixIsNotADifference(t *testing.T) {
	d := []Desired{desired("prod", "api", "1.2.0")}
	a := []Actual{actual("prod", "api", "v1.2.0")}

	for _, r := range Build(d, a, envs).Rows {
		if r.Disagrees() {
			t.Errorf("row %s/%s disagrees on a v-prefix alone: %+v", r.Env, r.Service, r.Flags)
		}
	}
}

func TestRowsSortProdFirstThenService(t *testing.T) {
	var d []Desired
	for _, env := range envs {
		for _, svc := range []string{"b", "a"} {
			d = append(d, desired(env, svc, "v1.0.0"))
		}
	}

	var got []string
	for _, r := range Build(d, nil, envs).Rows {
		got = append(got, r.Env+"/"+r.Service)
	}
	want := []string{"prod/a", "prod/b", "stage/a", "stage/b", "test/a", "test/b"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestNoteTruncatesLongSyncMessage(t *testing.T) {
	d := []Desired{desired("prod", "api", "v1.0.0")}
	a := actual("prod", "api", "v1.0.0")
	a.SyncStatus = "OutOfSync"
	a.LastMessage = strings.Repeat("é", 1000)

	got := Build(d, []Actual{a}, envs).Rows[0].Note()
	if n := len([]rune(got)); n > 300 {
		t.Errorf("Note() has %d runes, want <= 300", n)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("Note() = %q, want a trailing ...", got)
	}
}
