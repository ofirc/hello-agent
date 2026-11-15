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
	"strings"
	"syscall"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/commands"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/config"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/llm"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/argocd"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/github"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/gitops"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/grafana"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/jira"
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

	gh := github.New(cfg.GitHubToken, cfg.GitHubOwner, cfg.GitHubRepo)

	vertex, err := llm.New(ctx, cfg.VertexProject, cfg.VertexLocation, cfg.VertexModel)
	if err != nil {
		// Not fatal. A triage without the narrative is still a triage.
		log.Warn("vertex unavailable, falling back to deterministic narratives", "err", err)
		vertex = nil
	}

	deps := &commands.Deps{
		Cfg:     cfg,
		Slack:   api,
		Argo:    argocd.New(cfg.ArgoCDBaseURL, cfg.ArgoCDToken, cfg.ArgoCDProject, cfg.ArgoCDInsecure),
		GH:      gh,
		GitOps:  gitops.New(gh, cfg.GitOpsPrefix, cfg.GitHubBase),
		Grafana: grafana.New(cfg.GrafanaBaseURL, cfg.GrafanaToken, cfg.GrafanaDSUID),
		JIRA:    jira.New(cfg.JIRABaseURL, cfg.JIRAEmail, cfg.JIRAToken, cfg.JIRAProject),
		LLM:     vertex,
		Log:     log,
	}
	router := commands.NewRouter(deps)

	enabled := cfg.Enabled()
	log.Info("starting",
		"bot", auth.User, "bot_id", auth.UserID, "team", auth.Team,
		"envs", strings.Join(cfg.Environments, ","),
		"github", enabled["github"], "argocd", enabled["argocd"],
		"grafana", enabled["grafana"], "jira", enabled["jira"], "vertex", enabled["vertex"],
	)

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
