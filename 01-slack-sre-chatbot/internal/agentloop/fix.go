package agentloop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/gitops"
)

// fixMaxLLMCalls is the fix agent's model-call budget. It is lower than debug's: the
// fix agent edits one file it has already been pointed at.
const fixMaxLLMCalls = 12

// GitHub rejects titles over about 256 characters and bodies over about 64 KB, and
// CreatePR runs only after the branch is pushed and the proposal consumed, so an
// oversize title or body is refused here, where the model can still revise it. The
// body cap also keeps it short enough for the requester to read before confirming.
const (
	maxPRTitleRunes = 200
	maxPRBodyBytes  = 8192
)

type FixRequest struct {
	Service, Env       string // Env "" lets the model choose
	Requester, Channel string
	Rows               []drift.Row // disagreeing rows given to the model up front
	Progress           Progress
}

// FixResult has exactly one of its fields set.
type FixResult struct {
	Proposal *Proposal // set when the model proposed
	Declined string    // set when the model declined
}

type proposeArgs struct {
	Env        string `json:"env" jsonschema:"the environment whose overlay you edit, for example prod"`
	NewContent string `json:"new_content" jsonschema:"the complete new overlay file, not a diff"`
	Title      string `json:"title" jsonschema:"pull request title, as the gitops-fix skill describes, at most 200 characters"`
	Rationale  string `json:"rationale" jsonschema:"one or two sentences on why this edit"`
	PRBody     string `json:"pr_body" jsonschema:"pull request body, as the gitops-fix skill describes, at most 8192 bytes. The engineer reads it before confirming."`
}

type declineArgs struct {
	Reason string `json:"reason" jsonschema:"what is wrong and who needs to do what"`
}

type finishResult struct {
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

const fixJob = "You fix a disagreement between git (the GitOps overlays) and the cluster (Argo CD) for an engineer in Slack, " +
	"by proposing the full new content of one environment's kustomization overlay. " +
	"You never change git or the cluster yourself: an engineer reviews your proposal and confirms it before a pull request is opened.\n" +
	"Before anything else, load the gitops-fix skill with load_skill and follow its procedure. " +
	"Load the drift-triage skill too if you need it to tell what kind of disagreement this is.\n" +
	"Read the overlay with get_overlay before proposing a change to it.\n"

const fixFinish = "Finish with exactly one call: propose_fix with the complete new overlay, or decline_fix with a reason. " +
	"Decline rather than invent an edit the gitops-fix skill does not support. " +
	"If propose_fix returns an error, correct the edit and call it again, or decline.\n"

func fixInstruction(sandbox bool) string {
	return fixJob + skillResourcesInstruction + safetyInstruction + codeInstruction(sandbox) + fixFinish
}

// fixAllowed is the gate on which overlay may be edited: a configured environment that
// is also on the fix allow list.
func (a *Agent) fixAllowed(env string) bool {
	return slices.Contains(a.cfg.Envs, env) && slices.Contains(a.cfg.AllowFixEnvs, env)
}

// Fix runs the fix agent. It returns the model's proposal or its reason for declining;
// it does not store the proposal, the caller does when it asks for confirmation.
func (a *Agent) Fix(ctx context.Context, req FixRequest) (*FixResult, error) {
	if req.Service == "" {
		return nil, errors.New("agentloop: Fix needs a service")
	}
	// A run whose only permitted target is off limits could never propose; refuse it
	// before spending model calls. The env is not quoted: the error reaches Slack.
	if req.Env != "" && !a.fixAllowed(req.Env) {
		return nil, errors.New("fixes are not allowed in the requested environment")
	}
	s, err := a.newRun(req.Progress)
	if err != nil {
		return nil, err
	}
	defer s.closeDetached(ctx)

	task := "Fix the service " + req.Service + ". "
	if req.Env != "" {
		task += "Edit the " + req.Env + " overlay only. "
	} else {
		task += "Choose the environment whose overlay to edit. "
	}
	task += "Fixes are allowed in: " + strings.Join(a.cfg.AllowFixEnvs, ", ") + "."
	msg, err := a.factsMessage(task, req.Rows)
	if err != nil {
		return nil, err
	}
	tools, err := a.readTools(s)
	if err != nil {
		return nil, err
	}
	finish, err := s.fixTools(req)
	if err != nil {
		return nil, err
	}
	if _, err := a.runLoop(ctx, s, loopSpec{
		name:        "drift_fix",
		instruction: fixInstruction(a.SandboxAvailable()),
		tools:       append(tools, finish...),
		maxLLMCalls: fixMaxLLMCalls,
	}, msg); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.proposal != nil:
		p := *s.proposal
		return &FixResult{Proposal: &p}, nil
	case s.declined != "":
		return &FixResult{Declined: s.declined}, nil
	}
	return nil, errors.New("the model finished without proposing or declining")
}

