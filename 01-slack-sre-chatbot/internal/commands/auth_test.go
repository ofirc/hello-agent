package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/agentloop"
)

type fakeEmails struct {
	email string
	err   error
	calls int
}

func (f *fakeEmails) Email(context.Context, string) (string, error) {
	f.calls++
	return f.email, f.err
}

func authRouter(t *testing.T, allow []string, emails EmailResolver) *Router {
	t.Helper()
	r := testRouter(t)
	r.deps.Cfg.AuthorizedEmails = allow
	r.deps.Emails = emails
	return r
}

func TestAuthorizeOpenCommandNeedsNoLookup(t *testing.T) {
	f := &fakeEmails{err: errors.New("must not be called")}
	r := authRouter(t, nil, f)
	if reason, msg := r.authorize(context.Background(), r.specs["drift list"], "U1"); reason != "" {
		t.Errorf("drift list refused: %s %q", reason, msg)
	}
	if f.calls != 0 {
		t.Errorf("open command looked up the email %d times", f.calls)
	}
}

func TestAuthorizeAllowedIgnoresCaseAndSpaces(t *testing.T) {
	r := authRouter(t, []string{"ofir@example.com"}, &fakeEmails{email: "  Ofir@Example.COM "})
	if reason, msg := r.authorize(context.Background(), r.specs["drift fix"], "U1"); reason != "" {
		t.Errorf("allowed user refused: %s %q", reason, msg)
	}
}

func TestAuthorizeRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		allow   []string
		emails  *fakeEmails
		reason  string
		want    string
		lookups int
	}{
		"not on the list":  {[]string{"ofir@example.com"}, &fakeEmails{email: "eve@example.com"}, denyNotListed, "not authorized", 1},
		"lookup failed":    {[]string{"ofir@example.com"}, &fakeEmails{err: errors.New("users_not_found")}, denyLookupFailed, "could not verify", 1},
		"no email on file": {[]string{"ofir@example.com"}, &fakeEmails{email: ""}, denyLookupFailed, "could not verify", 1},
		"no allowlist":     {nil, &fakeEmails{email: "ofir@example.com"}, denyNoAllowlist, "AUTHORIZED_EMAILS", 0},
	} {
		r := authRouter(t, tc.allow, tc.emails)
		reason, msg := r.authorize(context.Background(), r.specs["drift debug"], "U1")
		if reason != tc.reason || !strings.Contains(msg, tc.want) {
			t.Errorf("%s: authorize = (%q, %q), want %q mentioning %q", name, reason, msg, tc.reason, tc.want)
		}
		if tc.emails.calls != tc.lookups {
			t.Errorf("%s: %d lookups, want %d", name, tc.emails.calls, tc.lookups)
		}
		if strings.Contains(msg, "eve@example.com") {
			t.Errorf("%s: refusal echoes the caller's email: %q", name, msg)
		}
	}
}

func TestAuthorizeWithoutResolverRefuses(t *testing.T) {
	r := authRouter(t, []string{"ofir@example.com"}, nil)
	if reason, _ := r.authorize(context.Background(), r.specs["doctor"], "U1"); reason != denyNoResolver {
		t.Errorf("reason = %q, want %q", reason, denyNoResolver)
	}
}

func TestAuthorizeNamesTheButtonsForDriftApply(t *testing.T) {
	r := authRouter(t, []string{"ofir@example.com"}, &fakeEmails{email: "eve@example.com"})
	_, msg := r.authorize(context.Background(), r.specs["drift apply"], "U1")
	if !strings.Contains(msg, "Open this PR") || strings.Contains(msg, "drift apply") {
		t.Errorf("refusal %q should name the buttons, not the hidden command", msg)
	}
}

// The confirm and cancel buttons route through drift apply, so it is gated like the
// commands that create proposals.
func TestRestrictedCommands(t *testing.T) {
	r := testRouter(t)
	want := map[string]bool{"drift debug": true, "drift fix": true, "drift apply": true, "doctor": true}
	for name, s := range r.specs {
		if s.Restricted != want[name] {
			t.Errorf("%s: Restricted = %v, want %v", name, s.Restricted, want[name])
		}
	}
}

func TestHelpMarksRestrictedCommands(t *testing.T) {
	help := testRouter(t).helpText()
	for _, line := range strings.Split(help, "\n") {
		restricted := strings.HasPrefix(line, "`/sre drift debug") ||
			strings.HasPrefix(line, "`/sre drift fix") || strings.HasPrefix(line, "`/sre doctor")
		open := strings.HasPrefix(line, "`/sre drift list") || strings.HasPrefix(line, "`/sre help")
		if restricted && !strings.Contains(line, "restricted") {
			t.Errorf("help line %q is not marked restricted", line)
		}
		if open && strings.Contains(line, "restricted") {
			t.Errorf("help line %q is marked restricted", line)
		}
	}
}

