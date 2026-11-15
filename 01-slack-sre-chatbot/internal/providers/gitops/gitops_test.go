package gitops

import "testing"

const overlay = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: prod

resources:
  - ../../base

# Pinned by release engineering. The SRE agent edits newTag only.
images:
  - name: us-east1-docker.pkg.dev/PROJECT_ID/stack/product-catalog
    newTag: v1.1.0
  - name: us-east1-docker.pkg.dev/PROJECT_ID/stack/checkout
    newTag: v2.4.1

replicas:
  - name: product-catalog
    count: 3
`

func TestSetTagChangesOnlyTheTargetService(t *testing.T) {
	got, err := SetTag(overlay, "product-catalog", "v1.2.0")
	if err != nil {
		t.Fatalf("SetTag: %v", err)
	}
	if !contains(got, "newTag: v1.2.0") {
		t.Error("product-catalog was not bumped")
	}
	if !contains(got, "newTag: v2.4.1") {
		t.Error("checkout was changed, it should not have been")
	}
}

// The whole reason SetTag is a line edit rather than a YAML round trip.
func TestSetTagPreservesCommentsAndStructure(t *testing.T) {
	got, err := SetTag(overlay, "product-catalog", "v1.2.0")
	if err != nil {
		t.Fatalf("SetTag: %v", err)
	}
	for _, want := range []string{
		"# Pinned by release engineering. The SRE agent edits newTag only.",
		"namespace: prod",
		"  - ../../base",
		"replicas:",
		"    count: 3",
	} {
		if !contains(got, want) {
			t.Errorf("lost %q from the document", want)
		}
	}
	if countLines(got) != countLines(overlay) {
		t.Errorf("line count changed: %d -> %d, the diff should be exactly one line",
			countLines(overlay), countLines(got))
	}
}

func TestSetTagIndentationIsPreserved(t *testing.T) {
	got, err := SetTag(overlay, "checkout", "v2.5.0")
	if err != nil {
		t.Fatalf("SetTag: %v", err)
	}
	if !contains(got, "    newTag: v2.5.0") {
		t.Error("expected the replacement to keep four-space indentation")
	}
}

func TestSetTagUnknownServiceIsAnError(t *testing.T) {
	if _, err := SetTag(overlay, "payments", "v1.0.0"); err == nil {
		t.Error("expected an error for a service the overlay does not pin")
	}
}

// A digest-pinned image has no newTag to edit, and silently doing nothing would be the
// worst outcome: the agent would report a fix it did not make.
func TestSetTagDigestPinnedIsAnError(t *testing.T) {
	const digestOverlay = `images:
  - name: us-east1-docker.pkg.dev/p/stack/product-catalog
    digest: sha256:abc123
`
	if _, err := SetTag(digestOverlay, "product-catalog", "v1.2.0"); err == nil {
		t.Error("expected an error when the image is pinned by digest")
	}
}

func TestServiceFromImage(t *testing.T) {
	cases := map[string]string{
		"us-east1-docker.pkg.dev/proj/stack/product-catalog": "product-catalog",
		"us-east1-docker.pkg.dev/proj/stack/checkout:v1.0.0": "checkout",
		"nginx":                   "nginx",
		"localhost:5000/payments": "payments",
	}
	for in, want := range cases {
		if got := serviceFromImage(in); got != want {
			t.Errorf("serviceFromImage(%q) = %q, want %q", in, got, want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func countLines(s string) int {
	n := 1
	for _, c := range s {
		if c == '\n' {
			n++
		}
	}
	return n
}
