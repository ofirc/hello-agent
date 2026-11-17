package agentloop

import (
	"regexp"
	"testing"
)

// These patterns judge the debug model's root cause in the eval checks. They live in a
// file without the eval build tag so a plain `go test` checks them against sample
// wording, and the eval test cannot fail a correct answer because a pattern is off.

// unpromotedPattern accepts "not promoted", "has not yet been promoted", "hasn't been
// promoted", "never promoted", as long as the negation and "promoted" share a sentence.
var unpromotedPattern = regexp.MustCompile(`(?i)\b(not|never)\b[^.]*\bpromoted\b|n't[^.]*\bpromoted\b`)

// wrongKindPattern matches only claims that a deployment or sync failed. The word "fail"
// alone is not enough: the skill's own wording for an unpromoted release is "nothing
// failed, the release was simply never promoted".
var wrongKindPattern = regexp.MustCompile(`(?i)sync(ing)? failed|failed to (sync|deploy|roll ?out)|deploy(ment)? fail(ed|ure)|roll ?out failed`)

func saysUnpromoted(rootCause string) bool { return unpromotedPattern.MatchString(rootCause) }
func claimsFailure(rootCause string) bool  { return wrongKindPattern.MatchString(rootCause) }

func TestDebugVerdictPatternsJudgeSampleAnswers(t *testing.T) {
	for _, tc := range []struct {
		text       string
		unpromoted bool
		failure    bool
	}{
		{"Nothing failed, the release was simply never promoted.", true, false},
		{"prod has not yet been promoted", true, false},
		{"hasn't been promoted to prod", true, false},
		{"The sync failed with ImagePullBackOff", false, true},
		{"the deployment failed", false, true},
	} {
		if got := saysUnpromoted(tc.text); got != tc.unpromoted {
			t.Errorf("saysUnpromoted(%q) = %v, want %v", tc.text, got, tc.unpromoted)
		}
		if got := claimsFailure(tc.text); got != tc.failure {
			t.Errorf("claimsFailure(%q) = %v, want %v", tc.text, got, tc.failure)
		}
	}
}
