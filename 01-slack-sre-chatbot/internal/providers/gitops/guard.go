package gitops

import (
	"fmt"
	"reflect"

	"gopkg.in/yaml.v3"
)

// maxOverlayBytes bounds what the agent may propose. A real overlay is well under 2 KB,
// so this only ever trips on runaway output, and it keeps a bad proposal from becoming
// a huge commit.
const maxOverlayBytes = 65536

// partOfPath is the JSON6902 path of the label the agent reads to work out which
// environment an Argo CD application belongs to.
const partOfPath = "/metadata/labels/app.kubernetes.io~1part-of"

// ValidateOverlay is the cheap gate on a proposed overlay: small enough, parses as YAML,
// and still pins at least one image. It does not judge whether the content is a good
// idea, that is for CheckProtected and the human reviewing the PR.
func ValidateOverlay(content string) error {
	if len(content) >= maxOverlayBytes {
		return fmt.Errorf("overlay is too large: %d bytes, the limit is %d", len(content), maxOverlayBytes)
	}

	var k kustomization
	if err := yaml.Unmarshal([]byte(content), &k); err != nil {
		return fmt.Errorf("overlay is not valid kustomization YAML: %w", err)
	}

	for _, img := range k.Images {
		if img.Name != "" {
			return nil
		}
	}
	return fmt.Errorf("overlay has no images entry with a name")
}

// CheckProtected rejects a proposal that changes a field the rest of the system depends
// on: where the manifests come from, which namespace they land in, which images are
// pinned, and the part-of label. Tags, replicas and everything else stay editable.
//
// The list is deliberately short. It is a safety limit, not a policy engine, and other
// ways of changing what gets deployed are left to PR review.
func CheckProtected(oldContent, newContent string) error {
	oldDoc, err := parseDoc(oldContent)
	if err != nil {
		return fmt.Errorf("current overlay: %w", err)
	}
	newDoc, err := parseDoc(newContent)
	if err != nil {
		return fmt.Errorf("proposed overlay: %w", err)
	}

	if !reflect.DeepEqual(oldDoc["resources"], newDoc["resources"]) {
		return fmt.Errorf("the resources field is protected and must not change")
	}
	if !reflect.DeepEqual(oldDoc["namespace"], newDoc["namespace"]) {
		return fmt.Errorf("the namespace field is protected and must not change")
	}
	if !reflect.DeepEqual(imagePairs(oldDoc), imagePairs(newDoc)) {
		return fmt.Errorf("the images name and newName values are protected, only newTag may change")
	}

	// Kustomize applies patches in order, so a later op wins. Looking only at the first
	// part-of op would let a proposal keep an early correct one and override it after.
	// Every op that touches the label has to agree with the old value.
	if oldVals := partOfValues(oldDoc); len(oldVals) > 0 {
		newVals := partOfValues(newDoc)
		if len(newVals) == 0 {
			return fmt.Errorf("the part-of label patch is protected and must not be removed")
		}
		for _, v := range newVals {
			if !reflect.DeepEqual(v, oldVals[0]) {
				return fmt.Errorf("the part-of label patch is protected and must stay %v, found %v", oldVals[0], v)
			}
		}
	}
	return nil
}

func parseDoc(content string) (map[string]any, error) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// imagePairs is the multiset of (name, newName) pairs, so reordering entries is fine
// but adding, dropping or retargeting one is not.
func imagePairs(doc map[string]any) map[[2]string]int {
	pairs := map[[2]string]int{}
	images, _ := doc["images"].([]any)
	for _, raw := range images {
		img, _ := raw.(map[string]any)
		name, _ := img["name"].(string)
		newName, _ := img["newName"].(string)
		pairs[[2]string{name, newName}]++
	}
	return pairs
}

// partOfValues returns the value of every op that sets the part-of label, across all
// patches. Each patch is a YAML string of JSON6902 ops embedded in the kustomization, so
// it needs a second parse. Patches that are not op lists (strategic merge patches) are
// skipped.
func partOfValues(doc map[string]any) []any {
	var values []any
	patches, _ := doc["patches"].([]any)
	for _, raw := range patches {
		entry, _ := raw.(map[string]any)
		patch, _ := entry["patch"].(string)
		if patch == "" {
			continue
		}

		var ops []map[string]any
		if err := yaml.Unmarshal([]byte(patch), &ops); err != nil {
			continue
		}
		for _, op := range ops {
			if op["path"] == partOfPath {
				values = append(values, op["value"])
			}
		}
	}
	return values
}
