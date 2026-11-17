package commands

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/agentloop"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/github"
)

// fakePR records what applyProposal asks GitHub to do, in order.
type fakePR struct {
	fileSHA string
	calls   []string

	putBranch, putPath, putContent, putBlob, putMsg string
	prHead, prBase, prTitle, prBody                 string
	labels                                          []string
}

func (f *fakePR) GetFile(_ context.Context, path, ref string) (*github.File, error) {
	f.calls = append(f.calls, "GetFile "+path+"@"+ref)
	return &github.File{Path: path, SHA: f.fileSHA}, nil
}
func (f *fakePR) HeadSHA(_ context.Context, branch string) (string, error) {
	f.calls = append(f.calls, "HeadSHA "+branch)
	return "head123", nil
}
func (f *fakePR) CreateBranch(_ context.Context, branch, fromSHA string) error {
	f.calls = append(f.calls, "CreateBranch "+branch+" "+fromSHA)
	return nil
}
func (f *fakePR) PutFile(_ context.Context, branch, path, content, blobSHA, message string) error {
	f.calls = append(f.calls, "PutFile")
	f.putBranch, f.putPath, f.putContent, f.putBlob, f.putMsg = branch, path, content, blobSHA, message
	return nil
}
func (f *fakePR) CreatePR(_ context.Context, head, base, title, body string) (*github.PullRequest, error) {
	f.calls = append(f.calls, "CreatePR")
	f.prHead, f.prBase, f.prTitle, f.prBody = head, base, title, body
	return &github.PullRequest{Number: 7, HTMLURL: "https://example.test/pr/7"}, nil
}
func (f *fakePR) AddLabels(_ context.Context, number int, labels ...string) {
	f.calls = append(f.calls, "AddLabels")
	f.labels = labels
}

func testProposal() agentloop.Proposal {
	return agentloop.Proposal{
		ID: "0123456789abcdef", Requester: "U1", Channel: "C1",
		Env: "prod", Service: "checkout", Path: "gitops/prod/kustomization.yaml",
		BaseSHA: "blob1", OldContent: "old", NewContent: "new",
		Title: "fix checkout", Rationale: "why", PRBody: "the body",
	}
}

func TestApplyProposalOpensPR(t *testing.T) {
	gh := &fakePR{fileSHA: "blob1"}
	p := testProposal()

	pr, err := applyProposal(context.Background(), gh, "main", p, "U2")
	if err != nil {
		t.Fatalf("applyProposal: %v", err)
	}
	if pr == nil || pr.Number != 7 {
		t.Fatalf("pr = %+v, want #7", pr)
	}
	if gh.putContent != "new" || gh.putBlob != "blob1" || gh.putMsg != "fix checkout" || gh.putPath != p.Path {
		t.Errorf("PutFile got content=%q blob=%q msg=%q path=%q", gh.putContent, gh.putBlob, gh.putMsg, gh.putPath)
	}
	if gh.putBranch != "sre-agent/prod-checkout-01234567" || gh.prHead != gh.putBranch || gh.prBase != "main" {
		t.Errorf("branch=%q head=%q base=%q", gh.putBranch, gh.prHead, gh.prBase)
	}
	if !strings.Contains(gh.prBody, "the body") ||
		!strings.Contains(gh.prBody, "confirmed in Slack by <@U2>") ||
		!strings.Contains(gh.prBody, "The agent does not merge.") {
		t.Errorf("PR body = %q", gh.prBody)
	}
	if len(gh.labels) != 2 || gh.labels[0] != "sre-agent" || gh.labels[1] != "drift" {
		t.Errorf("labels = %v", gh.labels)
	}
}

func TestApplyProposalRejectsChangedOverlay(t *testing.T) {
	gh := &fakePR{fileSHA: "someone-else"}

	_, err := applyProposal(context.Background(), gh, "main", testProposal(), "U1")
	if err == nil || !strings.Contains(err.Error(), "changed since the proposal") {
		t.Fatalf("err = %v, want a changed-since-the-proposal error", err)
	}
	for _, c := range gh.calls {
		if c == "PutFile" || c == "CreatePR" || strings.HasPrefix(c, "CreateBranch") {
			t.Errorf("wrote to GitHub despite a changed overlay: %v", gh.calls)
		}
	}
}

// storeWith returns a real store holding testProposal (requester U1) and its ID, since
// Put assigns the ID.
func storeWith(t *testing.T) (*agentloop.Store, string) {
	t.Helper()
	s := agentloop.NewStore(time.Minute, nil)
	return s, s.Put(testProposal())
}

func wantUsage(t *testing.T, err error, msg string) {
	t.Helper()
	var ue *ErrUsage
	if !asUsage(err, &ue) {
		t.Fatalf("err = %v (%T), want ErrUsage", err, err)
	}
	if ue.Msg != msg {
		t.Errorf("message = %q, want %q", ue.Msg, msg)
	}
}

