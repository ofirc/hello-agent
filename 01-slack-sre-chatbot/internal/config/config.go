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

	// The model behind the agents. Required for drift debug and drift fix, which are
	// refused without one; every other command works without a model.
	// ModelProvider is "vertex", "ollama" or "" (no model). Unset with VERTEX_PROJECT set
	// means "vertex", so a Cloud Run deployment that predates MODEL_PROVIDER keeps working.
	ModelProvider  string
	VertexProject  string
	VertexLocation string
	VertexModel    string
	OllamaBaseURL  string // OpenAI-compatible endpoint, with the /v1
	OllamaModel    string

	// Where run_code executes: "cloudrun" (the Cloud Run sandbox CLI), "docker" (a
	// throwaway container from SandboxImage), "shim" (a local stand-in for the Cloud Run
	// CLI that just execs, with no isolation) or "auto", which picks Cloud Run or Docker,
	// whichever is present, and never the shim.
	SandboxMode  string
	SandboxImage string
	SandboxCLI   string // the Cloud Run CLI, or the shim standing in for it locally
	WorkDir      string // AGENT_WORKDIR; "" lets agentloop pick its default

	// Behaviour
	Environments   []string      // ordered, least to most production-like
	MaxConcurrent  int           // in-flight long jobs
	QuickTimeout   time.Duration // reads: version, drift list, rollout status
	TriageTimeout  time.Duration // drift debug
	RemedyTimeout  time.Duration // drift fix
	AllowFixEnvs   []string      // environments drift fix may target
	RequireApprove bool          // never merge, only open the PR
	// AuthorizedEmails may run the restricted commands (drift debug, drift fix, drift
	// apply, doctor), lowercased. Empty means nobody may: the gate fails closed.
	AuthorizedEmails []string
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
		ModelProvider:  os.Getenv("MODEL_PROVIDER"),
		OllamaBaseURL:  envOr("OLLAMA_BASE_URL", "http://localhost:11434/v1"),
		OllamaModel:    envOr("OLLAMA_MODEL", "sre-qwen3:14b"),
		SandboxMode:    envOr("SANDBOX", "auto"),
		SandboxImage:   envOr("SANDBOX_IMAGE", "sre-agent:local"),
		SandboxCLI:     envOr("SANDBOX_CLI", "/usr/local/gcp/bin/sandbox"),
		WorkDir:        os.Getenv("AGENT_WORKDIR"),
		Environments:   splitOr("ENVIRONMENTS", []string{"test", "stage", "prod"}),
		MaxConcurrent:  4,
		QuickTimeout:   20 * time.Second,
		TriageTimeout:  8 * time.Minute,
		RemedyTimeout:  4 * time.Minute,
		AllowFixEnvs:   splitOr("ALLOW_FIX_ENVS", []string{"stage", "prod"}),
		RequireApprove: envOr("REQUIRE_APPROVE", "true") != "false",
		// Lowercased here so the gate compares like with like; Slack does not
		// normalise the case of the email a user typed.
		AuthorizedEmails: lowerAll(splitOr("AUTHORIZED_EMAILS", nil)),
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
	switch c.ModelProvider {
	case "":
		if c.VertexProject != "" {
			c.ModelProvider = "vertex"
		}
	case "vertex":
		// Fail here rather than boot with "model configured" and fail to start the agent:
		// an explicit provider with no project is a typo, not a degraded mode.
		if c.VertexProject == "" {
			return nil, fmt.Errorf("MODEL_PROVIDER=vertex needs VERTEX_PROJECT")
		}
	case "ollama":
	default:
		return nil, fmt.Errorf("MODEL_PROVIDER must be vertex or ollama (got %q)", c.ModelProvider)
	}
	switch c.SandboxMode {
	case "auto", "cloudrun", "docker", "shim":
	default:
		return nil, fmt.Errorf("SANDBOX must be auto, cloudrun, docker or shim (got %q)", c.SandboxMode)
	}
	return c, nil
}

// ModelName is the configured provider's model, "" when there is no provider.
func (c *Config) ModelName() string {
	switch c.ModelProvider {
	case "vertex":
		return c.VertexModel
	case "ollama":
		return c.OllamaModel
	}
	return ""
}

// Enabled reports which integrations have credentials, for /sre help and startup logging.
func (c *Config) Enabled() map[string]bool {
	return map[string]bool{
		"argocd":  c.ArgoCDBaseURL != "" && c.ArgoCDToken != "",
		"github":  c.GitHubToken != "",
		"grafana": c.GrafanaBaseURL != "" && c.GrafanaToken != "",
		"jira":    c.JIRABaseURL != "" && c.JIRAToken != "",
		"model":   c.ModelProvider != "",
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

func lowerAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.ToLower(s))
	}
	return out
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
