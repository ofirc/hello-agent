package config

import (
	"strings"
	"testing"
)

// clearEnv blanks every variable the model and sandbox settings read, so a developer's
// shell cannot leak into the case under test. t.Setenv restores them afterwards.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"MODEL_PROVIDER", "VERTEX_PROJECT", "VERTEX_LOCATION", "VERTEX_MODEL",
		"OLLAMA_BASE_URL", "OLLAMA_MODEL", "SANDBOX", "SANDBOX_IMAGE", "SANDBOX_CLI", "AGENT_WORKDIR",
		"AUTHORIZED_EMAILS",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-x")
	t.Setenv("SLACK_APP_TOKEN", "xapp-x")
}

func TestModelProviderDefaults(t *testing.T) {
	t.Run("vertex is implied by VERTEX_PROJECT", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("VERTEX_PROJECT", "p")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.ModelProvider != "vertex" || c.ModelName() != "gemini-2.5-pro" {
			t.Errorf("got provider %q model %q", c.ModelProvider, c.ModelName())
		}
		if !c.Enabled()["model"] {
			t.Error(`Enabled()["model"] = false, want true`)
		}
	})

	t.Run("ollama", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("MODEL_PROVIDER", "ollama")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.OllamaBaseURL != "http://localhost:11434/v1" {
			t.Errorf("OllamaBaseURL = %q", c.OllamaBaseURL)
		}
		if c.OllamaModel != "sre-qwen3:14b" || c.ModelName() != "sre-qwen3:14b" {
			t.Errorf("OllamaModel = %q, ModelName = %q", c.OllamaModel, c.ModelName())
		}
		if !c.Enabled()["model"] {
			t.Error(`Enabled()["model"] = false, want true`)
		}
	})

	t.Run("neither", func(t *testing.T) {
		clearEnv(t)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.ModelProvider != "" || c.ModelName() != "" {
			t.Errorf("got provider %q model %q, want both empty", c.ModelProvider, c.ModelName())
		}
		en := c.Enabled()
		if en["model"] {
			t.Error(`Enabled()["model"] = true, want false`)
		}
		if _, ok := en["vertex"]; ok {
			t.Error(`Enabled() still has a "vertex" key`)
		}
	})
}

func TestModelProviderErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"vertex without project", map[string]string{"MODEL_PROVIDER": "vertex"}, []string{"needs VERTEX_PROJECT"}},
		{"unknown provider", map[string]string{"MODEL_PROVIDER": "openai"}, []string{"vertex", "ollama"}},
		{"unknown sandbox", map[string]string{"SANDBOX": "podman"}, []string{"auto"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load()
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

func TestSandboxAndWorkDirDefaults(t *testing.T) {
	clearEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.SandboxMode != "auto" || c.SandboxImage != "sre-agent:local" || c.WorkDir != "" {
		t.Errorf("got mode %q image %q workdir %q", c.SandboxMode, c.SandboxImage, c.WorkDir)
	}

	t.Setenv("AGENT_WORKDIR", "/var/tmp/runs")
	t.Setenv("SANDBOX", "docker")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkDir != "/var/tmp/runs" || c.SandboxMode != "docker" {
		t.Errorf("got workdir %q mode %q", c.WorkDir, c.SandboxMode)
	}
}

func TestAuthorizedEmails(t *testing.T) {
	clearEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AuthorizedEmails) != 0 {
		t.Errorf("unset AUTHORIZED_EMAILS = %q, want empty", c.AuthorizedEmails)
	}

	t.Setenv("AUTHORIZED_EMAILS", " A@x.io , b@y.io ,,")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.AuthorizedEmails, " ") != "a@x.io b@y.io" {
		t.Errorf("AuthorizedEmails = %q, want [a@x.io b@y.io]", c.AuthorizedEmails)
	}
}

func TestSandboxShimConfig(t *testing.T) {
	clearEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.SandboxCLI != "/usr/local/gcp/bin/sandbox" {
		t.Errorf("SandboxCLI default = %q", c.SandboxCLI)
	}

	t.Setenv("SANDBOX", "shim")
	t.Setenv("SANDBOX_CLI", "/x/shim")
	c, err = Load()
	if err != nil {
		t.Fatalf("SANDBOX=shim: %v", err)
	}
	if c.SandboxMode != "shim" || c.SandboxCLI != "/x/shim" {
		t.Errorf("got mode %q cli %q", c.SandboxMode, c.SandboxCLI)
	}

	t.Setenv("SANDBOX", "bogus")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "shim") {
		t.Errorf("SANDBOX=bogus error = %v, want it to name shim", err)
	}
}
