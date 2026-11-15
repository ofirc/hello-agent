package commands

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/config"
)

func testRouter(t *testing.T) *Router {
	t.Helper()
	return NewRouter(&Deps{
		Cfg: &config.Config{
			Environments:  []string{"test", "stage", "prod"},
			AllowFixEnvs:  []string{"stage", "prod"},
			MaxConcurrent: 1,
			QuickTimeout:  10 * time.Second,
			TriageTimeout: time.Minute,
			RemedyTimeout: time.Minute,
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestResolveCanonicalCommands(t *testing.T) {
	r := testRouter(t)

	cases := map[string]struct{ noun, verb string }{
		"version get prod":            {"version", "get"},
		"version diff stage prod":     {"version", "diff"},
		"drift list":                  {"drift", "list"},
		"drift debug product-catalog": {"drift", "debug"},
		"drift fix product-catalog":   {"drift", "fix"},
		"rollout status checkout":     {"rollout", "status"},
		"service list":                {"service", "list"},
		"service get product-catalog": {"service", "get"},
		"doctor":                      {"doctor", ""},
		"help":                        {"help", ""},
	}

	for input, want := range cases {
		s, _, _, err := r.Resolve(input)
		if err != nil {
			t.Errorf("Resolve(%q) errored: %v", input, err)
			continue
		}
		if s.Noun != want.noun || s.Verb != want.verb {
			t.Errorf("Resolve(%q) = %s %s, want %s %s", input, s.Noun, s.Verb, want.noun, want.verb)
		}
	}
}

// The verb-first spellings anyone coming from kubectl will type by reflex, plus the
// original hyphenated set from the first iteration of the command surface.
func TestResolveAliases(t *testing.T) {
	r := testRouter(t)

	cases := map[string]struct{ noun, verb string }{
		"list services":               {"service", "list"},
		"get service product-catalog": {"service", "get"},
		"describe service checkout":   {"service", "get"},
		"get version prod":            {"version", "get"},
		"list drift":                  {"drift", "list"},
		"services":                    {"service", "list"},
		"debug-drift":                 {"drift", "debug"},
		"fix-drift product-catalog":   {"drift", "fix"},
		"versions":                    {"version", "get"},
		"status checkout":             {"rollout", "status"},
		"--help":                      {"help", ""},
	}

	for input, want := range cases {
		s, _, note, err := r.Resolve(input)
		if err != nil {
			t.Errorf("Resolve(%q) errored: %v", input, err)
			continue
		}
		if s.Noun != want.noun || s.Verb != want.verb {
			t.Errorf("Resolve(%q) = %s %s, want %s %s", input, s.Noun, s.Verb, want.noun, want.verb)
		}
		if note == "" {
			t.Errorf("Resolve(%q) returned no note, an alias should name the canonical form", input)
		}
		if !strings.Contains(note, "/sre") {
			t.Errorf("Resolve(%q) note %q does not mention the canonical /sre form", input, note)
		}
	}
}

// Longest match has to win. "get service product-catalog" must not resolve on "get"
// and lose its noun, and the trailing argument must survive the rewrite.
func TestTwoTokenAliasKeepsArguments(t *testing.T) {
	r := testRouter(t)

	s, args, _, err := r.Resolve("get service product-catalog")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Noun != "service" || s.Verb != "get" {
		t.Fatalf("resolved to %s %s, want service get", s.Noun, s.Verb)
	}
	if len(args) != 1 || args[0] != "product-catalog" {
		t.Errorf("args = %v, want [product-catalog]", args)
	}
}

func TestCanonicalCommandsGetNoAliasNote(t *testing.T) {
	r := testRouter(t)

	for _, input := range []string{"service list", "drift debug", "version get prod"} {
		if _, _, note, _ := r.Resolve(input); note != "" {
			t.Errorf("Resolve(%q) returned note %q, canonical commands should be silent", input, note)
		}
	}
}

func TestResolveRejectsUnknownInput(t *testing.T) {
	r := testRouter(t)

	for _, input := range []string{"frobnicate", "drift wibble", "nonsense noun verb"} {
		if _, _, _, err := r.Resolve(input); err == nil {
			t.Errorf("Resolve(%q) should have failed", input)
		}
	}
}

// A bare noun should list its verbs rather than guessing one.
func TestBareNounSuggestsVerbs(t *testing.T) {
	r := testRouter(t)

	_, _, _, err := r.Resolve("drift")
	if err == nil {
		t.Fatal("expected a usage error for a bare noun")
	}
	var ue *ErrUsage
	if !asUsage(err, &ue) {
		t.Fatalf("expected ErrUsage, got %T", err)
	}
	for _, verb := range []string{"debug", "fix", "list"} {
		if !strings.Contains(ue.Msg, verb) {
			t.Errorf("message %q does not offer the %q verb", ue.Msg, verb)
		}
	}
}

func TestEmptyInputIsHelp(t *testing.T) {
	r := testRouter(t)

	s, _, _, err := r.Resolve("   ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Noun != "help" {
		t.Errorf("bare /sre resolved to %q, want help", s.Noun)
	}
}

// Help is generated from the spec table, so a command cannot ship without appearing
// in it. This is the test that keeps that promise honest.
func TestHelpListsEveryCommand(t *testing.T) {
	r := testRouter(t)
	help := r.helpText()

	for _, s := range r.specs {
		if !strings.Contains(help, s.usage()) {
			t.Errorf("help does not mention %q", s.usage())
		}
	}
	if !strings.Contains(help, "/sre service list") {
		t.Error("help is missing the service commands")
	}
}

// Every alias must point at a command that exists. A typo in the alias table would
// otherwise fail only when a user typed that exact spelling.
func TestEveryAliasPointsAtARealCommand(t *testing.T) {
	r := testRouter(t)

	for alias, canon := range r.aliases {
		if _, ok := r.specs[canon]; !ok {
			t.Errorf("alias %q points at %q, which is not a command", alias, canon)
		}
	}
}
