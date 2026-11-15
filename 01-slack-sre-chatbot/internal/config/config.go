// Package config loads agent configuration from the environment.
//
// Nothing is read from disk and nothing is baked into the image. On Cloud Run the
// secret-backed values arrive via --set-secrets, which projects Secret Manager versions
// into the process environment.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	// Slack
	SlackBotToken string // xoxb-..., bot token
	SlackAppToken string // xapp-..., app-level token with connections:write

	// GitOps repo: the desired state.
	GitHubToken  string
	GitHubOwner  string
	GitHubRepo   string
	GitHubBase   string // base branch for fix PRs, e.g. "main"
	GitOpsPrefix string // path prefix of the overlays, e.g. "gitops/overlays"

	// Argo CD: the actual state.
	ArgoCDBaseURL  string
	ArgoCDToken    string
	ArgoCDProject  string
	ArgoCDInsecure bool // skip TLS verification, for Argo CD's default self-signed cert

	// Grafana: is the drift actually hurting anyone?
	GrafanaBaseURL string
	GrafanaToken   string
	GrafanaDSUID   string // Prometheus datasource UID to proxy queries through

	// JIRA: has a human already complained?
	JIRABaseURL string
	JIRAEmail   string
	JIRAToken   string
	JIRAProject string

	// Vertex AI: narrates the triage. Optional, there is a deterministic fallback.
	VertexProject  string
	VertexLocation string
	VertexModel    string

	// Behaviour
	Environments   []string      // ordered, least to most production-like
	MaxConcurrent  int           // in-flight long jobs
	QuickTimeout   time.Duration // reads: version, drift list, rollout status
	TriageTimeout  time.Duration // drift debug
	RemedyTimeout  time.Duration // drift fix
	AllowFixEnvs   []string      // environments drift fix may target
	RequireApprove bool          // never merge, only open the PR
}

func Load() (*Config, error) {
	c := &Config{
		SlackBotToken:  os.Getenv("SLACK_BOT_TOKEN"),
		SlackAppToken:  os.Getenv("SLACK_APP_TOKEN"),
		GitHubToken:    os.Getenv("GITHUB_TOKEN"),
		GitHubOwner:    envOr("GITHUB_OWNER", "ofirc"),
		GitHubRepo:     envOr("GITHUB_REPO", "hello-agent"),
		GitHubBase:     envOr("GITHUB_BASE_BRANCH", "main"),
		GitOpsPrefix:   envOr("GITOPS_PREFIX", "01-slack-sre-chatbot/gitops/overlays"),
		ArgoCDBaseURL:  strings.TrimRight(os.Getenv("ARGOCD_BASE_URL"), "/"),
		ArgoCDToken:    os.Getenv("ARGOCD_TOKEN"),
		ArgoCDProject:  envOr("ARGOCD_PROJECT", "default"),
		ArgoCDInsecure: envOr("ARGOCD_INSECURE", "true") != "false",
		GrafanaBaseURL: strings.TrimRight(os.Getenv("GRAFANA_BASE_URL"), "/"),
		GrafanaToken:   os.Getenv("GRAFANA_TOKEN"),
		GrafanaDSUID:   os.Getenv("GRAFANA_DATASOURCE_UID"),
		JIRABaseURL:    strings.TrimRight(os.Getenv("JIRA_BASE_URL"), "/"),
		JIRAEmail:      os.Getenv("JIRA_EMAIL"),
		JIRAToken:      os.Getenv("JIRA_TOKEN"),
		JIRAProject:    os.Getenv("JIRA_PROJECT"),
		VertexProject:  os.Getenv("VERTEX_PROJECT"),
		VertexLocation: envOr("VERTEX_LOCATION", "us-east1"),
		VertexModel:    envOr("VERTEX_MODEL", "gemini-2.5-pro"),
		Environments:   splitOr("ENVIRONMENTS", []string{"test", "stage", "prod"}),
		MaxConcurrent:  4,
		QuickTimeout:   20 * time.Second,
		TriageTimeout:  8 * time.Minute,
		RemedyTimeout:  4 * time.Minute,
		AllowFixEnvs:   splitOr("ALLOW_FIX_ENVS", []string{"stage", "prod"}),
		RequireApprove: envOr("REQUIRE_APPROVE", "true") != "false",
	}

	// Only Slack is non-negotiable. Every provider degrades to "not configured" so the
	// agent still boots and can tell you which integration is missing, which beats
	// crash-looping on a Cloud Run instance you then have to SSH into.
	var missing []string
	if c.SlackBotToken == "" {
		missing = append(missing, "SLACK_BOT_TOKEN")
	}
	if c.SlackAppToken == "" {
		missing = append(missing, "SLACK_APP_TOKEN")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env: %s", strings.Join(missing, ", "))
	}
	if !strings.HasPrefix(c.SlackAppToken, "xapp-") {
		return nil, fmt.Errorf("SLACK_APP_TOKEN must start with xapp- (got %q), Socket Mode needs the app-level token", truncate(c.SlackAppToken))
	}
	if !strings.HasPrefix(c.SlackBotToken, "xoxb-") {
		return nil, fmt.Errorf("SLACK_BOT_TOKEN must start with xoxb- (got %q)", truncate(c.SlackBotToken))
	}
	return c, nil
}

// Enabled reports which integrations have credentials, for /sre help and startup logging.
func (c *Config) Enabled() map[string]bool {
	return map[string]bool{
		"argocd":  c.ArgoCDBaseURL != "" && c.ArgoCDToken != "",
		"github":  c.GitHubToken != "",
		"grafana": c.GrafanaBaseURL != "" && c.GrafanaToken != "",
		"jira":    c.JIRABaseURL != "" && c.JIRAToken != "",
		"vertex":  c.VertexProject != "",
	}
}

func (c *Config) IsEnv(s string) bool {
	for _, e := range c.Environments {
		if e == s {
			return true
		}
	}
	return false
}

func (c *Config) CanFix(env string) bool {
	for _, e := range c.AllowFixEnvs {
		if e == env {
			return true
		}
	}
	return false
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitOr(k string, def []string) []string {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func truncate(s string) string {
	if len(s) > 8 {
		return s[:8] + "..."
	}
	return s
}
