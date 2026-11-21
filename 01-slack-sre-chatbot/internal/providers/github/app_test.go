package github

import (
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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testKey = func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}()

func TestParsePrivateKeyAcceptsGitHubsAndOpenSSLsForm(t *testing.T) {
	pkcs8, err := x509.MarshalPKCS8PrivateKey(testKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, block := range map[string]*pem.Block{
		"pkcs1": {Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey)},
		"pkcs8": {Type: "PRIVATE KEY", Bytes: pkcs8},
	} {
		k, err := ParsePrivateKey(pem.EncodeToMemory(block))
		if err != nil || !k.Equal(testKey) {
			t.Errorf("%s: ParsePrivateKey = %v, %v", name, k != nil, err)
		}
	}
	if _, err := ParsePrivateKey([]byte("SHA256:abc= is the fingerprint, not the key")); err == nil {
		t.Error("ParsePrivateKey accepted text that is not PEM")
	}
}

func TestAppFromConfig(t *testing.T) {
	if a, err := AppFromConfig("", "", "", "", "o", "r"); a != nil || err != nil {
		t.Errorf("unset GITHUB_APP_ID = %v, %v, want nil, nil", a, err)
	}
	if _, err := AppFromConfig("abc", "", "", "", "o", "r"); err == nil || !strings.Contains(err.Error(), "GITHUB_APP_ID") {
		t.Errorf("non-numeric app id: err = %v", err)
	}
	if _, err := AppFromConfig("1", "", "", "", "o", "r"); err == nil || !strings.Contains(err.Error(), "GITHUB_APP_PRIVATE_KEY_FILE") {
		t.Errorf("no key: err = %v, want it to name GITHUB_APP_PRIVATE_KEY_FILE", err)
	}
}

func TestJWTIsSignedForTheApp(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a := NewApp(123, 0, testKey, "o", "r")
	a.now = func() time.Time { return now }

	jwt, err := a.JWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testKey.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != "123" || claims.Iat >= now.Unix() || claims.Exp-now.Unix() > 600 {
		t.Errorf("claims = %+v, want iss 123, iat in the past, exp within GitHub's 10 minutes", claims)
	}
}

// fakeGitHub serves the two App endpoints a mint needs and counts the mints.
func fakeGitHub(t *testing.T, now func() time.Time) (*httptest.Server, *atomic.Int32) {
	var mints atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			t.Errorf("%s %s without a JWT", r.Method, r.URL.Path)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/installation":
			fmt.Fprint(w, `{"id":42}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/other/installation":
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			_ = json.Unmarshal(body, &req)
			if strings.Join(req.Repositories, ",") != "r" || req.Permissions["contents"] != "write" || req.Permissions["pull_requests"] != "write" {
				t.Errorf("mint asked for %s, want only repo r with contents and pull_requests write", body)
			}
			n := mints.Add(1)
			fmt.Fprintf(w, `{"token":"ghs_%d","expires_at":%q}`, n, now().Add(time.Hour).Format(time.RFC3339))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &mints
}

func TestTokenIsCachedAndRefreshedBeforeExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	srv, mints := fakeGitHub(t, clock)
	a := NewApp(1, 0, testKey, "o", "r")
	a.base, a.http, a.now = srv.URL, srv.Client(), clock

	for i := 0; i < 3; i++ {
		if tok, err := a.Token(context.Background()); err != nil || tok != "ghs_1" {
			t.Fatalf("Token = %q, %v, want ghs_1", tok, err)
		}
	}
	if mints.Load() != 1 {
		t.Errorf("%d mints for three calls inside the hour, want 1", mints.Load())
	}

	now = now.Add(56 * time.Minute) // inside refreshBefore of the expiry
	if tok, _ := a.Token(context.Background()); tok != "ghs_2" {
		t.Errorf("Token near expiry = %q, want a fresh ghs_2", tok)
	}
}

func TestMintNamesAMissingInstallation(t *testing.T) {
	srv, _ := fakeGitHub(t, time.Now)
	a := NewApp(1, 0, testKey, "o", "other")
	a.base, a.http = srv.URL, srv.Client()
	if _, err := a.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "not installed on o/other") {
		t.Errorf("err = %v, want it to say the App is not installed", err)
	}
}

type fakeSource struct {
	tok   string
	err   error
	calls int
}

func (f *fakeSource) Token(context.Context) (string, error) { f.calls++; return f.tok, f.err }
func (f *fakeSource) Identity() string                      { return "fake " + f.tok }

func TestFallbackCoolsDownAndRecovers(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	app := &fakeSource{tok: "app", err: errors.New("422 not covered")}
	pat := &fakeSource{tok: "pat"}
	f := WithFallback(app, pat, slog.New(slog.NewTextHandler(io.Discard, nil))).(*fallback)
	f.now = func() time.Time { return now }
	ctx := context.Background()

	if tok, _ := f.Token(ctx); tok != "pat" {
		t.Fatalf("failing App: Token = %q, want the PAT", tok)
	}
	if !strings.Contains(f.Identity(), "fake pat, because fake app failed") {
		t.Errorf("Identity while falling back = %q", f.Identity())
	}
	f.Token(ctx)
	if app.calls != 1 {
		t.Errorf("App tried %d times inside the cooldown, want 1", app.calls)
	}

	now = now.Add(fallbackCooldown + time.Second)
	app.err = nil
	if tok, _ := f.Token(ctx); tok != "app" {
		t.Errorf("after the cooldown with the App fixed: Token = %q, want app", tok)
	}
	if strings.Contains(f.Identity(), "because") {
		t.Errorf("Identity after recovery still reports the failure: %q", f.Identity())
	}
}

func TestFallbackDoesNotBlameTheAppForACancelledCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app := &fakeSource{err: context.Canceled}
	pat := &fakeSource{tok: "pat"}
	f := WithFallback(app, pat, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := f.Token(ctx); err == nil || pat.calls != 0 {
		t.Errorf("cancelled caller: err = %v, PAT calls = %d, want the error and no fallback", err, pat.calls)
	}
}