func TestSlackEmailsCachesSuccessOnly(t *testing.T) {
	var calls atomic.Int32
	var body atomic.Value
	ok := `{"ok":true,"user":{"id":"U1","profile":{"email":"ofir@example.com"}}}`
	body.Store(ok)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	defer srv.Close()

	now := time.Unix(0, 0)
	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	e := newSlackEmails(api, 10*time.Minute, func() time.Time { return now })
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		got, err := e.Email(ctx, "U1")
		if err != nil || got != "ofir@example.com" {
			t.Fatalf("Email = (%q, %v)", got, err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("%d users.info calls within the TTL, want 1", calls.Load())
	}

	// Each of these is refused and then fixed. None may be cached, or the fix would not
	// be seen until the TTL ran out.
	for name, failing := range map[string]string{
		"slack error": `{"ok":false,"error":"ratelimited"}`,
		"no email":    `{"ok":true,"user":{"id":"U1","profile":{"email":""}}}`,
		"bot":         `{"ok":true,"user":{"id":"U1","is_bot":true,"profile":{"email":"ofir@example.com"}}}`,
		"deactivated": `{"ok":true,"user":{"id":"U1","deleted":true,"profile":{"email":"ofir@example.com"}}}`,
		"stranger":    `{"ok":true,"user":{"id":"U1","is_stranger":true,"profile":{"email":"ofir@example.com"}}}`,
	} {
		now = now.Add(11 * time.Minute)
		body.Store(failing)
		before := calls.Load()
		if got, err := e.Email(ctx, "U1"); err == nil {
			t.Errorf("%s: Email = %q, want an error", name, got)
		}
		body.Store(ok)
		if got, err := e.Email(ctx, "U1"); err != nil || got != "ofir@example.com" {
			t.Errorf("%s: after the fix, Email = (%q, %v): the refusal must not be cached", name, got, err)
		}
		if n := calls.Load() - before; n != 2 {
			t.Errorf("%s: %d users.info calls, want 2", name, n)
		}
	}
}

// fakeSlack records the Slack API calls the router makes, with the user each one is
// about, and answers users.info from emails.
type fakeSlack struct {
	mu     sync.Mutex
	calls  []string
	emails map[string]string
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	_ = req.ParseForm()
	method := strings.TrimPrefix(req.URL.Path, "/")
	f.mu.Lock()
	f.calls = append(f.calls, method+" "+req.Form.Get("user"))
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if method == "users.info" {
		u := req.Form.Get("user")
		fmt.Fprintf(w, `{"ok":true,"user":{"id":%q,"profile":{"email":%q}}}`, u, f.emails[u])
		return
	}
	_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"1.0","message_ts":"1.0"}`))
}

func (f *fakeSlack) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// dispatchRouter is a router wired to a fake Slack, with a pending proposal requested
// by the allowlisted owner.
func dispatchRouter(t *testing.T) (*Router, *fakeSlack, string) {
	t.Helper()
	fs := &fakeSlack{emails: map[string]string{"UOWNER": "ofir@example.com", "UEVE": "eve@example.com"}}
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))

	r := authRouter(t, []string{"ofir@example.com"}, newSlackEmails(api, 10*time.Minute, time.Now))
	r.deps.Slack = api
	r.deps.Proposals = agentloop.NewStore(30*time.Minute, nil)
	id := r.deps.Proposals.Put(agentloop.Proposal{Requester: "UOWNER", Channel: "C1", Env: "prod", Service: "checkout"})
	return r, fs, id
}

func applyClick(user, actionID, value string) slack.InteractionCallback {
	var cb slack.InteractionCallback
	cb.Type = slack.InteractionTypeBlockActions
	cb.Channel.ID = "C1"
	cb.User.ID = user
	cb.ActionCallback.BlockActions = []*slack.BlockAction{{ActionID: actionID, Value: value}}
	return cb
}

// Refused before backpressure, the anchor message or the proposal store: Slack hears
// only the lookup and the private refusal, both about the actor, not the requester.
func assertRefusedBeforeAnyWork(t *testing.T, r *Router, fs *fakeSlack, id string) {
	t.Helper()
	want := []string{"users.info UEVE", "chat.postEphemeral UEVE"}
	if got := fs.recorded(); !slices.Equal(got, want) {
		t.Errorf("Slack calls = %q, want %q", got, want)
	}
	if len(r.sem) != 0 {
		t.Errorf("refused command holds %d backpressure slots", len(r.sem))
	}
	if _, ok := r.deps.Proposals.Get(id); !ok {
		t.Error("refused click consumed the proposal")
	}
}

func TestDispatchRefusesButtonClickByUnlistedUser(t *testing.T) {
	for _, action := range []string{"drift_apply", "drift_cancel"} {
		t.Run(action, func(t *testing.T) {
			r, fs, id := dispatchRouter(t)
			r.HandleInteraction(context.Background(), applyClick("UEVE", action, id))
			assertRefusedBeforeAnyWork(t, r, fs, id)
		})
	}
}

func TestDispatchChecksTheSenderNotAMentionedUser(t *testing.T) {
	r, fs, id := dispatchRouter(t)
	r.HandleMention(context.Background(), "C1", "UEVE", "<@UBOT> drift debug <@UOWNER>")
	assertRefusedBeforeAnyWork(t, r, fs, id)
}

func TestDispatchLetsTheOwnerThrough(t *testing.T) {
	r, fs, id := dispatchRouter(t)
	r.HandleInteraction(context.Background(), applyClick("UOWNER", "drift_cancel", id))
	got := fs.recorded()
	if len(got) == 0 || got[0] != "users.info UOWNER" {
		t.Fatalf("Slack calls = %q, want a users.info lookup for the owner first", got)
	}
	for _, c := range got {
		if strings.HasPrefix(c, "chat.postEphemeral") {
			t.Errorf("owner was refused: Slack calls = %q", got)
		}
	}
	if len(got) < 2 {
		t.Errorf("Slack calls = %q: nothing happened past the gate", got)
	}
}