// finishFirst records the run's outcome unless another finish got there first. ADK runs
// one turn's calls concurrently, so this is the only place the check is decisive.
func (s *runState) finishFirst(record func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return false
	}
	s.finished = true
	record()
	return true
}

func (s *runState) alreadyFinished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished
}

const errFinished = "this run already finished"

// proposeFix checks a proposed overlay and records it. Rejections go back to the model,
// which may name its own values; nothing from args reaches Slack except an env that
// passed checkEnv.
func (s *runState) proposeFix(ctx context.Context, req FixRequest, args proposeArgs) finishResult {
	reject := func(msg string) finishResult {
		s.reject("propose_fix")
		return finishResult{Error: msg}
	}
	if s.alreadyFinished() {
		return reject(errFinished)
	}
	if msg := s.checkEnv(args.Env); msg != "" {
		return reject(msg)
	}
	if !slices.Contains(s.a.cfg.AllowFixEnvs, args.Env) {
		return reject(fmt.Sprintf("fixes are not allowed in %s, only in %s", args.Env, strings.Join(s.a.cfg.AllowFixEnvs, ", ")))
	}
	if req.Env != "" && args.Env != req.Env {
		return reject(fmt.Sprintf("this run fixes the %s overlay only", req.Env))
	}
	s.mu.Lock()
	o := s.overlays[args.Env]
	s.mu.Unlock()
	if o == nil {
		return reject("call get_overlay first")
	}
	// ValidateOverlay first: CheckProtected reports bad YAML without saying which file.
	if err := gitops.ValidateOverlay(args.NewContent); err != nil {
		return reject(err.Error())
	}
	if args.NewContent == o.Content {
		return reject("no change")
	}
	if err := gitops.CheckProtected(o.Content, args.NewContent); err != nil {
		return reject(err.Error())
	}
	if strings.TrimSpace(args.Title) == "" {
		return reject("title is required")
	}
	if utf8.RuneCountInString(args.Title) > maxPRTitleRunes {
		return reject(fmt.Sprintf("title is over %d characters, shorten it", maxPRTitleRunes))
	}
	if len(args.PRBody) > maxPRBodyBytes {
		return reject(fmt.Sprintf("pr_body is over %d bytes, shorten it", maxPRBodyBytes))
	}

	p := &Proposal{
		Requester: req.Requester, Channel: req.Channel,
		Env: args.Env, Service: req.Service, Path: o.Path, BaseSHA: o.SHA,
		OldContent: o.Content, NewContent: args.NewContent,
		Title: args.Title, Rationale: args.Rationale, PRBody: args.PRBody,
	}
	if !s.finishFirst(func() { s.proposal = p }) {
		return reject(errFinished)
	}
	s.begin(ctx, "propose_fix", envArgs{Env: args.Env}, fmt.Sprintf("Proposing a change to the %s overlay", args.Env))
	return finishResult{Status: "proposed"}
}

// declineFix records why no edit is proposed. The reason is shown to the user as the
// run's answer, so the trail leaves it out.
func (s *runState) declineFix(ctx context.Context, args declineArgs) finishResult {
	reject := func(msg string) finishResult {
		s.reject("decline_fix")
		return finishResult{Error: msg}
	}
	if s.alreadyFinished() {
		return reject(errFinished)
	}
	reason := strings.TrimSpace(args.Reason)
	if reason == "" {
		return reject("reason is required")
	}
	if !s.finishFirst(func() { s.declined = reason }) {
		return reject(errFinished)
	}
	s.begin(ctx, "decline_fix", noArgs{}, "Declining to propose a change")
	return finishResult{Status: "declined"}
}

// fixTools are the two ways a fix run ends. A successful call sets SkipSummarization,
// which makes its response event final, so ADK ends the invocation without calling the
// model again. A rejected call does not, so the model sees the error and can retry.
func (s *runState) fixTools(req FixRequest) ([]tool.Tool, error) {
	propose, err := functiontool.New(functiontool.Config{
		Name: "propose_fix",
		Description: "Propose the complete new content of one environment's overlay, read with get_overlay first. " +
			"It is checked and shown to an engineer, who confirms before a pull request is opened. Ends the run.",
	}, func(ctx agent.Context, args proposeArgs) (finishResult, error) {
		r := s.proposeFix(ctx, req, args)
		if r.Error == "" {
			ctx.Actions().SkipSummarization = true
		}
		return r, nil
	})
	if err != nil {
		return nil, err
	}
	decline, err := functiontool.New(functiontool.Config{
		Name:        "decline_fix",
		Description: "Decline to propose an edit, with a reason a human can act on. Ends the run.",
	}, func(ctx agent.Context, args declineArgs) (finishResult, error) {
		r := s.declineFix(ctx, args)
		if r.Error == "" {
			ctx.Actions().SkipSummarization = true
		}
		return r, nil
	})
	if err != nil {
		return nil, err
	}
	return []tool.Tool{propose, decline}, nil
}
