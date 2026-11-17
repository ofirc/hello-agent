package commands

import (
	"context"
	"fmt"

	"github.com/slack-go/slack"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/agentloop"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/github"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/slackio"
)

// prOpener is the slice of the GitHub client that opening a proposal's PR needs. It is
// an interface so the confirmation path can be tested without a network.
type prOpener interface {
	GetFile(ctx context.Context, path, ref string) (*github.File, error)
	HeadSHA(ctx context.Context, branch string) (string, error)
	CreateBranch(ctx context.Context, branch, fromSHA string) error
	PutFile(ctx context.Context, branch, path, content, blobSHA, message string) error
	CreatePR(ctx context.Context, head, base, title, body string) (*github.PullRequest, error)
	AddLabels(ctx context.Context, number int, labels ...string)
}

// proposalStore is the slice of agentloop.Store that the buttons need. It is an
// interface so the confirm/cancel race can be tested deterministically.
type proposalStore interface {
	Get(id string) (agentloop.Proposal, bool)
	Take(id string) (agentloop.Proposal, bool)
}

const proposalGone = "That proposal expired or was already used. Run /sre drift fix <service> again."

// applyResult is what decideApply did, for runDriftApply to render. Exactly one of
// Cancelled or PR is set when the error is nil.
type applyResult struct {
	Proposal  agentloop.Proposal
	Cancelled bool
	PR        *github.PullRequest
}

// decideApply is every authorisation and single-use decision behind the confirm and
// cancel buttons, kept apart from Slack rendering so the order of its checks can be
// tested. Swapping them would not break compilation, only the security properties.
//
// The button value, and so the ID, is visible to everyone in the channel, so the ID
// alone is not authority. Only the person who asked for the fix may confirm or cancel
// it; anyone else gets a refusal and the proposal stays pending.
func decideApply(ctx context.Context, store proposalStore, gh prOpener, base, id, user string, cancel bool) (applyResult, error) {
	p, ok := store.Get(id)
	if !ok {
		return applyResult{}, &ErrUsage{Msg: proposalGone}
	}
	if user != p.Requester {
		return applyResult{}, &ErrUsage{Msg: "Only <@" + p.Requester + "> can confirm this proposal."}
	}

	// Take, not Get, gates both buttons. Get above only decides who may act; two clicks
	// that both passed it race here, and exactly one gets the proposal. The loser sees
	// the same message as an expired proposal, which is what it now is, so a cancel
	// that lost to a confirm never claims nothing was pushed.
	p, ok = store.Take(id)
	if !ok {
		return applyResult{}, &ErrUsage{Msg: proposalGone}
	}
	if cancel {
		return applyResult{Proposal: p, Cancelled: true}, nil
	}

	pr, err := applyProposal(ctx, gh, base, p, user)
	if err != nil {
		// The proposal is already consumed. Re-running drift fix is the safe retry: it
		// re-reads the overlay, so a half-applied branch cannot be confirmed blindly.
		return applyResult{}, err
	}
	return applyResult{Proposal: p, PR: pr}, nil
}

// runDriftApply is what the confirm and cancel buttons run. It is hidden from help
// because a proposal ID is only meaningful inside the buttons that carry it. It only
// renders: the decisions live in decideApply.
func runDriftApply(ctx context.Context, d *Deps, in *Invocation) error {
	// Checked here, not in decideApply: a nil *Store inside the interface is not nil.
	if d.Proposals == nil {
		return fmt.Errorf("internal: proposal store is not configured")
	}
	res, err := decideApply(ctx, d.Proposals, d.GH, d.Cfg.GitHubBase,
		in.Arg(0), in.UserID, in.Arg(1) == "cancel")
	if err != nil {
		return err
	}
	p := res.Proposal

	if res.Cancelled {
		in.Rep.Done(ctx, []slack.Block{slackio.Section(fmt.Sprintf(
			":no_entry_sign: Proposal for %s in %s cancelled.", p.Service, p.Env))},
			"proposal cancelled")
		return nil
	}

	pr := res.PR
	d.Log.Info("agent fix PR opened", "pr", pr.HTMLURL, "service", p.Service, "env", p.Env,
		"proposal", p.ID, "requested_by", p.Requester, "confirmed_by", in.UserID)

	in.Rep.Done(ctx, []slack.Block{
		slackio.Section(fmt.Sprintf(":white_check_mark: *PR opened* <%s|#%d> for `%s` in `%s`",
			pr.HTMLURL, pr.Number, p.Service, p.Env)),
		prActionBar(pr.HTMLURL, p.Service, p.Env),
		slackio.Context(fmt.Sprintf("confirmed by %s | the agent does not merge, that stays with a human", in.UserTag)),
	}, "PR opened: "+pr.HTMLURL)
	return nil
}

// applyProposal opens the PR for a confirmed proposal.
//
// The overlay is re-read from base and compared by blob SHA with the one the model
// edited. If anyone touched it between the proposal and the click, the edit was made
// against text that no longer exists, and committing it would silently revert their
// change. Refusing costs one re-run; PutFile would otherwise also reject the stale SHA,
// but only after a stray branch had been created.
func applyProposal(ctx context.Context, gh prOpener, base string, p agentloop.Proposal, confirmer string) (*github.PullRequest, error) {
	file, err := gh.GetFile(ctx, p.Path, base)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", p.Path, err)
	}
	if file == nil || file.SHA != p.BaseSHA {
		return nil, fmt.Errorf("the overlay changed since the proposal, re-run /sre drift fix %s", p.Service)
	}

	// The ID prefix keeps two proposals for the same service from sharing a branch.
	branch := fmt.Sprintf("sre-agent/%s-%s-%s", p.Env, p.Service, shortID(p.ID))

	head, err := gh.HeadSHA(ctx, base)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", base, err)
	}
	if err := gh.CreateBranch(ctx, branch, head); err != nil {
		return nil, fmt.Errorf("creating branch: %w", err)
	}
	if err := gh.PutFile(ctx, branch, p.Path, p.NewContent, p.BaseSHA, p.Title); err != nil {
		return nil, fmt.Errorf("committing: %w", err)
	}

	// The footer states who decided, so a reviewer can tell a model's proposal from a
	// human-written change and knows a person approved it before it reached them.
	body := p.PRBody + "\n\n---\nProposed by the SRE Agent's model, confirmed in Slack by <@" + confirmer + ">. The agent does not merge.\n"
	pr, err := gh.CreatePR(ctx, branch, base, p.Title, body)
	if err != nil {
		return nil, fmt.Errorf("opening PR: %w", err)
	}
	gh.AddLabels(ctx, pr.Number, "sre-agent", "drift")
	return pr, nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
