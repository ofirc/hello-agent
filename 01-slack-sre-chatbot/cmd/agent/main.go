// Command agent is a ChatOps SRE bot: it answers deployment questions, finds release
// drift between environments, and opens pull requests to fix it.
//
// It connects to Slack over Socket Mode, which means the process dials out and holds a
// WebSocket open. There is no inbound listener, no public URL and no webhook signature
// to verify, which is what satisfies the "no open ports" requirement and is why it runs
// on a Cloud Run instance rather than a Cloud Run service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"google.golang.org/adk/v2/model"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/agentloop"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/commands"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/config"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/argocd"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/github"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/gitops"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/grafana"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/jira"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/sandbox"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/skills"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Cancel on SIGTERM so a Cloud Run instance restart closes the WebSocket cleanly
	// rather than leaving Slack to time it out.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	api := slack.New(cfg.SlackBotToken, slack.OptionAppLevelToken(cfg.SlackAppToken))

	// Fail fast and loudly on a bad bot token. Without this the process starts, the
	// socket connects, and every command fails with invalid_auth in a way that looks
	// like a scope problem.
	authCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	auth, err := api.AuthTestContext(authCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("slack auth.test failed, check SLACK_BOT_TOKEN: %w", err)
	}

	gh := newGitHub(ctx, cfg, log)

	deps := &commands.Deps{
		Cfg:     cfg,
		Slack:   api,
		Argo:    argocd.New(cfg.ArgoCDBaseURL, cfg.ArgoCDToken, cfg.ArgoCDProject, cfg.ArgoCDInsecure),
		GH:      gh,
		GitOps:  gitops.New(gh, cfg.GitOpsPrefix, cfg.GitHubBase),
		Grafana: grafana.New(cfg.GrafanaBaseURL, cfg.GrafanaToken, cfg.GrafanaDSUID),
		JIRA:    jira.New(cfg.JIRABaseURL, cfg.JIRAEmail, cfg.JIRAToken, cfg.JIRAProject),
		Log:     log,

		Proposals: agentloop.NewStore(30*time.Minute, time.Now),
		Emails:    commands.NewSlackEmails(api, 10*time.Minute),
	}
	if cfg.ModelProvider != "" {
		agent, err := newAgent(ctx, cfg, deps)
		if err != nil {
			// The rest of the bot is still useful, so this is not fatal. drift debug and
			// drift fix refuse with a pointer to the logs and /sre doctor.
			log.Warn("model unavailable ("+cfg.ModelProvider+"), drift debug and drift fix are disabled", "err", err)
		} else {
			deps.Agent = agent
		}
	}
	router := commands.NewRouter(deps)

	sandboxName := "none"
	if deps.Agent != nil && deps.Agent.SandboxName() != "" {
		sandboxName = deps.Agent.SandboxName()
	}
	enabled := cfg.Enabled()
	log.Info("starting",
		"bot", auth.User, "bot_id", auth.UserID, "team", auth.Team,
		"envs", strings.Join(cfg.Environments, ","),
		"github", enabled["github"], "argocd", enabled["argocd"],
		"grafana", enabled["grafana"], "jira", enabled["jira"],
		"model_provider", cfg.ModelProvider, "model", cfg.ModelName(),
		"sandbox", sandboxName,
		// A count, not the addresses: the log is not the place to list who may act.
		"authorized_emails", len(cfg.AuthorizedEmails),
	)
	if len(cfg.AuthorizedEmails) == 0 {
		log.Warn("AUTHORIZED_EMAILS is not set: drift debug, drift fix and doctor are refused for everyone")
	}

	// Cloud Run instances require a container that listens on a port once --ingress is
	// configured; the create fails with "Ingress settings require a single container
	// with a port". This agent is otherwise outbound-only, so the listener exists purely
	// to satisfy that contract and serves nothing but health.
	//
	// It is not an inbound control path. The instance runs with --ingress=internal and
	// the invoker IAM check on, so the port is unreachable from the internet, and no
	// Slack traffic ever arrives this way: commands come down the outbound WebSocket.
	go serveHealth(ctx, log)

	client := socketmode.New(api)
	go handleEvents(ctx, client, router, log)

	err = client.RunContext(ctx)
	if errors.Is(err, context.Canceled) {
		log.Info("shutting down")
		return nil
	}
	return err
}

