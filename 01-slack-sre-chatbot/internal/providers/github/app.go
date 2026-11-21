package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/httpx"
)

// AppPermissions is everything an installation token is minted with, and nothing more.
// Asking for exactly these makes the mint itself the check: GitHub refuses it with a
// 422 if the installation lacks any of them or does not cover the repository, so a
// misconfigured App fails here, before a branch exists, rather than halfway through.
var AppPermissions = map[string]string{
	"metadata":      "read",
	"contents":      "write",
	"pull_requests": "write",
}

// refreshBefore is how long before expiry a cached installation token is replaced.
// Tokens last an hour; a request must not start with one that expires mid-flight.
const refreshBefore = 5 * time.Minute

// App authenticates as a GitHub App installation. Commits and PRs made with its tokens
// are attributed to the App's bot user, <slug>[bot], and the commits are signed by
// GitHub, because the contents API is called without a custom author or committer.
type App struct {
	id             int64
	installationID int64 // 0 means look it up from the repository
	key            *rsa.PrivateKey
	owner, repo    string
	http           *http.Client
	base           string
	now            func() time.Time

	mintMu  sync.Mutex // guards token and expires
	token   string
	expires time.Time

	mu   sync.Mutex // guards slug
	slug string
}

func NewApp(appID, installationID int64, key *rsa.PrivateKey, owner, repo string) *App {
	return &App{
		id: appID, installationID: installationID, key: key, owner: owner, repo: repo,
		http: httpx.Default(), base: apiBase, now: time.Now,
	}
}

// AppFromConfig builds the App from the GITHUB_APP_* settings. It returns nil and no
// error when appID is empty, meaning the App is not configured.
func AppFromConfig(appID, installationID, key, keyFile, owner, repo string) (*App, error) {
	if appID == "" {
		return nil, nil
	}
	id, err := ParseAppID("GITHUB_APP_ID", appID)
	if err != nil {
		return nil, err
	}
	inst, err := ParseAppID("GITHUB_APP_INSTALLATION_ID", installationID)
	if err != nil {
		return nil, err
	}
	k, err := LoadPrivateKey(key, keyFile)
	if err != nil {
		return nil, err
	}
	return NewApp(id, inst, k, owner, repo), nil
}

// ParseAppID reads GITHUB_APP_ID or GITHUB_APP_INSTALLATION_ID. Empty is 0, "not set".
func ParseAppID(name, v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive number, got %q", name, v)
	}
	return n, nil
}

// LoadPrivateKey reads the App's key from the environment or, failing that, from a file.
// The inline form is for Cloud Run, where Secret Manager projects it into the
// environment; the file form is for a laptop, where a multi-line value in .env is a trap.
func LoadPrivateKey(inline, file string) (*rsa.PrivateKey, error) {
	data := []byte(inline)
	if inline == "" {
		if file == "" {
			return nil, errors.New("set GITHUB_APP_PRIVATE_KEY_FILE to the .pem file the App downloaded, or GITHUB_APP_PRIVATE_KEY to its contents")
		}
		var err error
		if data, err = os.ReadFile(file); err != nil {
			return nil, fmt.Errorf("reading GITHUB_APP_PRIVATE_KEY_FILE: %w", err)
		}
	}
	return ParsePrivateKey(data)
}

// ParsePrivateKey accepts the PKCS#1 key GitHub downloads and the PKCS#8 form that
// `openssl pkcs8` converts it to.
func ParsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("the App private key is not PEM: expected a file starting with -----BEGIN RSA PRIVATE KEY-----")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the App private key is PEM but not an RSA key: %w", err)
	}
	k, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the App private key is a %T, GitHub Apps use RSA", parsed)
	}
	return k, nil
}

