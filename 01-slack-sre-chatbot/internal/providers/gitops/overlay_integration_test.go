package gitops

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
)

// These tests run against the real overlays checked into gitops/overlays, not a
// fixture. A fixture that drifts from the manifests Argo CD actually applies would let
// the parser break silently, which is the one failure mode that would make the agent
// confidently wrong.
//
// The local kind stack has its own copy in gitops/overlays-local, so it can never change
// what the cloud stack deploys. Both trees have to stay valid and keep the same drift.
const (
	overlayRoot      = "../../../gitops/overlays"
	localOverlayRoot = "../../../gitops/overlays-local"
)

var overlayRoots = []string{overlayRoot, localOverlayRoot}

// readOverlay reads an overlay from the cloud tree.
func readOverlay(t *testing.T, env string) string {
	t.Helper()
	return readOverlayAt(t, overlayRoot, env)
}

func readOverlayAt(t *testing.T, root, env string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, env, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("reading the %s overlay under %s: %v", env, root, err)
	}
	return string(b)
}

func TestRealOverlaysParse(t *testing.T) {
	want := map[string]string{"test": "v1.2.0", "stage": "v1.2.0", "prod": "v1.1.0"}

	for _, root := range overlayRoots {
		for env, wantTag := range want {
			var k kustomization
			if err := yaml.Unmarshal([]byte(readOverlayAt(t, root, env)), &k); err != nil {
				t.Fatalf("%s/%s: %v", root, env, err)
			}
			if len(k.Images) == 0 {
				t.Fatalf("%s/%s: no images block found", root, env)
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
						t.Errorf("%s/%s: product-catalog pinned to %s, want %s", root, env, img.NewTag, wantTag)
					}
				}
			}
			if !found {
				t.Errorf("%s/%s: product-catalog not found in the images block", root, env)
			}
		}
	}
}

// The demo depends on prod lagging stage. If someone bumps prod by hand, the talk has
// no drift to find, and this test says so before the room does.
func TestDemoDriftIsStaged(t *testing.T) {
	for _, root := range overlayRoots {
		var stage, prod kustomization
		_ = yaml.Unmarshal([]byte(readOverlayAt(t, root, "stage")), &stage)
		_ = yaml.Unmarshal([]byte(readOverlayAt(t, root, "prod")), &prod)

		if tagOf(stage) == tagOf(prod) {
			t.Fatalf("%s: stage and prod are both on %s, there is no promotion gap to demo", root, tagOf(prod))
		}
	}
}

// The local cluster is a single kind node, so every local environment runs one replica.
func TestLocalOverlaysUseOneReplica(t *testing.T) {
	replicas := regexp.MustCompile(`path: /spec/replicas\n\s+value: 1\n`)
	for _, env := range []string{"test", "stage", "prod"} {
		if !replicas.MatchString(readOverlayAt(t, localOverlayRoot, env)) {
			t.Errorf("%s: the local overlay does not set /spec/replicas to 1", env)
		}
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
