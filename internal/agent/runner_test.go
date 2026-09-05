package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guygrigsby/jess"
	ac "github.com/voocel/agentcore"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/sandbox"
	"github.com/guygrigsby/autophage/internal/store"
	"github.com/guygrigsby/autophage/internal/storetest"
)

var t0 = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

var (
	baseSha = strings.Repeat("a", 40)
	headSha = strings.Repeat("b", 40)
)

// fakeSandbox records the pipeline and simulates commits. It mirrors the
// podman manager's contract closely enough to catch a runner that breaks it:
// Start hands the Workspace back inside the Container, so a Teardown built
// by hand instead of passed through shows up as a missing repository, and
// every call is recorded in order so the test can prove CommitAndPush ran
// before the container was torn down and before the toolbox closer closed.
type fakeSandbox struct {
	mu        sync.Mutex
	calls     []string
	prepared  []string
	started   []string
	torn      []string
	tornRepos []string
	pushed    []string
	commits   bool
	failStart bool
	failTools bool
	failPush  error
	diff      int
}

func (f *fakeSandbox) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

// order is a copy of the recorded call sequence.
func (f *fakeSandbox) order() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeSandbox) Prepare(_ context.Context, repo, cloneURL, branch, def, token string) (sandbox.Workspace, error) {
	if token == "" {
		return sandbox.Workspace{}, errors.New("no token")
	}
	f.mu.Lock()
	f.calls = append(f.calls, "prepare")
	f.prepared = append(f.prepared, repo+" "+cloneURL+" "+branch+" "+def)
	f.mu.Unlock()
	return sandbox.Workspace{Path: "/tmp/x", Repository: repo, CloneURL: cloneURL, Branch: branch, DefaultBranch: def, BaseSha: baseSha}, nil
}

func (f *fakeSandbox) Start(_ context.Context, ws sandbox.Workspace, id string) (sandbox.Container, error) {
	if f.failStart {
		return sandbox.Container{}, errors.New("podman: image missing")
	}
	f.mu.Lock()
	f.calls = append(f.calls, "start")
	f.started = append(f.started, id)
	f.mu.Unlock()
	return sandbox.Container{Name: "c-" + id, Workspace: ws}, nil
}

func (f *fakeSandbox) Tools(context.Context, sandbox.Container) ([]ac.Tool, io.Closer, error) {
	if f.failTools {
		return nil, nil, errors.New("toolbox: exec refused")
	}
	f.record("tools")
	return []ac.Tool{&echoTool{}}, recordCloser{f}, nil
}

func (f *fakeSandbox) DiffLines(context.Context, sandbox.Container, string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "diff")
	return f.diff, nil
}

func (f *fakeSandbox) CommitAndPush(_ context.Context, ws sandbox.Workspace, _, msg string) (string, bool, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "push")
	f.pushed = append(f.pushed, msg)
	f.mu.Unlock()
	if f.failPush != nil {
		return "", false, f.failPush
	}
	if f.commits {
		return headSha, true, nil
	}
	return ws.BaseSha, true, nil
}

func (f *fakeSandbox) Teardown(_ context.Context, c sandbox.Container) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "teardown")
	f.torn = append(f.torn, c.Name)
	f.tornRepos = append(f.tornRepos, c.Workspace.Repository)
	return nil
}

// recordCloser is the toolbox closer, recorded like every other call so the
// test can prove it is closed after CommitAndPush rather than before it.
type recordCloser struct{ f *fakeSandbox }

func (c recordCloser) Close() error {
	c.f.record("close")
	return nil
}

type fakeGitHub struct {
	mu     sync.Mutex
	prs    []string
	issue  resolution.IssueDetail
	branch string // DefaultBranch answer; empty means the enrolled one
	prErr  error
}

func (g *fakeGitHub) GetIssue(context.Context, string, int) (resolution.IssueDetail, error) {
	return g.issue, nil
}

func (g *fakeGitHub) DefaultBranch(_ context.Context, repo resolution.Repository) (string, error) {
	if g.branch == "" {
		return repo.DefaultBranch, nil
	}
	return g.branch, nil
}

func (g *fakeGitHub) MintToken(context.Context, resolution.Repository) (resolution.Token, error) {
	return resolution.Token{Value: "tok", ExpiresAt: t0.Add(time.Hour)}, nil
}