// newGitHub picks who the agent writes to GitHub as: the App when it is configured,
// falling back to GITHUB_TOKEN, or the token alone. A broken App is never fatal, since
// the token may still work and the read-only commands need neither.
//
// The App is probed once here, so the startup log names the identity actually in use
// rather than the one configured, and doctor can name the bot user.
func newGitHub(ctx context.Context, cfg *config.Config, log *slog.Logger) *github.Client {
	var pat github.TokenSource
	if cfg.GitHubToken != "" {
		pat = github.StaticToken(cfg.GitHubToken)
	}
	app, err := github.AppFromConfig(cfg.GitHubAppID, cfg.GitHubAppInstallationID,
		cfg.GitHubAppPrivateKey, cfg.GitHubAppPrivateKeyFile, cfg.GitHubOwner, cfg.GitHubRepo)
	if err != nil {
		if pat != nil {
			log.Warn("github app misconfigured, using GITHUB_TOKEN", "err", err.Error())
		} else {
			log.Warn("github app misconfigured and GITHUB_TOKEN is not set, github is disabled", "err", err.Error())
		}
	}

	var src github.TokenSource
	switch {
	case app != nil && pat != nil:
		src = github.WithFallback(app, pat, log)
	case app != nil:
		src = app
	case pat != nil:
		src = pat
	}
	gh := github.New(src, cfg.GitHubOwner, cfg.GitHubRepo)

	if app != nil {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if _, err := app.Info(probeCtx); err != nil {
			log.Warn("github app: could not read the App, check GITHUB_APP_ID and the private key", "err", err.Error())
		}
		if _, err := src.Token(probeCtx); err != nil {
			log.Warn("github: no working credential", "err", err.Error())
		}
	}
	if src != nil {
		log.Info("github identity", "as", gh.Identity(), "repo", gh.Slug())
	}
	return gh
}

// newAgent builds the drift debug and fix agents over the same clients the commands use.
// The providers are closures, so the agent never holds a credential, and Facts goes
// through commands.Gather so the model sees exactly the table the static commands show.
func newAgent(ctx context.Context, cfg *config.Config, d *commands.Deps) (*agentloop.Agent, error) {
	var (
		m          model.LLM
		err        error
		answerTool bool
		endpoint   string
	)
	switch cfg.ModelProvider {
	case "vertex":
		m, err = agentloop.NewVertexModel(ctx, cfg.VertexProject, cfg.VertexLocation, cfg.VertexModel)
	case "ollama":
		// Non-Gemini models cannot take the debug output schema, so they finish through
		// the submit_triage tool, and their errors name the endpoint that failed.
		m, err = agentloop.NewOllamaModel(ctx, cfg.OllamaBaseURL, cfg.OllamaModel)
		answerTool = true
		endpoint = "ollama at " + cfg.OllamaBaseURL
	default:
		return nil, fmt.Errorf("unknown MODEL_PROVIDER %q", cfg.ModelProvider)
	}
	if err != nil {
		return nil, err
	}

	// The Docker probe and the runs must bind-mount the same directory, so the default
	// is resolved here once, to an absolute path, and handed to both.
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = filepath.Join(os.TempDir(), "sre-runs")
	}
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("create AGENT_WORKDIR %s: %w", workDir, err)
	}

	runner := sandbox.Select(cfg.SandboxMode, cfg.SandboxImage, workDir, cfg.SandboxCLI)
	if runner != nil && runner.Name() == "shim" {
		// Said once at startup, at warn level, because nothing else in the log would
		// show that run_code is running model code inside this process's container.
		d.Log.Warn("model code runs without isolation (SANDBOX=shim)", "cli", cfg.SandboxCLI)
	}
	if runner != nil && runner.Name() == "docker" && !runner.Available(ctx) {
		var probeErr error
		if p, ok := runner.(interface{ ProbeError() error }); ok {
			probeErr = p.ProbeError()
		}
		d.Log.Warn("docker sandbox unavailable",
			"err", probeErr,
			"hint", "check the Docker socket mount and that AGENT_WORKDIR is a host path shared with Docker",
			"workdir", workDir)
	}

	p := agentloop.Providers{
		Facts:   func(ctx context.Context) (drift.Table, error) { return commands.Gather(ctx, d) },
		Refresh: d.Argo.Refresh,
		// Read from the base branch on every call, never cached: the proposal records this
		// blob SHA, and drift apply refuses to write if base has moved past it.
		GetOverlay: func(ctx context.Context, env string) (*agentloop.Overlay, error) {
			f, err := d.GH.GetFile(ctx, d.GitOps.Path(env), cfg.GitHubBase)
			if err != nil || f == nil {
				return nil, err
			}
			return &agentloop.Overlay{Path: f.Path, Content: f.Content, SHA: f.SHA}, nil
		},
	}
	if d.Grafana.Configured() {
		p.Health = d.Grafana.ServiceHealth
	}
	if d.JIRA.Configured() {
		p.Tickets = d.JIRA.RecentForService
	}

	return agentloop.New(ctx, agentloop.Config{
		Model:         m,
		Sandbox:       runner,
		Skills:        skills.FS,
		Providers:     p,
		Envs:          cfg.Environments,
		AllowFixEnvs:  cfg.AllowFixEnvs,
		WorkDir:       workDir,
		AnswerTool:    answerTool,
		ModelEndpoint: endpoint,
	})
}

