// Package slackio owns everything that touches the Slack Web API, so the command
// handlers can stay about CI/CD and know nothing about Block Kit.
package slackio

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/slack-go/slack"
)

// Reporter renders a long-running job as one message that keeps updating, rather than a
// wall of new messages. The triage p50 target is two minutes, which is far too long to
// leave a channel silent, and far too noisy to narrate one message per step.
type Reporter struct {
	api     *slack.Client
	channel string
	user    string

	mu      sync.Mutex
	ts      string
	title   string
	steps   []string
	done    bool
	noPost  bool // true until Start succeeds, so Step on a dead message is a no-op
	maxStep int
}

func NewReporter(api *slack.Client, channel, user string) *Reporter {
	return &Reporter{api: api, channel: channel, user: user, maxStep: 12}
}

// Start posts the anchor message. Every later update edits this one message.
func (r *Reporter) Start(ctx context.Context, title string) error {
	r.mu.Lock()
	r.title = title
	r.mu.Unlock()

	_, ts, err := r.api.PostMessageContext(ctx, r.channel,
		slack.MsgOptionBlocks(r.render()...),
		slack.MsgOptionText(title, false), // fallback for notifications
	)
	if err != nil {
		return fmt.Errorf("post anchor message: %w", err)
	}
	r.mu.Lock()
	r.ts, r.noPost = ts, false
	r.mu.Unlock()
	return nil
}

// Step appends a progress line and repaints.
func (r *Reporter) Step(ctx context.Context, format string, args ...any) {
	r.mu.Lock()
	if r.noPost || r.done {
		r.mu.Unlock()
		return
	}
	r.steps = append(r.steps, fmt.Sprintf(format, args...))
	// Keep the message bounded. Slack silently truncates oversized blocks, which looks
	// like the agent hung.
	if len(r.steps) > r.maxStep {
		r.steps = append([]string{"_...earlier steps elided_"}, r.steps[len(r.steps)-r.maxStep:]...)
	}
	r.mu.Unlock()
	r.repaint(ctx)
}

// Done replaces the progress view with the final result.
func (r *Reporter) Done(ctx context.Context, blocks []slack.Block, fallback string) {
	r.mu.Lock()
	r.done = true
	ts, ch, dead := r.ts, r.channel, r.noPost
	r.mu.Unlock()

	if dead || ts == "" {
		_, _, _ = r.api.PostMessageContext(ctx, ch,
			slack.MsgOptionBlocks(blocks...), slack.MsgOptionText(fallback, false))
		return
	}
	_, _, _, _ = r.api.UpdateMessageContext(ctx, ch, ts,
		slack.MsgOptionBlocks(blocks...), slack.MsgOptionText(fallback, false))
}

// Fail reports an error in the same slot. Errors are shown, never swallowed: a ChatOps
// agent that silently drops a command is worse than one that has no command.
func (r *Reporter) Fail(ctx context.Context, err error) {
	blocks := []slack.Block{
		slack.NewSectionBlock(mrkdwn(":x: *"+r.title+"*"), nil, nil),
		slack.NewSectionBlock(mrkdwn("```\n"+truncateForSlack(err.Error(), 2800)+"\n```"), nil, nil),
	}
	r.Done(ctx, blocks, "failed: "+err.Error())
}

// Thread posts a reply under the anchor, for bulky detail that should not bloat the
// main message.
func (r *Reporter) Thread(ctx context.Context, text string) {
	r.mu.Lock()
	ts, ch := r.ts, r.channel
	r.mu.Unlock()
	if ts == "" {
		return
	}
	_, _, _ = r.api.PostMessageContext(ctx, ch,
		slack.MsgOptionText(text, false), slack.MsgOptionTS(ts))
}

// Snippet uploads long output as a file instead of a message. Argo CD manifests and
// multi-service diffs blow past Slack's 3000-character block limit routinely.
func (r *Reporter) Snippet(ctx context.Context, filename, title, content string) {
	_ = r.SnippetErr(ctx, filename, title, content)
}

// SnippetErr is Snippet for callers whose next message depends on the upload, such as a
// confirm button for a diff that is only shown in the snippet.
func (r *Reporter) SnippetErr(ctx context.Context, filename, title, content string) error {
	r.mu.Lock()
	ts, ch := r.ts, r.channel
	r.mu.Unlock()
	_, err := r.api.UploadFileContext(ctx, slack.UploadFileParameters{
		Channels:        []string{ch},
		Filename:        filename,
		Title:           title,
		Content:         content,
		FileSize:        len(content),
		ThreadTimestamp: ts,
	})
	return err
}

func (r *Reporter) TS() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ts
}

func (r *Reporter) repaint(ctx context.Context) {
	r.mu.Lock()
	ts, ch, dead := r.ts, r.channel, r.noPost
	blocks := r.render()
	title := r.title
	r.mu.Unlock()
	if dead || ts == "" {
		return
	}
	_, _, _, _ = r.api.UpdateMessageContext(ctx, ch, ts,
		slack.MsgOptionBlocks(blocks...), slack.MsgOptionText(title, false))
}

// render must be called with the lock held.
func (r *Reporter) render() []slack.Block {
	var b strings.Builder
	b.WriteString(":hourglass_flowing_sand: *" + r.title + "*")
	for _, s := range r.steps {
		b.WriteString("\n" + s)
	}
	return []slack.Block{slack.NewSectionBlock(mrkdwn(truncateForSlack(b.String(), 2900)), nil, nil)}
}

func mrkdwn(s string) *slack.TextBlockObject {
	return slack.NewTextBlockObject(slack.MarkdownType, s, false, false)
}

// Mrkdwn is the exported helper for handlers building their own final blocks.
func Mrkdwn(s string) *slack.TextBlockObject { return mrkdwn(s) }

// Section is a convenience wrapper used by the command handlers.
func Section(s string) *slack.SectionBlock {
	return slack.NewSectionBlock(mrkdwn(truncateForSlack(s, 2900)), nil, nil)
}

// Context renders small grey footnote text.
func Context(s string) *slack.ContextBlock {
	return slack.NewContextBlock("", mrkdwn(truncateForSlack(s, 2900)))
}

func truncateForSlack(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-20] + "\n_...truncated_"
}
