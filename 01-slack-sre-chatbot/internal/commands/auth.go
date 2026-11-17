package commands

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
)

// EmailResolver maps a Slack user to the email on their profile. An interface so the
// authorization tests do not need Slack.
type EmailResolver interface {
	Email(ctx context.Context, userID string) (string, error)
}

// NewSlackEmails resolves emails with users.info (scope users:read.email), caching
// each answer for ttl. Every restricted command would otherwise cost a Slack round
// trip, and a button press during an incident is not the time for a rate limit.
func NewSlackEmails(api *slack.Client, ttl time.Duration) EmailResolver {
	return newSlackEmails(api, ttl, time.Now)
}

func newSlackEmails(api *slack.Client, ttl time.Duration, now func() time.Time) *slackEmails {
	return &slackEmails{api: api, ttl: ttl, now: now, cache: map[string]cachedEmail{}}
}

type slackEmails struct {
	api *slack.Client
	ttl time.Duration
	now func() time.Time

	mu    sync.Mutex
	cache map[string]cachedEmail
}

type cachedEmail struct {
	email string
	at    time.Time
}

func (s *slackEmails) Email(ctx context.Context, userID string) (string, error) {
	s.mu.Lock()
	c, ok := s.cache[userID]
	s.mu.Unlock()
	if ok && s.now().Sub(c.at) < s.ttl {
		return c.email, nil
	}
	u, err := s.api.GetUserInfoContext(ctx, userID)
	if err != nil {
		// Not cached: a transient Slack error must not lock the owner out for ttl.
		return "", err
	}
	// The email is the only identity signal, so accounts whose email nobody here
	// vouches for are refused: bots, deactivated users, and users from another
	// organization in a shared channel, whose own admins set their email.
	switch {
	case u.IsBot:
		return "", errors.New("bot user")
	case u.Deleted:
		return "", errors.New("deactivated user")
	case u.IsStranger:
		return "", errors.New("user from another organization")
	}
	email := strings.TrimSpace(u.Profile.Email)
	if email == "" {
		// Not cached either: a missing users:read.email scope looks like this, and
		// reinstalling the app has to fix it at once.
		return "", errors.New("no email on the Slack profile")
	}
	s.mu.Lock()
	s.cache[userID] = cachedEmail{email: email, at: s.now()}
	s.mu.Unlock()
	return email, nil
}

// Reasons a restricted command is refused, as logged.
const (
	denyNoAllowlist  = "no_allowlist"
	denyNoResolver   = "no_resolver"
	denyLookupFailed = "lookup_failed"
	denyNotListed    = "not_listed"
)

// authorize decides whether userID may run s and returns why not, or "" when allowed.
// Open commands pass without a lookup. Restricted ones fail closed: no allowlist, no
// resolver, a lookup error or an empty email all refuse. The message is shown to the
// caller only, and never quotes their email, so a refusal reveals nothing about whose
// address is on the list.
func (r *Router) authorize(ctx context.Context, s spec, userID string) (reason, msg string) {
	if !s.Restricted {
		return "", ""
	}
	what := "`" + s.usage() + "`"
	if s.Hidden {
		// drift apply is only ever reached through its buttons.
		what = "the *Open this PR* and *Cancel* buttons"
	}
	allow := r.deps.Cfg.AuthorizedEmails
	if len(allow) == 0 {
		return denyNoAllowlist, "Only authorized users may use " + what + ", and nobody is authorized yet: AUTHORIZED_EMAILS is not set."
	}
	if r.deps.Emails == nil {
		return denyNoResolver, "I could not verify who you are, so you may not use " + what + "."
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	email, err := r.deps.Emails.Email(ctx, userID)
	if err == nil && strings.TrimSpace(email) == "" {
		err = errors.New("no email on the Slack profile")
	}
	if err != nil {
		r.deps.Log.Warn("authorization lookup failed", "user", userID, "err", err)
		return denyLookupFailed, "I could not verify your Slack email, so you may not use " + what + " yet. Try again in a minute."
	}
	if !slices.Contains(allow, strings.ToLower(strings.TrimSpace(email))) {
		return denyNotListed, fmt.Sprintf("You are not authorized to use %s.", what)
	}
	return "", ""
}
