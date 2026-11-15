package gitops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// These tests run against the real overlays checked into gitops/overlays, not a
// fixture. A fixture that drifts from the manifests Argo CD actually applies would let
// the parser break silently, which is the one failure mode that would make the agent
// confidently wrong.
const overlayRoot = "../../../gitops/overlays"

func readOverlay(t *testing.T, env string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(overlayRoot, env, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("reading the %s overlay: %v", env, err)
	}
	return string(b)
}

func TestRealOverlaysParse(t *testing.T) {
	want := map[string]string{"test": "v1.2.0", "stage": "v1.2.0", "prod": "v1.1.0"}

	for env, wantTag := range want {
		var k kustomization
		if err := yaml.Unmarshal([]byte(readOverlay(t, env)), &k); err != nil {
			t.Fatalf("%s: %v", env, err)
		}
		if len(k.Images) == 0 {
			t.Fatalf("%s: no images block found", env)
		}

		var found bool
		for _, img := range k.Images {
			name := img.NewName
			if name == "" {
				name = img.Name
			}
			if serviceFromImage(name) == "product-catalog" {
				found = true
				if img.NewTag != wantTag {
					t.Errorf("%s: product-catalog pinned to %s, want %s", env, img.NewTag, wantTag)
				}
			}
		}
		if !found {
			t.Errorf("%s: product-catalog not found in the images block", env)
		}
	}
}

// The demo depends on prod lagging stage. If someone bumps prod by hand, the talk has
// no drift to find, and this test says so before the room does.
func TestDemoDriftIsStaged(t *testing.T) {
	var stage, prod kustomization
	_ = yaml.Unmarshal([]byte(readOverlay(t, "stage")), &stage)
	_ = yaml.Unmarshal([]byte(readOverlay(t, "prod")), &prod)

	if tagOf(stage) == tagOf(prod) {
		t.Fatalf("stage and prod are both on %s, there is no promotion gap to demo", tagOf(prod))
	}
}

// SetTag must work on the real file, including its comments and the replacements block
// that also contains "name: product-catalog" lines.
func TestSetTagOnRealProdOverlay(t *testing.T) {
	original := readOverlay(t, "prod")

	got, err := SetTag(original, "product-catalog", "v1.2.0")
	if err != nil {
		t.Fatalf("SetTag on the real overlay: %v", err)
	}
	if !strings.Contains(got, "newTag: v1.2.0") {
		t.Error("the tag was not updated")
	}
	if strings.Count(got, "\n") != strings.Count(original, "\n") {
		t.Error("line count changed, the PR diff should be exactly one line")
	}

	// The replacements block below images: must be untouched. It contains its own
	// "name: product-catalog", which is precisely the thing a naive parser trips on.
	for _, want := range []string{
		"replacements:",
		"fieldPath: spec.template.spec.containers.[name=product-catalog].image",
		"namespace: prod",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %q", want)
		}
	}

	// And the result must still be valid YAML that parses to the new tag.
	var k kustomization
	if err := yaml.Unmarshal([]byte(got), &k); err != nil {
		t.Fatalf("the edited overlay is no longer valid YAML: %v", err)
	}
	if tagOf(k) != "v1.2.0" {
		t.Errorf("round trip gave %s, want v1.2.0", tagOf(k))
	}
}

func tagOf(k kustomization) string {
	for _, img := range k.Images {
		name := img.NewName
		if name == "" {
			name = img.Name
		}
		if serviceFromImage(name) == "product-catalog" {
			return img.NewTag
		}
	}
	return ""
}
