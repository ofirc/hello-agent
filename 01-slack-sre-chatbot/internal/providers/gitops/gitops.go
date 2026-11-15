// Package gitops reads desired state out of the GitOps repo.
//
// Desired state lives in one kustomize overlay per environment:
//
//	<prefix>/<env>/kustomization.yaml
//
// with an images: block that pins each service to a tag. That file is the single source
// of truth the fix PR edits.
package gitops

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/github"
)

type Client struct {
	gh     *github.Client
	prefix string
	ref    string
}

func New(gh *github.Client, prefix, ref string) *Client {
	return &Client{gh: gh, prefix: strings.Trim(prefix, "/"), ref: ref}
}

func (c *Client) Configured() bool { return c.gh != nil && c.gh.Configured() }

type kustomization struct {
	Images []struct {
		Name    string `yaml:"name"`
		NewName string `yaml:"newName"`
		NewTag  string `yaml:"newTag"`
	} `yaml:"images"`
}

func (c *Client) Path(env string) string {
	return fmt.Sprintf("%s/%s/kustomization.yaml", c.prefix, env)
}

// Desired reads every environment overlay in parallel. Three sequential GitHub round
// trips would eat a meaningful slice of the two-second budget for the fast commands.
func (c *Client) Desired(ctx context.Context, envs []string) ([]drift.Desired, error) {
	if !c.Configured() {
		return nil, fmt.Errorf("github is not configured (set GITHUB_TOKEN)")
	}

	type result struct {
		items []drift.Desired
		err   error
	}
	results := make([]result, len(envs))

	var wg sync.WaitGroup
	for i, env := range envs {
		wg.Add(1)
		go func(i int, env string) {
			defer wg.Done()
			items, err := c.desiredForEnv(ctx, env)
			results[i] = result{items, err}
		}(i, env)
	}
	wg.Wait()

	var out []drift.Desired
	var errs []string
	for i, r := range results {
		if r.err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", envs[i], r.err))
			continue
		}
		out = append(out, r.items...)
	}
	// Partial success is still useful: knowing prod drifted matters even if the test
	// overlay is malformed.
	if len(out) == 0 && len(errs) > 0 {
		return nil, fmt.Errorf("could not read any overlay: %s", strings.Join(errs, "; "))
	}
	return out, nil
}

func (c *Client) desiredForEnv(ctx context.Context, env string) ([]drift.Desired, error) {
	path := c.Path(env)
	f, err := c.gh.GetFile(ctx, path, c.ref)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, nil // environment not onboarded, not an error
	}

	var k kustomization
	if err := yaml.Unmarshal([]byte(f.Content), &k); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	out := make([]drift.Desired, 0, len(k.Images))
	for _, img := range k.Images {
		name := img.NewName
		if name == "" {
			name = img.Name
		}
		if name == "" || img.NewTag == "" {
			continue
		}
		out = append(out, drift.Desired{
			Service: serviceFromImage(name),
			Env:     env,
			Version: img.NewTag,
			Path:    path,
		})
	}
	return out, nil
}

// SetTag rewrites one image's newTag in place.
//
// This is a line edit rather than unmarshal-modify-marshal on purpose. Round-tripping
// through yaml.Marshal discards comments, reorders keys and reflows the document, which
// turns a one-line fix into an unreviewable diff. A reviewer has to be able to see at a
// glance that the bot changed exactly one version and nothing else.
func SetTag(content, service, newTag string) (string, error) {
	lines := strings.Split(content, "\n")

	inImages := false
	matched := false
	imagesIndent := -1

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		if !inImages {
			if trimmed == "images:" {
				inImages, imagesIndent = true, indent
			}
			continue
		}
		// A key at or left of images: ends the block.
		if indent <= imagesIndent && !strings.HasPrefix(trimmed, "-") {
			break
		}

		// Entering a new list item resets whether we are on the target image.
		if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			matched = isNameLine(rest, service)
			continue
		}
		if strings.HasPrefix(trimmed, "name:") || strings.HasPrefix(trimmed, "newName:") {
			matched = isNameLine(trimmed, service)
			continue
		}
		if matched && strings.HasPrefix(trimmed, "newTag:") {
			lines[i] = strings.Repeat(" ", indent) + "newTag: " + newTag
			return strings.Join(lines, "\n"), nil
		}
	}
	return "", fmt.Errorf("no newTag found for service %q, the overlay may pin it by digest or not list it at all", service)
}

func isNameLine(s, service string) bool {
	for _, k := range []string{"name:", "newName:"} {
		if strings.HasPrefix(s, k) {
			v := strings.TrimSpace(strings.TrimPrefix(s, k))
			v = strings.Trim(v, `"'`)
			return serviceFromImage(v) == service
		}
	}
	return false
}

// serviceFromImage reduces a full image reference to its service name:
// us-east1-docker.pkg.dev/proj/stack/product-catalog -> product-catalog
func serviceFromImage(name string) string {
	if i := strings.LastIndex(name, ":"); i >= 0 && !strings.Contains(name[i+1:], "/") {
		name = name[:i]
	}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}