func handleEvents(ctx context.Context, client *socketmode.Client, router *commands.Router, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-client.Events:
			if !ok {
				return
			}
			switch evt.Type {

			case socketmode.EventTypeConnecting:
				log.Info("connecting to slack over socket mode")
			case socketmode.EventTypeConnected:
				log.Info("connected to slack with socket mode")
			case socketmode.EventTypeConnectionError:
				// The library reconnects on its own. Log and carry on, because a
				// transient disconnect is normal on a process that stays up for days.
				log.Warn("socket mode connection error, will retry", "data", fmt.Sprint(evt.Data))
			case socketmode.EventTypeInvalidAuth:
				log.Error("slack rejected the app-level token, check SLACK_APP_TOKEN and its connections:write scope")

			case socketmode.EventTypeSlashCommand:
				cmd, ok := evt.Data.(slack.SlashCommand)
				if !ok {
					continue
				}
				// Ack first, always, before doing anything that can block. Slack gives
				// a slash command three seconds, and a missed ack shows the user
				// "dispatch_failed" no matter how well the work goes afterwards.
				client.Ack(*evt.Request)

				go func() {
					defer recoverPanic(log, "slash command")
					router.Dispatch(ctx, cmd.ChannelID, cmd.UserID, cmd.UserName, cmd.Text)
				}()

			case socketmode.EventTypeInteractive:
				cb, ok := evt.Data.(slack.InteractionCallback)
				if !ok {
					continue
				}
				client.Ack(*evt.Request)

				go func() {
					defer recoverPanic(log, "interaction")
					router.HandleInteraction(ctx, cb)
				}()

			case socketmode.EventTypeEventsAPI:
				api, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					continue
				}
				client.Ack(*evt.Request)

				switch inner := api.InnerEvent.Data.(type) {
				case *slackevents.AppMentionEvent:
					go func() {
						defer recoverPanic(log, "app mention")
						router.HandleMention(ctx, inner.Channel, inner.User, inner.Text)
					}()

				case *slackevents.MessageEvent:
					// Direct messages only, and never the bot's own messages, which is
					// how a bot talks itself into an infinite loop.
					if inner.ChannelType != "im" || inner.BotID != "" || inner.User == "" {
						continue
					}
					go func() {
						defer recoverPanic(log, "direct message")
						router.HandleMention(ctx, inner.Channel, inner.User, inner.Text)
					}()
				}
			}
		}
	}
}

// serveHealth runs the liveness endpoint Cloud Run needs. Deliberately tiny: no
// routing, no business logic, nothing that could become an accidental control plane.
func serveHealth(ctx context.Context, log *slog.Logger) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("health endpoint listening", "port", port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("health endpoint failed", "err", err)
	}
}

// recoverPanic keeps one bad command from taking down a process that is meant to stay
// up for days at a time.
func recoverPanic(log *slog.Logger, where string) {
	if r := recover(); r != nil {
		log.Error("panic recovered", "where", where, "panic", fmt.Sprint(r))
	}
}
