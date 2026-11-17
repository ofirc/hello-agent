package agentloop

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewOllamaModelName(t *testing.T) {
	llm, err := NewOllamaModel(context.Background(), "http://localhost:11434/v1", "sre-qwen3:14b")
	if err != nil {
		t.Fatal(err)
	}
	if got := llm.Name(); got != "sre-qwen3:14b" {
		t.Fatalf("Name() = %q", got)
	}
}

// A stopped Ollama must be named in the error the user sees, not reported as a bare
// connection failure. The client retries twice with sub-second backoff, so no knob is
// needed to keep this fast; the deadline only bounds a regression.
func TestOllamaUnreachableNamesBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/v1"
	srv.Close()

	llm, err := NewOllamaModel(context.Background(), url, "sre-qwen3:14b")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := newTestAgent(t, llm, Providers{})
	a.cfg.AnswerTool = true
	a.cfg.ModelEndpoint = "ollama at " + url

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = a.Debug(ctx, debugRequest(&fakeProgress{}))
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("took over 10s: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "ollama at "+url+": ") {
		t.Fatalf("err = %v, want it to start with the endpoint", err)
	}
}
