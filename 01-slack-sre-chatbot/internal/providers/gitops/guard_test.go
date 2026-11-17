package gitops

import (
	"strings"
	"testing"
)

func TestValidateOverlayAcceptsRealOverlays(t *testing.T) {
	for _, env := range []string{"test", "stage", "prod"} {
		if err := ValidateOverlay(readOverlay(t, env)); err != nil {
			t.Errorf("%s: %v", env, err)
		}
	}
}

func TestValidateOverlayAcceptsLocalOverlays(t *testing.T) {
	for _, env := range []string{"test", "stage", "prod"} {
		if err := ValidateOverlay(readOverlayAt(t, localOverlayRoot, env)); err != nil {
			t.Errorf("%s: %v", env, err)
		}
	}
}

func TestValidateOverlayRejects(t *testing.T) {
	cases := map[string]string{
		"not yaml":  "not: [yaml",
		"no images": "apiVersion: v1\n",
	}
	for name, content := range cases {
		if err := ValidateOverlay(content); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// padTo pads a valid overlay with one YAML comment line so the result is exactly size
// bytes. It stays valid YAML with a real images block, so only the size check can
// reject it.
func padTo(t *testing.T, content string, size int) string {
	t.Helper()
	fill := size - len(content) - len("# \n")
	if fill < 0 {
		t.Fatalf("overlay is already %d bytes, cannot pad to %d", len(content), size)
	}
	out := content + "# " + strings.Repeat("x", fill) + "\n"
	if len(out) != size {
		t.Fatalf("padded to %d bytes, want %d", len(out), size)
	}
	return out
}

func TestValidateOverlaySizeLimit(t *testing.T) {
	prod := readOverlay(t, "prod")

	over := padTo(t, prod, 65536)
	err := ValidateOverlay(over)
	if err == nil {
		t.Fatal("a 65536 byte overlay should be rejected")
	}
	if !strings.Contains(err.Error(), "too large") || !strings.Contains(err.Error(), "65536") {
		t.Errorf("error %q should say the overlay is too large and name the limit", err)
	}

	if err := ValidateOverlay(padTo(t, prod, 65535)); err != nil {
		t.Errorf("a 65535 byte overlay should be accepted: %v", err)
	}
}

func TestCheckProtectedAllowsTagAndReplicas(t *testing.T) {
	prod := readOverlay(t, "prod")

	for name, edit := range map[string][2]string{
		"tag":      {"newTag: v1.1.0", "newTag: v1.2.0"},
		"replicas": {"value: 3", "value: 5"},
	} {
		next := strings.Replace(prod, edit[0], edit[1], 1)
		if next == prod {
			t.Fatalf("%s: the edit did not change the overlay, the test is not testing anything", name)
		}
		if err := CheckProtected(prod, next); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCheckProtectedRejects(t *testing.T) {
	prod := readOverlay(t, "prod")

	const partOfOp = "path: /metadata/labels/app.kubernetes.io~1part-of\n        value: prod"

	// Cut the whole part-of patch entry, from its comment to the blank line after it.
	start := strings.Index(prod, "  # part-of is what")
	end := strings.Index(prod, "# The SRE agent edits")
	if start < 0 || end < start {
		t.Fatal("could not locate the part-of patch in the prod overlay")
	}
	withoutPartOf := prod[:start] + prod[end:]

	cases := []struct {
		name string
		next string
		want string
	}{
		{"resources", strings.Replace(prod, "- ../../base", "- github.com/evil/base", 1), "resources"},
		{"namespace", strings.Replace(prod, "namespace: prod", "namespace: kube-system", 1), "namespace"},
		{"image newName", strings.Replace(prod, "newName: ghcr.io/ofirc/product-catalog", "newName: ghcr.io/evil/product-catalog", 1), "images"},
		{"extra image", strings.Replace(prod, "    newTag: v1.1.0\n", "    newTag: v1.1.0\n  - name: extra\n    newName: ghcr.io/ofirc/extra\n    newTag: v1\n", 1), "images"},
		{"part-of value", strings.Replace(prod, partOfOp, strings.Replace(partOfOp, "value: prod", "value: stage", 1), 1), "part-of"},
		{"part-of removed", withoutPartOf, "part-of"},
	}
	for _, tc := range cases {
		if tc.next == prod {
			t.Errorf("%s: the edit did not change the overlay, the test is not testing anything", tc.name)
			continue
		}
		err := CheckProtected(prod, tc.next)
		if err == nil {
			t.Errorf("%s: expected an error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.want)
		}
	}
}

const (
	patchBase = "  - target:\n      kind: Deployment\n      name: product-catalog\n    patch: |-\n"
	prodImage = "  - name: product-catalog\n    newName: ghcr.io/ofirc/product-catalog\n    newTag: v1.1.0\n"
	extraImg  = "  - name: extra\n    newName: ghcr.io/ofirc/extra\n    newTag: v1\n"
)

// patchEntry is a whole extra patches entry holding one part-of op.
func patchEntry(op, value string) string {
	return patchBase +
		"      - op: " + op + "\n" +
		"        path: /metadata/labels/app.kubernetes.io~1part-of\n" +
		"        value: " + value + "\n\n"
}

// Kustomize applies patches in order, so the last op on a label wins. Every part-of op
// has to match, not just the first.
func TestCheckProtectedPartOfAllOps(t *testing.T) {
	prod := readOverlay(t, "prod")

	const partOfOp = "path: /metadata/labels/app.kubernetes.io~1part-of\n        value: prod"
	const secondOp = "\n      - op: replace\n        path: /metadata/labels/app.kubernetes.io~1part-of\n        value: "

	start := strings.Index(prod, "  # part-of is what")
	end := strings.Index(prod, "# The SRE agent edits")
	if start < 0 || end < start {
		t.Fatal("could not locate the part-of patch in the prod overlay")
	}
	withoutPartOf := prod[:start] + prod[end:]

	// images with an extra entry, in both orders.
	imagesOld := strings.Replace(prod, prodImage, prodImage+extraImg, 1)
	imagesNew := strings.Replace(prod, prodImage, extraImg+prodImage, 1)

	cases := []struct {
		name    string
		next    string
		wantErr bool
	}{
		{"later patch entry replaces with stage", prod[:end] + patchEntry("replace", "stage") + prod[end:], true},
		{"second op in the same patch changes it", strings.Replace(prod, partOfOp, partOfOp+secondOp+"stage", 1), true},
		{"duplicate op with the same value", strings.Replace(prod, partOfOp, partOfOp+secondOp+"prod", 1), false},
		{"part-of moved to another patch entry, same value", withoutPartOf[:start] + patchEntry("add", "prod") + withoutPartOf[start:], false},
	}
	for _, tc := range cases {
		if tc.next == prod {
			t.Errorf("%s: the edit did not change the overlay, the test is not testing anything", tc.name)
			continue
		}
		err := CheckProtected(prod, tc.next)
		switch {
		case tc.wantErr && err == nil:
			t.Errorf("%s: expected an error", tc.name)
		case tc.wantErr && !strings.Contains(err.Error(), "part-of"):
			t.Errorf("%s: error %q does not mention part-of", tc.name, err)
		case !tc.wantErr && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		}
	}

	if imagesOld == prod || imagesNew == imagesOld {
		t.Fatal("the images reorder edit did not change anything")
	}
	if err := CheckProtected(imagesOld, imagesNew); err != nil {
		t.Errorf("reordering images should be accepted: %v", err)
	}
}
