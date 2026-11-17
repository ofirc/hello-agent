package gitops

import "testing"

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
