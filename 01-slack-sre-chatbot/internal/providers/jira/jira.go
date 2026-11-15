// Package jira answers the other half of the triage question: has a human already
// noticed?
//
// A drift with an open customer ticket against it is a different priority from one
// nobody has reported, and the ticket usually describes the symptom far better than any
// metric does.
package jira

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/httpx"
)

type Client struct {
	base    string
	email   string
	token   string
	project string
	http    *http.Client
}

func New(base, email, token, project string) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"), email: email, token: token, project: project,
		http: httpx.Default(),
	}
}

func (c *Client) Configured() bool { return c.base != "" && c.token != "" }

type Issue struct {
	Key      string
	Summary  string
	Status   string
	Priority string
	Created  time.Time
	URL      string
}

func (i Issue) String() string {
	return fmt.Sprintf("<%s|%s> %s (%s)", i.URL, i.Key, i.Summary, i.Status)
}

// RecentForService finds tickets mentioning the service in the last `days` days.
//
// The text search is deliberately loose. A support ticket says "product catalog is
// missing the new filters", not "product-catalog@v1.2.0", so matching on the hyphenated
// service name alone would miss most of them.
func (c *Client) RecentForService(ctx context.Context, service string, days int, limit int) ([]Issue, error) {
	if !c.Configured() {
		return nil, fmt.Errorf("jira is not configured (set JIRA_BASE_URL, JIRA_EMAIL, JIRA_TOKEN)")
	}

	terms := []string{service}
	if spaced := strings.ReplaceAll(service, "-", " "); spaced != service {
		terms = append(terms, spaced)
	}
	var clauses []string
	for _, t := range terms {
		clauses = append(clauses, fmt.Sprintf(`text ~ %q`, t))
	}

	jql := fmt.Sprintf(`(%s) AND created >= -%dd ORDER BY created DESC`,
		strings.Join(clauses, " OR "), days)
	if c.project != "" {
		jql = fmt.Sprintf(`project = %q AND %s`, c.project, jql)
	}

	payload := map[string]any{
		"jql":        jql,
		"maxResults": limit,
		"fields":     []string{"summary", "status", "priority", "created"},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/rest/api/3/search/jql", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Basic "+basic(c.email, c.token))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }

	body, err := httpx.Do(c.http, req, "jira")
	if err != nil {
		return nil, err
	}

	var resp struct {
		Issues []struct {
			Key    string `json:"key"`
			Fields struct {
				Summary string `json:"summary"`
				Status  struct {
					Name string `json:"name"`
				} `json:"status"`
				Priority struct {
					Name string `json:"name"`
				} `json:"priority"`
				Created string `json:"created"`
			} `json:"fields"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("jira: decode search: %w", err)
	}

	out := make([]Issue, 0, len(resp.Issues))
	for _, it := range resp.Issues {
		created, _ := time.Parse("2006-01-02T15:04:05.000-0700", it.Fields.Created)
		out = append(out, Issue{
			Key:      it.Key,
			Summary:  it.Fields.Summary,
			Status:   it.Fields.Status.Name,
			Priority: it.Fields.Priority.Name,
			Created:  created,
			URL:      c.base + "/browse/" + it.Key,
		})
	}
	return out, nil
}

func basic(user, token string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
}
