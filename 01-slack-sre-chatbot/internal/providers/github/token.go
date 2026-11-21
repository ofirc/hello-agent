package github

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// TokenSource supplies the bearer token for each request. Identity says, for doctor and
// the startup log, whose name the agent's commits and PRs will carry.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Identity() string
}

type staticToken string

// StaticToken is a personal access token, sent as is on every request. Commits and PRs
// made with it carry the name of the person who created the token.
func StaticToken(token string) TokenSource { return staticToken(token) }

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }
func (staticToken) Identity() string                        { return "personal access token" }

// fallbackCooldown is how long a failing primary is left alone before it is tried again.
// Without it every request, including the three parallel overlay reads behind drift
// list, would spend a round trip on a mint that is known to fail.
const fallbackCooldown = time.Minute

type fallback struct {
	primary, secondary TokenSource
	log                *slog.Logger
	now                func() time.Time

	mu      sync.Mutex
	retryAt time.Time
	lastErr error
}

// WithFallback uses primary, and secondary whenever primary cannot produce a token.
//
// The switch happens when a token is minted, never after a request was rejected, so one
// change is never split across two identities: a branch made as the App and a PR opened
// as a person. The switch, and the recovery, are each logged once.
func WithFallback(primary, secondary TokenSource, log *slog.Logger) TokenSource {
	return &fallback{primary: primary, secondary: secondary, log: log, now: time.Now}
}

func (f *fallback) Token(ctx context.Context) (string, error) {
	f.mu.Lock()
	cooling := f.now().Before(f.retryAt)
	f.mu.Unlock()
	if cooling {
		return f.secondary.Token(ctx)
	}

	tok, err := f.primary.Token(ctx)
	if err != nil && ctx.Err() != nil {
		// The caller gave up. That says nothing about the primary.
		return "", err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		if f.lastErr != nil {
			f.log.Info("github: "+f.primary.Identity()+" works again, no longer falling back", "was", f.lastErr.Error())
			f.lastErr = nil
		}
		return tok, nil
	}
	if f.lastErr == nil {
		f.log.Warn("github: "+f.primary.Identity()+" failed, falling back to the "+f.secondary.Identity(),
			"err", err.Error(), "retry_in", fallbackCooldown.String())
	}
	f.lastErr = err
	f.retryAt = f.now().Add(fallbackCooldown)
	return f.secondary.Token(ctx)
}

func (f *fallback) Identity() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastErr != nil {
		return f.secondary.Identity() + ", because " + f.primary.Identity() + " failed: " + f.lastErr.Error()
	}
	return f.primary.Identity() + ", falling back to the " + f.secondary.Identity()
}
