// Command ghcheck tells you whether the GitHub settings in your environment work, and
// whose name the agent's fix PRs will carry, without starting the agent or Slack.
//
//	set -a && source .env && set +a && go run ./cmd/ghcheck
//	go run ./cmd/ghcheck -write     # also create and delete a scratch branch
//
// It checks the GitHub App (GITHUB_APP_*) and the personal access token (GITHUB_TOKEN)
// independently, through the same client code the agent uses, and exits non-zero when
// neither would let the agent read the overlays (or, with -write, push a branch).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/github"
)

const apiBase = "https://api.github.com"

// Defaults match internal/config, which cannot be used here: it insists on Slack tokens.
var (
	owner   = envOr("GITHUB_OWNER", "ofirc")
	repo    = envOr("GITHUB_REPO", "hello-agent")
	base    = envOr("GITHUB_BASE_BRANCH", "main")
	prefix  = strings.Trim(envOr("GITOPS_PREFIX", "01-slack-sre-chatbot/gitops/overlays"), "/")
	envs    = strings.Split(envOr("ENVIRONMENTS", "test,stage,prod"), ",")
	overlay = fmt.Sprintf("%s/%s/kustomization.yaml", prefix, strings.TrimSpace(envs[len(envs)-1]))
)

func main() {
	write := flag.Bool("write", false, "also prove write access by creating and deleting a scratch branch")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fmt.Printf("GitHub credentials for %s/%s (base %s)\n", owner, repo, base)

	appBot, appOK := checkApp(ctx, *write)
	patUser, patOK := checkPAT(ctx, *write)

	fmt.Println()
	switch {
	case appOK && patOK:
		fmt.Printf("Result: fix PRs will be opened by %s. If the App ever fails to mint a token, the agent falls back to the token and they are opened as %s.\n", appBot, patUser)
	case appOK:
		fmt.Printf("Result: fix PRs will be opened by %s. There is no fallback: GITHUB_TOKEN is unset or not working.\n", appBot)
	case patOK && os.Getenv("GITHUB_APP_ID") != "":
		fmt.Printf("Result: the App does not work, so the agent falls back to the token and fix PRs are opened as %s. Fix the App items marked FAIL above.\n", patUser)
	case patOK:
		fmt.Printf("Result: fix PRs will be opened as %s, with the personal access token. Set GITHUB_APP_* to open them as a bot.\n", patUser)
	default:
		fmt.Println("Result: no working GitHub credential. The agent would start with github disabled.")
		os.Exit(1)
	}
}

// checkApp walks the App setup in the order it can break, so the first FAIL is the
// thing to fix. It returns the bot login and whether the App is fully usable.
func checkApp(ctx context.Context, write bool) (string, bool) {
	fmt.Println("\nGitHub App (GITHUB_APP_ID, GITHUB_APP_PRIVATE_KEY_FILE, GITHUB_APP_INSTALLATION_ID)")
	appID := strings.TrimSpace(os.Getenv("GITHUB_APP_ID"))
	if appID == "" {
		skip("not configured", "GITHUB_APP_ID is not set")
		return "", false
	}

	keyFile := os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE")
	keyFrom := keyFile
	if os.Getenv("GITHUB_APP_PRIVATE_KEY") != "" {
		keyFrom = "GITHUB_APP_PRIVATE_KEY"
	}
	app, err := github.AppFromConfig(appID, strings.TrimSpace(os.Getenv("GITHUB_APP_INSTALLATION_ID")),
		os.Getenv("GITHUB_APP_PRIVATE_KEY"), keyFile, owner, repo)
	if err != nil {
		fail("settings", err)
		return "", false
	}
	pass("private key", "read from "+keyFrom)

	info, err := app.Info(ctx)
	if err != nil {
		fail("app", fmt.Errorf("%w\n      404: no App has this GITHUB_APP_ID. 401: the key belongs to a different App, or this machine's clock is off", err))
		return "", false
	}
	pass("app", fmt.Sprintf("%q (slug %s), owned by %s, %s", info.Name, info.Slug, info.Owner.Login, info.HTMLURL))

	id, err := app.InstallationID(ctx)
	if err != nil {
		fail("installation", fmt.Errorf("%w\n      install it: %s/installations/new, Only select repositories, %s", err, info.HTMLURL, repo))
		return "", false
	}
	if os.Getenv("GITHUB_APP_INSTALLATION_ID") == "" {
		pass("installation", fmt.Sprintf("%d, found from the repository (set GITHUB_APP_INSTALLATION_ID=%d to skip the lookup)", id, id))
	} else {
		pass("installation", fmt.Sprintf("%d", id))
	}

	tok, err := app.Mint(ctx)
	if err != nil {
		fail("token", fmt.Errorf("%w\n      a 422 means the installation does not cover %s or lacks one of %s: App settings, Permissions, then accept the change on the installation", err, repo, permList(github.AppPermissions)))
		return "", false
	}
	pass("token", fmt.Sprintf("expires %s, granted %s", tok.ExpiresAt.Local().Format("15:04"), permList(tok.Permissions)))

	bot := info.Slug + "[bot]"
	var botUser struct {
		ID int64 `json:"id"`
	}
	if err := get(ctx, tok.Token, "/users/"+url.PathEscape(bot), &botUser); err == nil {
		pass("bot user", fmt.Sprintf("%s, commits as %d+%s@users.noreply.github.com", bot, botUser.ID, bot))
	} else {
		warn("bot user", fmt.Sprintf("%s not found yet (%v), it appears once the App is installed", bot, err))
	}

	if !checkAccess(ctx, github.New(staticOf(tok.Token, app.Identity()), owner, repo), tok.Token, write) {
		return bot, false
	}
	return bot, true
}