const wantGone = "That proposal expired or was already used. Run /sre drift fix <service> again."

func TestDecideApplyRefusesNonRequesterConfirm(t *testing.T) {
	store, id := storeWith(t)
	gh := &fakePR{fileSHA: "blob1"}

	_, err := decideApply(context.Background(), store, gh, "main", id, "U2", false)
	wantUsage(t, err, "Only <@U1> can confirm this proposal.")
	if _, ok := store.Get(id); !ok {
		t.Error("a refused confirm consumed the proposal")
	}
	if len(gh.calls) != 0 {
		t.Errorf("GitHub was called on a refused confirm: %v", gh.calls)
	}
}

func TestDecideApplyRefusesNonRequesterCancel(t *testing.T) {
	store, id := storeWith(t)
	gh := &fakePR{fileSHA: "blob1"}

	_, err := decideApply(context.Background(), store, gh, "main", id, "U2", true)
	wantUsage(t, err, "Only <@U1> can confirm this proposal.")
	if _, ok := store.Get(id); !ok {
		t.Error("a non-requester cancel deleted the proposal")
	}
	if len(gh.calls) != 0 {
		t.Errorf("GitHub was called on a refused cancel: %v", gh.calls)
	}
}

func TestDecideApplyRequesterCancelDeletes(t *testing.T) {
	store, id := storeWith(t)
	gh := &fakePR{fileSHA: "blob1"}

	res, err := decideApply(context.Background(), store, gh, "main", id, "U1", true)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !res.Cancelled || res.PR != nil || res.Proposal.Service != "checkout" || res.Proposal.Env != "prod" {
		t.Errorf("result = %+v, want cancelled checkout/prod", res)
	}
	if _, ok := store.Get(id); ok {
		t.Error("cancel left the proposal in the store")
	}
	if len(gh.calls) != 0 {
		t.Errorf("GitHub was called on cancel: %v", gh.calls)
	}
}

// Two clicks on confirm must open exactly one PR: Take hands the proposal to one caller.
func TestDecideApplyConfirmTwiceOpensOnePR(t *testing.T) {
	store, id := storeWith(t)
	gh := &fakePR{fileSHA: "blob1"}

	res, err := decideApply(context.Background(), store, gh, "main", id, "U1", false)
	if err != nil || res.PR == nil || res.PR.Number != 7 || res.Cancelled {
		t.Fatalf("first confirm = %+v, %v; want PR #7", res, err)
	}
	callsAfterFirst := len(gh.calls)

	_, err = decideApply(context.Background(), store, gh, "main", id, "U1", false)
	wantUsage(t, err, wantGone)
	if len(gh.calls) != callsAfterFirst {
		t.Errorf("second confirm called GitHub again: %v", gh.calls[callsAfterFirst:])
	}
	n := 0
	for _, c := range gh.calls {
		if c == "CreatePR" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("CreatePR called %d times, want 1", n)
	}
}

// takenBetween is a store in which a confirm took the proposal after cancel's Get had
// already seen it: the race decideApply must not report as a cancel.
type takenBetween struct{ p agentloop.Proposal }

func (s takenBetween) Get(string) (agentloop.Proposal, bool)  { return s.p, true }
func (s takenBetween) Take(string) (agentloop.Proposal, bool) { return agentloop.Proposal{}, false }
func TestDecideApplyCancelLosingToConfirmIsGone(t *testing.T) {
	gh := &fakePR{fileSHA: "blob1"}
	res, err := decideApply(context.Background(), takenBetween{testProposal()}, gh, "main", "id", "U1", true)
	if res.Cancelled {
		t.Fatal("cancel reported success although a confirm had taken the proposal")
	}
	wantUsage(t, err, wantGone)
	if len(gh.calls) != 0 {
		t.Errorf("GitHub was called on cancel: %v", gh.calls)
	}
}

func TestDecideApplyUnknownAndExpired(t *testing.T) {
	gh := &fakePR{fileSHA: "blob1"}

	store, _ := storeWith(t)
	_, err := decideApply(context.Background(), store, gh, "main", "nope", "U1", false)
	wantUsage(t, err, wantGone)

	now := time.Now()
	clock := func() time.Time { return now }
	exp := agentloop.NewStore(time.Minute, clock)
	id := exp.Put(testProposal())
	now = now.Add(2 * time.Minute)
	_, err = decideApply(context.Background(), exp, gh, "main", id, "U1", false)
	wantUsage(t, err, wantGone)

	if len(gh.calls) != 0 {
		t.Errorf("GitHub was called for a missing proposal: %v", gh.calls)
	}
}
