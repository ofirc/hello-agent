package drift

import "testing"

var envs = []string{"test", "stage", "prod"}

func desired(env, svc, ver string) Desired {
	return Desired{Service: svc, Env: env, Version: ver, Path: "gitops/overlays/" + env + "/kustomization.yaml"}
}

func actual(env, svc, ver string) Actual {
	return Actual{Service: svc, Env: env, Version: ver, App: svc + "-" + env, SyncStatus: "Synced", Health: "Healthy"}
}

func TestNoDriftWhenEverythingAgrees(t *testing.T) {
	d := []Desired{desired("stage", "product-catalog", "v1.2.0"), desired("prod", "product-catalog", "v1.2.0")}
	a := []Actual{actual("stage", "product-catalog", "v1.2.0"), actual("prod", "product-catalog", "v1.2.0")}

	if got := Compute(d, a, envs); len(got) != 0 {
		t.Fatalf("expected no findings, got %d: %v", len(got), got)
	}
}

// The headline demo case: git is ahead, Argo could not land it.
func TestSyncFailureOutranksPlainSkew(t *testing.T) {
	d := []Desired{desired("prod", "product-catalog", "v1.2.0")}
	a := []Actual{{
		Service: "product-catalog", Env: "prod", Version: "v1.1.0", App: "product-catalog-prod",
		SyncStatus: "OutOfSync", Health: "Healthy", LastMessage: "ImagePullBackOff",
	}}

	got := Compute(d, a, envs)
	if len(got) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(got), got)
	}
	if got[0].Kind != KindSyncFailed {
		t.Errorf("Kind = %v, want %v", got[0].Kind, KindSyncFailed)
	}
	if got[0].Severity != 3 {
		t.Errorf("Severity = %d, want 3 (failed sync in prod)", got[0].Severity)
	}
}

// Direction matters: when the cluster cannot reach git, the fix is to move git back to
// what is running, not forward again to the version that already failed.
func TestPlanForSyncFailureRollsGitBackwards(t *testing.T) {
	f := Finding{
		Kind: KindSyncFailed, Service: "product-catalog", Env: "prod",
		Desired: "v1.2.0", Actual: "v1.1.0", Path: "overlays/prod/kustomization.yaml",
		Detail: "ImagePullBackOff",
	}
	plan := f.Plan()
	if plan == nil {
		t.Fatal("expected a plan")
	}
	if plan.From != "v1.2.0" || plan.To != "v1.1.0" {
		t.Errorf("plan = %s -> %s, want v1.2.0 -> v1.1.0 (pin git to what is running)", plan.From, plan.To)
	}
}

// Promotion gaps go the other way.
func TestPlanForPromotionGapMovesForward(t *testing.T) {
	d := []Desired{desired("stage", "product-catalog", "v1.2.0"), desired("prod", "product-catalog", "v1.1.0")}
	a := []Actual{actual("stage", "product-catalog", "v1.2.0"), actual("prod", "product-catalog", "v1.1.0")}

	got := Compute(d, a, envs)
	if len(got) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(got), got)
	}
	if got[0].Kind != KindPromotionGap {
		t.Fatalf("Kind = %v, want %v", got[0].Kind, KindPromotionGap)
	}
	plan := got[0].Plan()
	if plan.From != "v1.1.0" || plan.To != "v1.2.0" {
		t.Errorf("plan = %s -> %s, want v1.1.0 -> v1.2.0 (promote)", plan.From, plan.To)
	}
	if got[0].Env != "prod" {
		t.Errorf("Env = %s, want prod (the lagging side)", got[0].Env)
	}
}

// A loud finding must suppress the quiet one for the same service and environment,
// otherwise every deploy failure is reported twice.
func TestPromotionGapSuppressedWhenSyncAlreadyFailed(t *testing.T) {
	d := []Desired{desired("stage", "product-catalog", "v1.2.0"), desired("prod", "product-catalog", "v1.1.0")}
	a := []Actual{
		actual("stage", "product-catalog", "v1.2.0"),
		{Service: "product-catalog", Env: "prod", Version: "v1.0.0", SyncStatus: "OutOfSync",
			Health: "Healthy", LastMessage: "manifest generation error"},
	}

	got := Compute(d, a, envs)
	for _, f := range got {
		if f.Kind == KindPromotionGap {
			t.Errorf("promotion gap should be suppressed, got %v", f)
		}
	}
}

func TestVPrefixIsNotDrift(t *testing.T) {
	d := []Desired{desired("prod", "checkout", "1.4.0")}
	a := []Actual{actual("prod", "checkout", "v1.4.0")}

	if got := Compute(d, a, envs); len(got) != 0 {
		t.Errorf("v-prefix mismatch should not be drift, got %v", got)
	}
}

func TestUnhealthyAtCorrectVersionHasNoPlan(t *testing.T) {
	f := Finding{Kind: KindUnhealthy, Service: "payments", Env: "prod",
		Desired: "v2.0.0", Actual: "v2.0.0", Path: "overlays/prod/kustomization.yaml", Detail: "Degraded"}
	if f.Plan() != nil {
		t.Error("a degraded pod is not fixable by editing a version in git")
	}
}

func TestMissingFromClusterIsHighSeverity(t *testing.T) {
	d := []Desired{desired("prod", "payments", "v2.0.0")}
	got := Compute(d, nil, envs)
	if len(got) != 1 || got[0].Kind != KindMissing || got[0].Severity != 3 {
		t.Errorf("got %v, want one high-severity missing finding", got)
	}
}

func TestProdSortsFirst(t *testing.T) {
	d := []Desired{desired("stage", "a", "v2"), desired("prod", "a", "v2")}
	a := []Actual{actual("stage", "a", "v1"), actual("prod", "a", "v1")}

	got := Compute(d, a, envs)
	if len(got) < 2 {
		t.Fatalf("want 2 findings, got %d", len(got))
	}
	if got[0].Env != "prod" {
		t.Errorf("first finding env = %s, want prod", got[0].Env)
	}
}