func checkPAT(ctx context.Context, write bool) (string, bool) {
	fmt.Println("\nPersonal access token (GITHUB_TOKEN)")
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		skip("not configured", "GITHUB_TOKEN is not set")
		return "", false
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := get(ctx, token, "/user", &user); err != nil {
		fail("user", fmt.Errorf("%w\n      the token is wrong, expired or revoked", err))
		return "", false
	}
	pass("user", user.Login+", the name its commits and PRs carry")
	if !checkAccess(ctx, github.New(github.StaticToken(token), owner, repo), token, write) {
		return user.Login, false
	}
	return user.Login, true
}

// checkAccess reads the prod overlay the way drift list does and, with write, proves
// the token can push by creating a branch and deleting it again.
func checkAccess(ctx context.Context, gh *github.Client, token string, write bool) bool {
	f, err := gh.GetFile(ctx, overlay, base)
	switch {
	case err != nil:
		fail("read", err)
		return false
	case f == nil:
		fail("read", fmt.Errorf("%s does not exist on %s, check GITOPS_PREFIX and GITHUB_BASE_BRANCH", overlay, base))
		return false
	}
	pass("read", overlay)

	if !write {
		skip("write", "not checked, rerun with -write to create and delete a scratch branch")
		return true
	}
	head, err := gh.HeadSHA(ctx, base)
	if err != nil {
		fail("write", err)
		return false
	}
	branch := fmt.Sprintf("sre-agent/ghcheck-%d", time.Now().Unix())
	if err := gh.CreateBranch(ctx, branch, head); err != nil {
		fail("write", fmt.Errorf("creating %s: %w", branch, err))
		return false
	}
	if err := del(ctx, token, "/repos/"+owner+"/"+repo+"/git/refs/heads/"+branch); err != nil {
		warn("write", fmt.Sprintf("created %s but could not delete it (%v), delete it by hand", branch, err))
		return true
	}
	pass("write", "created and deleted "+branch)
	return true
}

// staticOf wraps an already-minted installation token, so the reads above use exactly
// the token Mint reported on rather than minting a second one.
func staticOf(token, identity string) github.TokenSource { return minted{token, identity} }

type minted struct{ token, identity string }

func (m minted) Token(context.Context) (string, error) { return m.token, nil }
func (m minted) Identity() string                      { return m.identity }

func get(ctx context.Context, token, path string, out any) error {
	return call(ctx, http.MethodGet, token, path, out)
}

func del(ctx context.Context, token, path string) error {
	return call(ctx, http.MethodDelete, token, path, nil)
}

func call(ctx context.Context, method, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Message)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func permList(p map[string]string) string {
	parts := make([]string, 0, len(p))
	for k, v := range p {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func pass(what, detail string) { fmt.Printf("  PASS  %-13s %s\n", what, detail) }
func warn(what, detail string) { fmt.Printf("  WARN  %-13s %s\n", what, detail) }
func skip(what, detail string) { fmt.Printf("  SKIP  %-13s %s\n", what, detail) }
func fail(what string, err error) {
	fmt.Printf("  FAIL  %-13s %v\n", what, err)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
