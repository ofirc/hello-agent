// Package llm wraps Vertex AI for the one job the agent needs a model for: turning a
// pile of correlated facts into a ranked root-cause narrative.
//
// Two deliberate constraints:
//
//   - The model never calls a tool and never decides what to fetch. The providers
//     already gathered the evidence deterministically. The model only explains it. That
//     keeps the blast radius of a bad generation at "a confusing Slack message" rather
//     than "an unexpected API call against prod".
//   - If Vertex is not configured or errors, Summarize falls back to a deterministic
//     rendering. A demo must not go blank because a quota ran out.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

type Client struct {
	project  string
	location string
	model    string
	http     *http.Client
}

// New uses Application Default Credentials, which on a Cloud Run instance is the
// attached service account. No key file to create, store, rotate or leak, which is why
// many organisations disable service-account key creation entirely.
func New(ctx context.Context, project, location, model string) (*Client, error) {
	if project == "" {
		return nil, nil // not configured, callers fall back
	}
	ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("vertex: no application default credentials: %w", err)
	}
	return &Client{
		project: project, location: location, model: model,
		http: &http.Client{
			Timeout:   90 * time.Second,
			Transport: &oauth2.Transport{Source: ts, Base: http.DefaultTransport},
		},
	}, nil
}

// Generate sends one prompt and returns the text. No streaming: the Reporter already
// gives the user progress, and a partially streamed root cause is worse than a complete
// one two seconds later.
func (c *Client) Generate(ctx context.Context, system, user string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("vertex is not configured")
	}
	endpoint := fmt.Sprintf(
		"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
		c.location, c.project, c.location, c.model)

	payload := map[string]any{
		"systemInstruction": map[string]any{
			"parts": []map[string]string{{"text": system}},
		},
		"contents": []map[string]any{{
			"role":  "user",
			"parts": []map[string]string{{"text": user}},
		}},
		"generationConfig": map[string]any{
			"temperature":     0.2, // this is forensics, not prose
			"maxOutputTokens": 1024,
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("vertex: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("vertex: HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("vertex: decode response: %w", err)
	}
	if len(out.Candidates) == 0 {
		return "", fmt.Errorf("vertex: empty response, likely a safety block")
	}

	var sb strings.Builder
	for _, p := range out.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return "", fmt.Errorf("vertex: no text in response (finish reason %q)",
			out.Candidates[0].FinishReason)
	}
	return text, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
