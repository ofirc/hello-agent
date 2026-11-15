package main

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// The Slack app manifest is pasted into a web form by hand, which means nothing else
// checks it. A missing scope does not fail the build, it fails at runtime with a
// missing_scope error in the middle of a demo, and fixing it needs a reinstall rather
// than a redeploy.
//
// These tests tie manifest.yaml to what the code actually calls, so adding an API call
// without the scope it needs fails here instead of on stage.
const manifestPath = "../../manifest.yaml"

type manifest struct {
	DisplayInformation struct {
		Name string `yaml:"name"`
	} `yaml:"display_information"`
	Features struct {
		BotUser struct {
			DisplayName  string `yaml:"display_name"`
			AlwaysOnline bool   `yaml:"always_online"`
		} `yaml:"bot_user"`
		SlashCommands []struct {
			Command     string `yaml:"command"`
			Description string `yaml:"description"`
			URL         string `yaml:"url"`
		} `yaml:"slash_commands"`
	} `yaml:"features"`
	OAuthConfig struct {
		Scopes struct {
			Bot []string `yaml:"bot"`
		} `yaml:"scopes"`
	} `yaml:"oauth_config"`
	Settings struct {
		EventSubscriptions struct {
			BotEvents []string `yaml:"bot_events"`
		} `yaml:"event_subscriptions"`
		Interactivity struct {
			IsEnabled bool `yaml:"is_enabled"`
		} `yaml:"interactivity"`
		SocketModeEnabled bool `yaml:"socket_mode_enabled"`
	} `yaml:"settings"`
}

func load(t *testing.T) manifest {
	t.Helper()
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("reading %s: %v", manifestPath, err)
	}
	var m manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatalf("manifest.yaml is not valid YAML, Slack will reject the paste: %v", err)
	}
	return m
}

// Every scope here backs a call the agent actually makes. Removing one from the
// manifest without removing the call is the bug this catches.
func TestManifestHasEveryScopeTheCodeNeeds(t *testing.T) {
	required := map[string]string{
		"commands":          "the /sre slash command",
		"chat:write":        "Reporter.Start posts the anchor message",
		"chat:write.public": "posting into channels the bot was not invited to",
		"files:write":       "Reporter.Snippet uploads long diffs",
		"app_mentions:read": "Router.HandleMention",
		"im:history":        "direct messages reach the same router",
		"users:read":        "resolving the caller for the audit trail",
		"reactions:write":   "progress signalling on long triages",
	}

	have := set(load(t).OAuthConfig.Scopes.Bot)
	for scope, why := range required {
		if !have[scope] {
			t.Errorf("manifest is missing bot scope %q, needed for %s", scope, why)
		}
	}
}

// main.go subscribes to these. A handler with no matching subscription is dead code
// that looks like it works.
func TestManifestSubscribesToEveryEventTheLoopHandles(t *testing.T) {
	required := []string{"app_mention", "message.im"}

	have := set(load(t).Settings.EventSubscriptions.BotEvents)
	for _, evt := range required {
		if !have[evt] {
			t.Errorf("manifest does not subscribe to %q, but handleEvents switches on it", evt)
		}
	}
}

// Socket Mode is the architectural decision, not a preference. Turning it off would
// require a public HTTPS endpoint, which breaks the no-open-ports requirement and makes
// a Cloud Run instance the wrong host.
func TestManifestUsesSocketMode(t *testing.T) {
	m := load(t)

	if !m.Settings.SocketModeEnabled {
		t.Error("socket_mode_enabled is false, but the agent only speaks Socket Mode")
	}
	for _, c := range m.Features.SlashCommands {
		if c.URL != "" {
			t.Errorf("slash command %s declares url %q, which only applies to request-URL apps",
				c.Command, c.URL)
		}
	}
}

// drift debug ends in buttons and drift fix posts a review link, both of which arrive
// as interactive payloads.
func TestManifestEnablesInteractivity(t *testing.T) {
	if !load(t).Settings.Interactivity.IsEnabled {
		t.Error("interactivity is disabled, so the buttons in drift debug and drift fix will not work")
	}
}

func TestManifestDeclaresTheCiCommand(t *testing.T) {
	m := load(t)

	var found bool
	for _, c := range m.Features.SlashCommands {
		if c.Command == "/sre" {
			found = true
			if c.Description == "" {
				t.Error("/sre has no description, Slack requires one")
			}
		}
	}
	if !found {
		t.Error("manifest does not declare /sre")
	}
	if m.Features.BotUser.DisplayName == "" {
		t.Error("bot_user.display_name is empty")
	}
}

func set(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}