func (g *fakeGitHub) PostComment(context.Context, string, int, string) (int64, error) { return 1, nil }

func (g *fakeGitHub) OpenPullRequest(_ context.Context, _, head, base, title, body string) (int, error) {
	if g.prErr != nil {
		return 0, g.prErr
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prs = append(g.prs, head+" -> "+base+": "+title+"\n"+body)
	return 12, nil
}

func (g *fakeGitHub) EnsureLabel(context.Context, string, string) error { return nil }

func (g *fakeGitHub) pullRequests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.prs...)
}

type countMetrics struct {
	mu    sync.Mutex
	ended []string
}

func (m *countMetrics) Ended(kind, outcome string, _ resolution.Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ended = append(m.ended, kind+"/"+outcome)
}

func (m *countMetrics) all() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.ended...)
}

// startedAttempt enrolls a repository, receives a case, sizes it small and
// opens one attempt of kind, returning the attempt id the runner takes.
func startedAttempt(t *testing.T, st *store.Store, kind resolution.AttemptKind) string {
	t.Helper()
	ctx := t.Context()
	r, err := resolution.NewRepository("guy/repo", 42, "main", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnrollRepository(ctx, r); err != nil {
		t.Fatal(err)
	}
	req, err := resolution.NewRequester("guy", resolution.AssociationOwner)
	if err != nil {
		t.Fatal(err)
	}
	c, err := resolution.NewCase("guy/repo", 7, req, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCase(ctx, c); err != nil {
		t.Fatal(err)
	}
	b, err := resolution.NewBudget(10, time.Minute, 500)
	if err != nil {
		t.Fatal(err)
	}
	c, err = st.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error {
		if err := c.RecordTriage(resolution.Triage{Size: resolution.Small, Rationale: "r", Model: "m", TriagedAt: t0}); err != nil {
			return err
		}
		_, err := c.StartAttempt(kind, b, "fix the typo", t0)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return c.OpenAttempt().ID
}

func newRunner(t *testing.T, st *store.Store, sb *fakeSandbox, gh *fakeGitHub, model ac.ChatModel) (*Runner, *countMetrics) {
	t.Helper()
	m := &countMetrics{}
	r := &Runner{
		Store: st, GitHub: gh, Sandbox: sb, Ledger: &memLedger{}, Clock: resolution.SystemClock{},
		AttemptModel: "test/auto", ApprovedModel: "test/approved", Metrics: m, Logf: t.Logf,
		CloneURL: func(repo string) string { return "file:///" + repo },
		model:    model,
	}
	return r, m
}

func index(t *testing.T, calls []string, want string) int {
	t.Helper()
	for i, c := range calls {
		if c == want {
			return i
		}
	}
	t.Fatalf("%q never called: %v", want, calls)
	return -1
}

func TestRunnerOpensPullRequestOnSuccess(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{commits: true}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "Typo in README", Body: "teh", Open: true}}
	r, m := newRunner(t, st, sb, gh, scripted(1, goodSummary, nil))
	r.Run(t.Context(), id)
	c, err := st.GetCase(t.Context(), "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if c.State() != resolution.Done {
		t.Fatalf("state = %s", c.State())
	}
	a := c.Attempts()[0]
	if a.Run == nil || a.Run.RunID == "" || a.Run.BaseSha != baseSha || a.Run.Model != "test/auto" {
		t.Errorf("run = %+v", a.Run)
	}
	if a.Outcome == nil || a.Outcome.Kind != resolution.PullRequestOpened || a.Outcome.PRNumber != 12 || a.Outcome.HeadSha != headSha || a.Outcome.Summary != goodSummary {
		t.Errorf("outcome = %+v", a.Outcome)
	}
	prs := gh.pullRequests()
	if len(prs) != 1 || !strings.Contains(prs[0], "autophage/7 -> main") || !strings.Contains(prs[0], "Fixes #7") || !strings.Contains(prs[0], "Typo in README") {
		t.Errorf("prs = %q", prs)
	}
	if len(sb.prepared) != 1 || sb.prepared[0] != "guy/repo file:///guy/repo autophage/7 main" {
		t.Errorf("prepared = %q", sb.prepared)
	}
	if len(sb.started) != 1 || len(sb.torn) != 1 || len(sb.pushed) != 1 || sb.pushed[0] != "autophage: attempt 1" {
		t.Errorf("started %v torn %v pushed %q", sb.started, sb.torn, sb.pushed)
	}
	if len(sb.tornRepos) != 1 || sb.tornRepos[0] != "guy/repo" {
		t.Errorf("teardown did not get the workspace Start returned: %q", sb.tornRepos)
	}
	// CommitAndPush kills the container, so it must come before the
	// teardown, and the toolbox closer must still be closed after it.
	calls := sb.order()
	push := index(t, calls, "push")
	if last := index(t, calls, "diff"); last > push {
		t.Errorf("diff counted after the push: %v", calls)
	}
	if index(t, calls, "close") < push || index(t, calls, "teardown") < push {
		t.Errorf("closer or teardown ran before the push: %v", calls)
	}
	if got := m.all(); len(got) != 1 || got[0] != "auto/pull_request_opened" {
		t.Errorf("metrics = %v", got)
	}
}

func TestRunnerNoCommitsIsAgentFailure(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{commits: false}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}}
	r, _ := newRunner(t, st, sb, gh, scripted(0, "What I found: this is not a bug.\nWhat I did: nothing.\nWhat is left: n/a.\nWhat I would do with more budget: n/a.", nil))
	r.Run(t.Context(), id)
	c, err := st.GetCase(t.Context(), "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Attempts()[0].Outcome
	if c.State() != resolution.Failed || o == nil || o.Kind != resolution.FailedOutcome || o.Class != resolution.FailureAgent || len(gh.pullRequests()) != 0 {
		t.Errorf("state %s outcome %+v prs %v", c.State(), o, gh.pullRequests())
	}
}

