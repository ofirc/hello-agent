package commands

import (
	"context"
	"strings"

	"github.com/slack-go/slack"
)

// HandleInteraction turns a button click back into a command.
//
// Buttons deliberately route through the same Dispatch path as typed commands rather
// than calling handlers directly. One code path means one place where authorisation,
// backpressure, timeouts and audit logging happen, and it makes every button press show
// up in the log looking exactly like the command a human could have typed.
func (r *Router) HandleInteraction(ctx context.Context, cb slack.InteractionCallback) {
	if cb.Type != slack.InteractionTypeBlockActions || len(cb.ActionCallback.BlockActions) == 0 {
		return
	}
	action := cb.ActionCallback.BlockActions[0]

	channel := cb.Channel.ID
	user := cb.User.ID
	name := cb.User.Name

	var text string
	switch action.ActionID {
	case "drift_fix":
		text = "drift fix " + action.Value // value is "<service> <env>"
	case "drift_recheck":
		text = "drift list"
		if svc := strings.TrimSpace(action.Value); svc != "" {
			text = "rollout status " + svc
		}
	case "view_pr", "open_grafana":
		// URL buttons. Slack opens the link itself, the ack is all we owe it.
		return
	default:
		r.deps.Log.Warn("unknown interaction", "action_id", action.ActionID, "user", user)
		return
	}

	r.deps.Log.Info("interaction", "action_id", action.ActionID, "user", user, "as_command", text)
	r.Dispatch(ctx, channel, user, name, text)
}

// HandleMention handles @sre-agent in a channel.
//
// The bot mention is stripped and the remainder is parsed with exactly the same grammar
// as the slash command, so there is one language to learn rather than two.
func (r *Router) HandleMention(ctx context.Context, channel, user, text string) {
	cleaned := stripMentions(text)
	if cleaned == "" {
		cleaned = "help"
	}
	r.Dispatch(ctx, channel, user, "", cleaned)
}

func stripMentions(s string) string {
	var out []string
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, "<@") && strings.HasSuffix(f, ">") {
			continue
		}
		out = append(out, f)
	}
	return strings.TrimSpace(strings.Join(out, " "))
}
