package slackio

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/slack-go/slack"
)

func TestSnippetErrReportsAFailedUpload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"not_allowed_token_type"}`))
	}))
	defer srv.Close()

	r := NewReporter(slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/")), "C1", "U1")
	if err := r.SnippetErr(context.Background(), "a.diff", "diff", "content"); err == nil {
		t.Fatal("a rejected upload must be reported")
	}
	// Snippet keeps its fire-and-forget contract on the same failure.
	r.Snippet(context.Background(), "a.diff", "diff", "content")
}