func TestRunnerBudgetExhaustedPushesAndParks(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{commits: true}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}}
	r, _ := newRunner(t, st, sb, gh, scripted(100, goodSummary, nil))
	r.Run(t.Context(), id)
	c, err := st.GetCase(t.Context(), "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Attempts()[0].Outcome
	if c.State() != resolution.AwaitingApproval || o == nil || o.Kind != resolution.BudgetExhausted || o.Limit != resolution.LimitTurns || len(sb.pushed) != 1 || len(gh.pullRequests()) != 0 {
		t.Errorf("state %s outcome %+v pushed %d prs %d", c.State(), o, len(sb.pushed), len(gh.pullRequests()))
	}
}

func TestRunnerInfraFailureAndTeardown(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{failStart: true}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}}
	r, _ := newRunner(t, st, sb, gh, scripted(0, goodSummary, nil))
	r.Run(t.Context(), id)
	c, err := st.GetCase(t.Context(), "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Attempts()[0].Outcome
	if c.State() != resolution.Failed || o == nil || o.Class != resolution.FailureInfra || !strings.Contains(o.Message, "image missing") || c.Attempts()[0].Run != nil {
		t.Errorf("state %s outcome %+v run %+v", c.State(), o, c.Attempts()[0].Run)
	}
	// A failed Start releases the workspace lock itself, so the runner must
	// not tear down a container it never got.
	if len(sb.torn) != 0 {
		t.Errorf("torn down after a failed start: %v", sb.torn)
	}
}

func TestRunnerFailedPushIsInfraAndOpensNoPullRequest(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{commits: true, failPush: errors.New("git: rejected, stale info")}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}}
	r, _ := newRunner(t, st, sb, gh, scripted(0, goodSummary, nil))
	r.Run(t.Context(), id)
	c, err := st.GetCase(t.Context(), "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Attempts()[0].Outcome
	if c.State() != resolution.Failed || o == nil || o.Class != resolution.FailureInfra || !strings.Contains(o.Message, "stale info") || len(gh.pullRequests()) != 0 {
		t.Errorf("state %s outcome %+v prs %v", c.State(), o, gh.pullRequests())
	}
	if len(sb.torn) != 1 {
		t.Errorf("container not torn down after a failed push: %v", sb.torn)
	}
}

