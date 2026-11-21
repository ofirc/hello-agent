// Package github is a small REST client covering exactly the six calls the agent needs
// to read a file and open a pull request. Pulling in go-github for this would add a
// large dependency surface to a bot whose entire GitHub vocabulary is "read file",
// "branch", "commit", "open PR".
package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/httpx"
)

const apiBase = "https://api.github.com"

type Client struct {
	src   TokenSource
	owner string
	repo  string
	http  *http.Client
}

// New takes nil when no credential is configured, and then reports !Configured.
func New(src TokenSource, owner, repo string) *Client {
	return &Client{src: src, owner: owner, repo: repo, http: httpx.Default()}
}

func (c *Client) Configured() bool { return c.src != nil && c.owner != "" && c.repo != "" }

func (c *Client) Slug() string { return c.owner + "/" + c.repo }

// Identity is whose name the agent writes to GitHub under, "" when not configured.
func (c *Client) Identity() string {
	if c == nil || c.src == nil {
		return ""
	}
	return c.src.Identity()
}

type File struct {
	Path    string
	Content string
	SHA     string // blob sha, required to update the file without clobbering
}

// GetFile reads a file at a ref. A missing file returns a nil File and no error, since
// "the overlay does not exist" is a normal answer for an environment not yet onboarded.
func (c *Client) GetFile(ctx context.Context, path, ref string) (*File, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s",
		apiBase, c.owner, c.repo, pathEscape(path), url.QueryEscape(ref))

	body, err := c.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		if ae, ok := httpx.AsAPIError(err); ok && ae.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}

	var resp struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		SHA      string `json:"sha"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("github: decode contents: %w", err)
	}
	if resp.Encoding != "base64" {
		return nil, fmt.Errorf("github: unexpected content encoding %q for %s", resp.Encoding, path)
	}
	// The API wraps base64 at 60 columns.
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(resp.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("github: decode base64 for %s: %w", path, err)
	}
	return &File{Path: path, Content: string(raw), SHA: resp.SHA}, nil
}

// HeadSHA returns the commit sha a branch points at.
func (c *Client) HeadSHA(ctx context.Context, branch string) (string, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/git/ref/heads/%s", apiBase, c.owner, c.repo, url.PathEscape(branch))
	body, err := c.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	var resp struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("github: decode ref: %w", err)
	}
	return resp.Object.SHA, nil
}

// CreateBranch is idempotent: an existing branch at the same name is not an error, which
// matters when someone runs drift fix twice on the same drift.
func (c *Client) CreateBranch(ctx context.Context, branch, fromSHA string) error {
	u := fmt.Sprintf("%s/repos/%s/%s/git/refs", apiBase, c.owner, c.repo)
	payload := map[string]string{"ref": "refs/heads/" + branch, "sha": fromSHA}
	_, err := c.do(ctx, http.MethodPost, u, payload)
	if err != nil {
		if ae, ok := httpx.AsAPIError(err); ok && ae.Status == http.StatusUnprocessableEntity &&
			strings.Contains(ae.Body, "already exists") {
			return nil
		}
		return err
	}
	return nil
}

// PutFile commits a new version of a file on a branch.
func (c *Client) PutFile(ctx context.Context, branch, path, content, blobSHA, message string) error {
	u := fmt.Sprintf("%s/repos/%s/%s/contents/%s", apiBase, c.owner, c.repo, pathEscape(path))
	payload := map[string]string{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"branch":  branch,
	}
	if blobSHA != "" {
		payload["sha"] = blobSHA
	}
	_, err := c.do(ctx, http.MethodPut, u, payload)
	return err
}

type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Title   string `json:"title"`
}

func (c *Client) CreatePR(ctx context.Context, head, base, title, body string) (*PullRequest, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/pulls", apiBase, c.owner, c.repo)
	payload := map[string]any{"title": title, "head": head, "base": base, "body": body}

	raw, err := c.do(ctx, http.MethodPost, u, payload)
	if err != nil {
		// A second run against an open PR for the same branch should surface the
		// existing PR rather than an error.
		if ae, ok := httpx.AsAPIError(err); ok && ae.Status == http.StatusUnprocessableEntity &&
			strings.Contains(ae.Body, "already exists") {
			return c.findPR(ctx, head)
		}
		return nil, err
	}
	var pr PullRequest
	if err := json.Unmarshal(raw, &pr); err != nil {
		return nil, fmt.Errorf("github: decode pr: %w", err)
	}
	return &pr, nil
}

func (c *Client) findPR(ctx context.Context, head string) (*PullRequest, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&head=%s:%s",
		apiBase, c.owner, c.repo, url.QueryEscape(c.owner), url.QueryEscape(head))
	raw, err := c.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	var prs []PullRequest
	if err := json.Unmarshal(raw, &prs); err != nil || len(prs) == 0 {
		return nil, fmt.Errorf("github: a PR for %s exists but could not be located", head)
	}
	return &prs[0], nil
}

// AddLabels is best-effort. Failing to label a PR must never fail the remediation.
func (c *Client) AddLabels(ctx context.Context, number int, labels ...string) {
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d/labels", apiBase, c.owner, c.repo, number)
	_, _ = c.do(ctx, http.MethodPost, u, map[string][]string{"labels": labels})
}

func (c *Client) do(ctx context.Context, method, u string, payload any) ([]byte, error) {
	if c.src == nil {
		return nil, fmt.Errorf("github is not configured (set GITHUB_APP_ID or GITHUB_TOKEN)")
	}
	token, err := c.src.Token(ctx)
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
		body := req.Body
		req.GetBody = func() (io.ReadCloser, error) { return body, nil }
	}
	return httpx.Do(c.http, req, "github")
}

func pathEscape(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
