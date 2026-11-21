// Package httpx is the one place that talks to a remote API, so timeouts, retries and
// error redaction are decided once instead of four times.
package httpx

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Default returns the client every provider should use. The timeout is per attempt and
// is deliberately shorter than the slash-command budget: a provider that hangs must not
// be the reason the whole triage blows its p99.
func Default() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        32,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// InsecureTransport skips TLS certificate verification.
//
// Exactly one caller is allowed to use this: the Argo CD provider, against a default
// install that serves a self-signed certificate on a private VPC address. It is a demo
// shortcut with a documented replacement, not a pattern to copy.
func InsecureTransport() *http.Transport {
	return &http.Transport{
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // see doc comment
	}
}

// APIError carries the status code so callers can distinguish "your token is wrong"
// from "the service is down", which matter very differently in a triage.
type APIError struct {
	Provider string
	Status   int
	Body     string
}

func (e *APIError) Error() string {
	switch {
	case e.Status == http.StatusUnauthorized, e.Status == http.StatusForbidden:
		return fmt.Sprintf("%s rejected our credentials (HTTP %d), the token is missing, expired or under-scoped", e.Provider, e.Status)
	case e.Status == http.StatusNotFound:
		return fmt.Sprintf("%s returned 404, check the base URL and the resource name", e.Provider)
	case e.Status == http.StatusTooManyRequests:
		return fmt.Sprintf("%s rate-limited us (HTTP 429)", e.Provider)
	default:
		return fmt.Sprintf("%s returned HTTP %d: %s", e.Provider, e.Status, e.Body)
	}
}

func IsAuth(err error) bool {
	ae, ok := AsAPIError(err)
	return ok && (ae.Status == 401 || ae.Status == 403)
}

// AsAPIError unwraps to an *APIError so callers can branch on the status code.
func AsAPIError(err error) (*APIError, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// Do executes the request, retrying idempotent calls on 429 and 5xx with a flat backoff.
// Flat rather than exponential on purpose: the caller already has a deadline, and three
// quick retries inside it beat one slow one.
func Do(c *http.Client, req *http.Request, provider string) ([]byte, error) {
	const attempts = 3

	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(time.Duration(i) * 700 * time.Millisecond):
			}
		}

		resp, err := c.Do(req.Clone(req.Context()))
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", provider, err)
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("%s: read body: %w", provider, readErr)
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return body, nil
		}

		apiErr := &APIError{Provider: provider, Status: resp.StatusCode, Body: snippet(body)}
		// 4xx other than 429 will not improve by asking again.
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return nil, apiErr
		}
		lastErr = apiErr

		// Only retry when the request body can actually be replayed.
		if req.Body != nil && req.GetBody == nil {
			return nil, apiErr
		}
	}
	return nil, lastErr
}

// snippet keeps error messages short and strips anything token-shaped, because these
// strings get posted into a Slack channel.
func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	for _, prefix := range []string{"xoxb-", "xapp-", "ghp_", "ghs_", "github_pat_", "Bearer "} {
		if i := strings.Index(s, prefix); i >= 0 {
			s = s[:i] + prefix + "[redacted]"
		}
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}