func TestRunnerRefreshesTheDefaultBranch(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{commits: true}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}, branch: "trunk"}
	r, _ := newRunner(t, st, sb, gh, scripted(0, goodSummary, nil))
	r.Run(t.Context(), id)
	repo, err := st.GetRepository(t.Context(), "guy/repo")
	if err != nil {
		t.Fatal(err)
	}
	if repo.DefaultBranch != "trunk" || repo.InstallationID != 42 {
		t.Errorf("repository = %+v", repo)
	}
	if len(sb.prepared) != 1 || !strings.HasSuffix(sb.prepared[0], "autophage/7 trunk") {
		t.Errorf("prepared against the stale branch: %q", sb.prepared)
	}
	prs := gh.pullRequests()
	if len(prs) != 1 || !strings.Contains(prs[0], "autophage/7 -> trunk") {
		t.Errorf("prs = %q", prs)
	}
}

func TestRunnerRecordsTheApprovedTierModel(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Approved)
	sb := &fakeSandbox{commits: true}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}}
	r, m := newRunner(t, st, sb, gh, scripted(0, goodSummary, nil))
	r.Run(t.Context(), id)
	c, err := st.GetCase(t.Context(), "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	a := c.Attempts()[0]
	if a.Run == nil || a.Run.Model != "test/approved" {
		t.Errorf("run = %+v", a.Run)
	}
	if got := m.all(); len(got) != 1 || got[0] != "approved/pull_request_opened" {
		t.Errorf("metrics = %v", got)
	}
}

func TestRunnerRefusesAnAttemptThatIsNotOpen(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	o, err := resolution.OutcomeFailed(resolution.FailureInfra, "already ended", resolution.Usage{}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateCase(t.Context(), "guy/repo", 7, func(c *resolution.Case) error {
		return c.RecordOutcome(id, o)
	}); err != nil {
		t.Fatal(err)
	}
	sb := &fakeSandbox{commits: true}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}}
	r, m := newRunner(t, st, sb, gh, scripted(0, goodSummary, nil))
	r.Run(t.Context(), id)
	if len(sb.order()) != 0 || len(m.all()) != 0 {
		t.Errorf("ran a closed attempt: calls %v metrics %v", sb.order(), m.all())
	}
}

// blocking is a model that answers nothing until its context ends, so the
// operator stop has something to cancel.
func blocking() ac.ChatModel {
	return jess.Once(true, func(ctx context.Context, _ []ac.Message, _ []ac.ToolSpec) (*ac.LLMResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
}

func TestRunnerStopByOperator(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{commits: true}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}}
	r, _ := newRunner(t, st, sb, gh, blocking())
	done := make(chan struct{})
	go func() { r.Run(t.Context(), id); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for !r.Stop(id) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not end after stop")
	}
	c, err := st.GetCase(t.Context(), "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Attempts()[0].Outcome
	if c.State() != resolution.AwaitingApproval || o == nil || o.Kind != resolution.Aborted || o.Reason != resolution.AbortOperatorStop {
		t.Errorf("state %s outcome %+v", c.State(), o)
	}
	// The work the agent did reaches the branch even though it was stopped.
	if len(sb.pushed) != 1 || len(sb.torn) != 1 {
		t.Errorf("pushed %v torn %v", sb.pushed, sb.torn)
	}
	if r.Stop(id) {
		t.Error("second stop reported running")
	}
}

func TestRunnerCancelCarriesTheReason(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	if err := st.StoreDeliveryForTest(t.Context(), "d-close"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateCase(t.Context(), "guy/repo", 7, func(c *resolution.Case) error {
		return c.Close(resolution.Closure{DeliveryID: "d-close", ClosedAt: t0})
	}); err != nil {
		t.Fatal(err)
	}
	sb := &fakeSandbox{commits: true}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: false}}
	r, _ := newRunner(t, st, sb, gh, blocking())
	done := make(chan struct{})
	go func() { r.Run(t.Context(), id); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for !r.Cancel(id, resolution.AbortIssueClosed) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not end after cancel")
	}
	c, err := st.GetCase(t.Context(), "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Attempts()[0].Outcome
	if c.State() != resolution.Closed || o == nil || o.Kind != resolution.Aborted || o.Reason != resolution.AbortIssueClosed {
		t.Errorf("state %s outcome %+v", c.State(), o)
	}
}

func TestTriagerReportsAModelItCannotBuild(t *testing.T) {
	r := &Runner{TriageModel: "some/model"}
	if _, err := r.Triager().Classify(t.Context(), "t", "b"); err == nil {
		t.Error("a runner with no OpenRouter key built a triager that answers")
	}
}