// JWT is the short-lived token that authenticates as the App itself. It can only call
// /app endpoints; everything else needs an installation token minted with it.
func (a *App) JWT() (string, error) {
	now := a.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		// Backdated, because GitHub rejects an iat that is in its future and clocks drift.
		"iat": now.Add(-60 * time.Second).Unix(),
		// GitHub's maximum is ten minutes.
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(a.id, 10),
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("github app: signing the JWT: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

type AppInfo struct {
	ID      int64  `json:"id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	HTMLURL string `json:"html_url"`
	Owner   struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// Info is the App this key belongs to. It also records the slug, so Identity can name
// the bot user rather than a number.
func (a *App) Info(ctx context.Context) (*AppInfo, error) {
	var info AppInfo
	if err := a.asApp(ctx, http.MethodGet, a.base+"/app", nil, &info); err != nil {
		return nil, fmt.Errorf("github app: reading the App: %w", err)
	}
	a.mu.Lock()
	a.slug = info.Slug
	a.mu.Unlock()
	return &info, nil
}

// InstallationID is GITHUB_APP_INSTALLATION_ID when set, otherwise the installation
// that covers the repository.
func (a *App) InstallationID(ctx context.Context) (int64, error) {
	if a.installationID != 0 {
		return a.installationID, nil
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/installation", a.base, url.PathEscape(a.owner), url.PathEscape(a.repo))
	if err := a.asApp(ctx, http.MethodGet, u, nil, &inst); err != nil {
		if ae, ok := httpx.AsAPIError(err); ok && ae.Status == http.StatusNotFound {
			return 0, fmt.Errorf("github app: the App is not installed on %s/%s", a.owner, a.repo)
		}
		return 0, fmt.Errorf("github app: finding the installation for %s/%s: %w", a.owner, a.repo, err)
	}
	return inst.ID, nil
}

type InstallationToken struct {
	Token               string            `json:"token"`
	ExpiresAt           time.Time         `json:"expires_at"`
	Permissions         map[string]string `json:"permissions"`
	RepositorySelection string            `json:"repository_selection"`
	Repositories        []struct {
		FullName string `json:"full_name"`
	} `json:"repositories"`
}

// Mint asks for a fresh installation token limited to the one repository and
// AppPermissions. Token is the cached form the client uses; Mint is for checking.
func (a *App) Mint(ctx context.Context) (*InstallationToken, error) {
	id, err := a.InstallationID(ctx)
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.base, id)
	payload := map[string]any{"repositories": []string{a.repo}, "permissions": AppPermissions}
	var tok InstallationToken
	if err := a.asApp(ctx, http.MethodPost, u, payload, &tok); err != nil {
		return nil, fmt.Errorf("github app: minting a token for installation %d: %w", id, err)
	}
	if tok.Token == "" {
		return nil, fmt.Errorf("github app: installation %d returned an empty token", id)
	}
	return &tok, nil
}

// Token returns a cached installation token, minting a new one when it is close to
// expiry. The lock is held across the mint on purpose: drift list reads every overlay
// in parallel, and they should share one mint, not race three.
func (a *App) Token(ctx context.Context) (string, error) {
	a.mintMu.Lock()
	defer a.mintMu.Unlock()
	if a.token != "" && a.now().Add(refreshBefore).Before(a.expires) {
		return a.token, nil
	}
	tok, err := a.Mint(ctx)
	if err != nil {
		return "", err
	}
	a.token, a.expires = tok.Token, tok.ExpiresAt
	return a.token, nil
}

// BotLogin is the user the App's commits and PRs are attributed to, "" until Info has
// run once.
func (a *App) BotLogin() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.slug == "" {
		return ""
	}
	return a.slug + "[bot]"
}

func (a *App) Identity() string {
	if bot := a.BotLogin(); bot != "" {
		return "GitHub App " + bot
	}
	return fmt.Sprintf("GitHub App %d", a.id)
}

// asApp calls an endpoint with the App's JWT. GitHub's explanation of a rejected JWT is
// the only useful part of a 401 here ("'Issued at' claim must be an Integer..."), so
// it is kept rather than replaced with the generic credentials message.
func (a *App) asApp(ctx context.Context, method, u string, payload, out any) error {
	jwt, err := a.JWT()
	if err != nil {
		return err
	}
	var body []byte
	if payload != nil {
		if body, err = json.Marshal(payload); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	raw, err := httpx.Do(a.http, req, "github")
	if err != nil {
		if ae, ok := httpx.AsAPIError(err); ok && ae.Body != "" {
			return fmt.Errorf("%w: %s", err, ae.Body)
		}
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
