package agentloop

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

const prodOverlayPath = "gitops/overlays/prod/kustomization.yaml"

func readRealOverlay(t *testing.T, env string) string {
	t.Helper()
	b, err := os.ReadFile("../../gitops/overlays/" + env + "/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// changedLines returns the diff's - and + content lines, leaving out the file headers.
func changedLines(diff string) (minus, plus []string) {
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "---"), strings.HasPrefix(l, "+++"):
		case strings.HasPrefix(l, "-"):
			minus = append(minus, l)
		case strings.HasPrefix(l, "+"):
			plus = append(plus, l)
		}
	}
	return minus, plus
}

func TestUnifiedDiffOneLine(t *testing.T) {
	old := readRealOverlay(t, "prod")
	bumped := strings.Replace(old, "newTag: v1.1.0", "newTag: v1.2.0", 1)
	if bumped == old {
		t.Fatal("the prod overlay no longer pins v1.1.0")
	}
	d := UnifiedDiff(prodOverlayPath, old, bumped)
	if !strings.HasPrefix(d, "--- a/"+prodOverlayPath+"\n+++ b/"+prodOverlayPath+"\n@@ ") {
		t.Fatalf("headers wrong:\n%s", d)
	}
	minus, plus := changedLines(d)
	if len(minus) != 1 || minus[0] != "-    newTag: v1.1.0" {
		t.Fatalf("- lines = %q\n%s", minus, d)
	}
	if len(plus) != 1 || plus[0] != "+    newTag: v1.2.0" {
		t.Fatalf("+ lines = %q\n%s", plus, d)
	}

	// Three lines of context each side, and line numbers that match the file.
	lines := strings.Split(old, "\n")
	at := 0
	for i, l := range lines {
		if l == "    newTag: v1.1.0" {
			at = i + 1
		}
	}
	wantHunk := fmt.Sprintf("@@ -%d,7 +%d,7 @@\n", at-3, at-3)
	if !strings.Contains(d, wantHunk) {
		t.Fatalf("want hunk header %q in:\n%s", wantHunk, d)
	}
	if strings.Count(d, "\n ") != 6 {
		t.Fatalf("want 6 context lines:\n%s", d)
	}
}

func TestUnifiedDiffNoChange(t *testing.T) {
	old := readRealOverlay(t, "prod")
	if d := UnifiedDiff(prodOverlayPath, old, old); d != "" {
		t.Fatalf("diff of equal inputs = %q", d)
	}
}

func TestUnifiedDiffHunks(t *testing.T) {
	for name, tc := range map[string]struct{ old, new, want string }{
		"insert at start": {
			old:  "a\nb\n",
			new:  "x\na\nb\n",
			want: "@@ -1,2 +1,3 @@\n+x\n a\n b\n",
		},
		"delete everything": {
			old:  "a\nb\n",
			new:  "",
			want: "@@ -1,2 +0,0 @@\n-a\n-b\n",
		},
		"append to empty": {
			old:  "",
			new:  "a\n",
			want: "@@ -0,0 +1,1 @@\n+a\n",
		},
		"two hunks far apart": {
			old:  "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n",
			new:  "one\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\ntwelve\n",
			want: "@@ -1,4 +1,4 @@\n-1\n+one\n 2\n 3\n 4\n@@ -9,4 +9,4 @@\n 9\n 10\n 11\n-12\n+twelve\n",
		},
		"close changes share a hunk": {
			old:  "1\n2\n3\n4\n5\n6\n7\n8\n",
			new:  "one\n2\n3\n4\n5\n6\n7\neight\n",
			want: "@@ -1,8 +1,8 @@\n-1\n+one\n 2\n 3\n 4\n 5\n 6\n 7\n-8\n+eight\n",
		},
		"missing final newline": {
			old:  "a\nb",
			new:  "a\nc",
			want: "@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+c\n\\ No newline at end of file\n",
		},
	} {
		got := UnifiedDiff("f", tc.old, tc.new)
		if want := "--- a/f\n+++ b/f\n" + tc.want; got != want {
			t.Errorf("%s:\ngot:\n%s\nwant:\n%s", name, got, want)
		}
	}
}
