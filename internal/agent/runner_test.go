package agent

import (
	"context"
	"errors"
	"fmt"
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
	mu         sync.Mutex
	calls      []string
	prepared   []string
	started    []string
	torn       []string
	tornRepos  []string
	pushed     []string
	pushTokens []string
	commits    bool
	failStart  bool
	failTools  bool
	failPush   error
	noPush     bool
	// blockPrepare holds Prepare until its context ends, so a stop can land
	// during setup. preparing is closed when that Prepare is entered.
	blockPrepare bool
	preparing    chan struct{}
	// onDiff runs at the top of DiffLines. RunAttempt takes its final count
	// there, so a test can land a shutdown in the window between the run
	// ending and the runner deciding what its outcome is.
	onDiff func()
	diff   int
	// conflicted is what ConflictMarkers answers, and conflictErr the error
	// it fails with instead.
	conflicted  bool
	conflictErr error
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

func (f *fakeSandbox) Prepare(ctx context.Context, repo, cloneURL, branch, def, token string) (sandbox.Workspace, error) {
	if token == "" {
		return sandbox.Workspace{}, errors.New("no token")
	}
	if f.blockPrepare {
		close(f.preparing)
		<-ctx.Done()
		// Not ctx.Err(): the real manager runs git and podman through
		// os/exec, which reports the *ExitError of the process it killed in
		// preference to the context's own error, so a cancelled step almost
		// never hands back something that unwraps to context.Canceled.
		return sandbox.Workspace{}, errors.New("git clone: signal: killed")
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
	if f.onDiff != nil {
		f.onDiff()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "diff")
	return f.diff, nil
}

func (f *fakeSandbox) CommitAndPush(_ context.Context, ws sandbox.Workspace, token, msg string) (string, bool, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "push")
	f.pushed = append(f.pushed, msg)
	f.pushTokens = append(f.pushTokens, token)
	f.mu.Unlock()
	if f.failPush != nil {
		return "", false, f.failPush
	}
	head := ws.BaseSha
	if f.commits {
		head = headSha
	}
	return head, !f.noPush, nil
}

// ConflictMarkers is valid only after CommitAndPush, so it records itself
// like every other call and the test asserts that order.
func (f *fakeSandbox) ConflictMarkers(context.Context, sandbox.Workspace) (bool, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "conflicts")
	f.mu.Unlock()
	return f.conflicted, f.conflictErr
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
	minted []string
	issue  resolution.IssueDetail
	branch string // DefaultBranch answer; empty means the enrolled one
	prErr  error
	// remintErr, when set, fails every mint after the first, so a test can
	// drive the push that has to fall back to the token already in hand.
	remintErr error
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

// MintToken hands out a distinct value per call, so a caller that reuses an
// earlier token instead of minting a fresh one is visible in what it pushed.
func (g *fakeGitHub) MintToken(context.Context, resolution.Repository) (resolution.Token, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.remintErr != nil && len(g.minted) > 0 {
		return resolution.Token{}, g.remintErr
	}
	value := fmt.Sprintf("tok-%d", len(g.minted)+1)
	g.minted = append(g.minted, value)
	return resolution.Token{Value: value, ExpiresAt: t0.Add(time.Hour)}, nil
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

func (g *fakeGitHub) tokens() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.minted...)
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

// logRecorder keeps every line the runner logs so a test can prove no minted
// token reached one. The runner's goroutine and the agent's both log, so it
// locks.
type logRecorder struct {
	t     *testing.T
	mu    sync.Mutex
	lines []string
}

func (l *logRecorder) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.mu.Unlock()
	l.t.Log(line)
}

func (l *logRecorder) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// assertOperatorMessage proves an outcome message is the one operator-facing
// line: it names what failed and where to look, and nothing else. The
// message is posted verbatim to the issue.
func assertOperatorMessage(t *testing.T, message, want, attemptID string) {
	t.Helper()
	head, _, _ := strings.Cut(message, "\n")
	if head != want+"; see `autophage why "+attemptID+"` on the daemon host" {
		t.Errorf("outcome message opens %q, want %q with the attempt id", head, want)
	}
}

// assertLogged proves the raw failure the outcome no longer carries reached
// the operator's log instead.
func assertLogged(t *testing.T, logs *logRecorder, want string) {
	t.Helper()
	for _, line := range logs.all() {
		if strings.Contains(line, want) {
			return
		}
	}
	t.Errorf("%q never reached the log: %v", want, logs.all())
}

// assertNoTokenLogged proves no installation token the run minted was
// written to the log.
func assertNoTokenLogged(t *testing.T, logs *logRecorder, gh *fakeGitHub) {
	t.Helper()
	tokens := gh.tokens()
	if len(tokens) == 0 {
		t.Fatal("nothing was minted, so the assertion proves nothing")
	}
	for _, line := range logs.all() {
		for _, token := range tokens {
			if strings.Contains(line, token) {
				t.Errorf("a minted token reached the log: %q", line)
			}
		}
	}
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

func newRunner(t *testing.T, st *store.Store, sb *fakeSandbox, gh *fakeGitHub, model ac.ChatModel) (*Runner, *countMetrics, *logRecorder) {
	t.Helper()
	m := &countMetrics{}
	logs := &logRecorder{t: t}
	r := &Runner{
		Store: st, GitHub: gh, Sandbox: sb, Ledger: &memLedger{}, Clock: resolution.SystemClock{},
		AttemptModel: "test/auto", ApprovedModel: "test/approved", Metrics: m, Logf: logs.logf,
		CloneURL:      func(repo string) string { return "file:///" + repo },
		ModelOverride: model,
	}
	return r, m, logs
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

// lastIndex is where want was called for the last time, which is what an
// assertion about the final DiffLines count needs.
func lastIndex(t *testing.T, calls []string, want string) int {
	t.Helper()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i] == want {
			return i
		}
	}
	t.Fatalf("%q never called: %v", want, calls)
	return -1
}

// waitFor blocks until ok reports true, failing the test if it never does.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunnerOpensPullRequestOnSuccess(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{commits: true}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "Typo in README", Body: "teh", Open: true}}
	r, m, logs := newRunner(t, st, sb, gh, scripted(1, goodSummary, nil))
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
	// An installation token lives an hour and an attempt can run for three,
	// so the push must carry a token minted for it, not the one Prepare used.
	minted := gh.tokens()
	if len(minted) != 2 {
		t.Fatalf("minted %v, want one token for the workspace and a fresh one for the push", minted)
	}
	if len(sb.pushTokens) != 1 || sb.pushTokens[0] != minted[1] {
		t.Errorf("pushed with %q, want the second mint %q", sb.pushTokens, minted[1])
	}
	assertNoTokenLogged(t, logs, gh)
	// CommitAndPush kills the container, so it must come before the
	// teardown, and the toolbox closer must still be closed after it.
	calls := sb.order()
	push := index(t, calls, "push")
	if last := lastIndex(t, calls, "diff"); last > push {
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
	r, _, _ := newRunner(t, st, sb, gh, scripted(0, "What I found: this is not a bug.\nWhat I did: nothing.\nWhat is left: n/a.\nWhat I would do with more budget: n/a.", nil))
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
	r, _, _ := newRunner(t, st, sb, gh, scripted(100, goodSummary, nil))
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
	r, _, logs := newRunner(t, st, sb, gh, scripted(0, goodSummary, nil))
	r.Run(t.Context(), id)
	c, err := st.GetCase(t.Context(), "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Attempts()[0].Outcome
	if c.State() != resolution.Failed || o == nil || o.Class != resolution.FailureInfra || c.Attempts()[0].Run != nil {
		t.Errorf("state %s outcome %+v run %+v", c.State(), o, c.Attempts()[0].Run)
	}
	// The outcome message is posted to the issue, so it names the step and
	// where to look, never the raw error; the raw error is the operator's,
	// in the log, against the attempt id.
	assertOperatorMessage(t, o.Message, "infrastructure failure at start container", id)
	assertLogged(t, logs, "image missing")
	if strings.Contains(o.Message, "image missing") {
		t.Errorf("the raw error reached the issue comment: %q", o.Message)
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
	r, _, logs := newRunner(t, st, sb, gh, scripted(0, goodSummary, nil))
	r.Run(t.Context(), id)
	c, err := st.GetCase(t.Context(), "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Attempts()[0].Outcome
	if c.State() != resolution.Failed || o == nil || o.Class != resolution.FailureInfra || len(gh.pullRequests()) != 0 {
		t.Errorf("state %s outcome %+v prs %v", c.State(), o, gh.pullRequests())
	}
	assertOperatorMessage(t, o.Message, "infrastructure failure at push", id)
	assertLogged(t, logs, "stale info")
	if strings.Contains(o.Message, "stale info") {
		t.Errorf("the raw git error reached the issue comment: %q", o.Message)
	}
	if len(sb.torn) != 1 {
		t.Errorf("container not torn down after a failed push: %v", sb.torn)
	}
	assertNoTokenLogged(t, logs, gh)
}

// A push that failed under a budget stop is still a failed push: the
// operator must not be told to resume from a branch that is not there.
func TestRunnerFailedPushBeatsTheBudgetStop(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{commits: true, failPush: errors.New("git: rejected, stale info")}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}}
	r, _, logs := newRunner(t, st, sb, gh, scripted(100, goodSummary, nil))
	r.Run(t.Context(), id)
	c, err := st.GetCase(t.Context(), "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Attempts()[0].Outcome
	if c.State() != resolution.Failed || o == nil || o.Kind != resolution.FailedOutcome || o.Class != resolution.FailureInfra {
		t.Fatalf("state %s outcome %+v, want a failed push over the budget stop", c.State(), o)
	}
	assertOperatorMessage(t, o.Message, "infrastructure failure at push", id)
	assertLogged(t, logs, "stale info")
	if !strings.Contains(o.Message, "What I found") {
		t.Errorf("the summary was not kept as detail: %q", o.Message)
	}
}

func TestRunnerUnpushedBranchIsInfra(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{commits: true, noPush: true}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}}
	r, _, logs := newRunner(t, st, sb, gh, scripted(0, goodSummary, nil))
	r.Run(t.Context(), id)
	c, err := st.GetCase(t.Context(), "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Attempts()[0].Outcome
	if c.State() != resolution.Failed || o == nil || o.Kind != resolution.FailedOutcome || o.Class != resolution.FailureInfra || len(gh.pullRequests()) != 0 {
		t.Fatalf("state %s outcome %+v prs %v", c.State(), o, gh.pullRequests())
	}
	assertOperatorMessage(t, o.Message, "infrastructure failure at push", id)
	assertLogged(t, logs, "did not reach the remote")
	if !strings.Contains(o.Message, "What I found") {
		t.Errorf("message = %q", o.Message)
	}
}

// A re-mint that fails must not skip the push. The commit leg runs either
// way, and the token already in hand is often still good, so the work
// reaches the branch whenever it is.
func TestRunnerPushesWithTheTokenInHandWhenTheReMintFails(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failPush  error
		wantState resolution.CaseState
		wantKind  resolution.OutcomeKind
	}{
		{name: "the token in hand still works", wantState: resolution.Done, wantKind: resolution.PullRequestOpened},
		{name: "the token in hand is spent too", failPush: errors.New("git: 401 unauthorized"), wantState: resolution.Failed, wantKind: resolution.FailedOutcome},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := storetest.Open(t)
			id := startedAttempt(t, st, resolution.Auto)
			sb := &fakeSandbox{commits: true, failPush: tc.failPush}
			gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}, remintErr: errors.New("github: 500 minting")}
			r, _, logs := newRunner(t, st, sb, gh, scripted(0, goodSummary, nil))
			r.Run(t.Context(), id)
			if len(sb.pushTokens) != 1 || sb.pushTokens[0] != "tok-1" {
				t.Errorf("pushed with %q, want the token already in hand", sb.pushTokens)
			}
			c, err := st.GetCase(t.Context(), "guy/repo", 7)
			if err != nil {
				t.Fatal(err)
			}
			o := c.Attempts()[0].Outcome
			if c.State() != tc.wantState || o == nil || o.Kind != tc.wantKind {
				t.Fatalf("state %s outcome %+v", c.State(), o)
			}
			if tc.failPush != nil {
				if o.Class != resolution.FailureInfra {
					t.Errorf("class = %s", o.Class)
				}
				assertOperatorMessage(t, o.Message, "infrastructure failure at push", id)
				// Both halves reach the operator's log: the push that was
				// refused and the re-mint that is the likely reason why.
				assertLogged(t, logs, "401 unauthorized")
				assertLogged(t, logs, "500 minting")
			}
			assertNoTokenLogged(t, logs, gh)
		})
	}
}

func TestRunnerRefreshesTheDefaultBranch(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{commits: true}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}, branch: "trunk"}
	r, _, _ := newRunner(t, st, sb, gh, scripted(0, goodSummary, nil))
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
	r, m, _ := newRunner(t, st, sb, gh, scripted(0, goodSummary, nil))
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
	r, m, _ := newRunner(t, st, sb, gh, scripted(0, goodSummary, nil))
	r.Run(t.Context(), id)
	if len(sb.order()) != 0 || len(m.all()) != 0 {
		t.Errorf("ran a closed attempt: calls %v metrics %v", sb.order(), m.all())
	}
}

// blocking is a model that answers nothing until its context ends, so a stop
// has something to cancel.
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
	r, _, _ := newRunner(t, st, sb, gh, blocking())
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
		t.Error("a finished attempt reported itself as running")
	}
}

func TestRunnerStopDuringSetupIsAborted(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{blockPrepare: true, preparing: make(chan struct{})}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}}
	r, _, _ := newRunner(t, st, sb, gh, scripted(0, goodSummary, nil))
	done := make(chan struct{})
	go func() { r.Run(t.Context(), id); close(done) }()
	select {
	case <-sb.preparing:
	case <-time.After(5 * time.Second):
		t.Fatal("the workspace was never prepared")
	}
	if !r.Stop(id) {
		t.Fatal("stop did not reach the attempt preparing its workspace")
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
		t.Errorf("a stop during setup was not an abort: state %s outcome %+v", c.State(), o)
	}
	if len(sb.started) != 0 || len(sb.pushed) != 0 {
		t.Errorf("started %v pushed %v", sb.started, sb.pushed)
	}
}

// A shutdown under a running attempt records no outcome: app.Recovery ends
// the attempt with Aborted{DaemonRestart} on the next boot, which re-queues
// the case instead of parking it for a human.
func TestRunnerShutdownLeavesTheAttemptOpen(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{commits: true}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}}
	r, m, _ := newRunner(t, st, sb, gh, blocking())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { r.Run(ctx, id); close(done) }()
	waitFor(t, "the attempt to reach the agent", func() bool { return len(sb.order()) >= 3 })
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not end after the daemon context was cancelled")
	}
	c, err := st.GetCase(t.Context(), "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	a := c.OpenAttempt()
	if a == nil || a.ID != id || c.State() != resolution.Attempting {
		t.Errorf("a shutdown ended the attempt: state %s open attempt %+v", c.State(), a)
	}
	if c.Attempts()[0].Outcome != nil {
		t.Errorf("outcome = %+v", c.Attempts()[0].Outcome)
	}
	// The commits still reach the branch, so the re-queued attempt resumes
	// from them.
	if len(sb.pushed) != 1 || len(sb.torn) != 1 {
		t.Errorf("pushed %v torn %v", sb.pushed, sb.torn)
	}
	if got := m.all(); len(got) != 0 {
		t.Errorf("metrics = %v", got)
	}
}

// A shutdown during setup is the same contract as one during the run: no
// outcome, and the attempt is left for app.Recovery.
func TestRunnerShutdownDuringSetupLeavesTheAttemptOpen(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{blockPrepare: true, preparing: make(chan struct{})}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}}
	r, m, _ := newRunner(t, st, sb, gh, scripted(0, goodSummary, nil))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { r.Run(ctx, id); close(done) }()
	select {
	case <-sb.preparing:
	case <-time.After(5 * time.Second):
		t.Fatal("the workspace was never prepared")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not end after the daemon context was cancelled")
	}
	c, err := st.GetCase(t.Context(), "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	a := c.OpenAttempt()
	if a == nil || a.ID != id || c.State() != resolution.Attempting {
		t.Errorf("a shutdown during setup ended the attempt: state %s open attempt %+v", c.State(), a)
	}
	if got := m.all(); len(got) != 0 {
		t.Errorf("metrics = %v", got)
	}
}

// A run that finished on its own terms keeps its outcome even when the
// daemon starts going down before the push: the work and the tokens are real
// either way, and app.Recovery would otherwise re-queue a case that is done.
func TestRunnerShutdownAfterTheRunRecordsTheOutcome(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The daemon goes down inside the run's own final diff count, the
	// window between the run ending and the runner deciding what it ended
	// as. A live read of the caller there discards a finished run.
	sb := &fakeSandbox{commits: true, onDiff: cancel}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}}
	r, m, _ := newRunner(t, st, sb, gh, scripted(0, goodSummary, nil))
	r.Run(ctx, id)
	c, err := st.GetCase(t.Context(), "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Attempts()[0].Outcome
	if c.State() != resolution.Done || o == nil || o.Kind != resolution.PullRequestOpened {
		t.Fatalf("a finished run lost its outcome to a shutdown: state %s outcome %+v", c.State(), o)
	}
	if got := m.all(); len(got) != 1 || got[0] != "auto/pull_request_opened" {
		t.Errorf("metrics = %v", got)
	}
}

// An empty reason must not read as "nobody asked", which is the shutdown
// signal; a caller that forgets one gets an operator stop.
func TestRunnerCancelWithNoReasonIsAnOperatorStop(t *testing.T) {
	r := &Runner{}
	_, cancel := context.WithCancel(t.Context())
	defer cancel()
	slot := r.register("a1", cancel)
	if !r.Cancel("a1", "") {
		t.Fatal("cancel did not reach the registered attempt")
	}
	if got := r.reason(slot, "unset"); got != resolution.AbortOperatorStop {
		t.Errorf("reason = %q, want %q", got, resolution.AbortOperatorStop)
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
	r, _, _ := newRunner(t, st, sb, gh, blocking())
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

// Conflict markers on the branch are the agent's failure, not a pull
// request: the rebase left them for it to resolve and it committed them
// instead. The branch is still pushed, so the resumed attempt starts from
// them.
func TestRunnerConflictMarkersOpenNoPullRequest(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{commits: true, conflicted: true}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}}
	r, _, _ := newRunner(t, st, sb, gh, scripted(0, goodSummary, nil))
	r.Run(t.Context(), id)
	c, err := st.GetCase(t.Context(), "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Attempts()[0].Outcome
	if c.State() != resolution.Failed || o == nil || o.Kind != resolution.FailedOutcome || o.Class != resolution.FailureAgent {
		t.Fatalf("state %s outcome %+v", c.State(), o)
	}
	if !strings.Contains(o.Message, "unresolved conflict markers") || !strings.Contains(o.Message, "What I found") {
		t.Errorf("message = %q", o.Message)
	}
	if len(gh.pullRequests()) != 0 {
		t.Errorf("a pull request was opened from a branch with conflict markers: %v", gh.pullRequests())
	}
	if len(sb.pushed) != 1 {
		t.Errorf("the work did not reach the branch: %v", sb.pushed)
	}
	// The check is only valid once CommitAndPush has removed the container
	// and made the commits it reads.
	calls := sb.order()
	if index(t, calls, "conflicts") < index(t, calls, "push") {
		t.Errorf("conflict markers were checked before the push: %v", calls)
	}
}

// A check that cannot run must not cost the attempt its outcome: the guard
// sits on top of the agent's own job and a human reads the pull request.
func TestRunnerConflictMarkerCheckFailureStillOpensThePullRequest(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{commits: true, conflictErr: errors.New("git: bad object")}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}}
	r, _, _ := newRunner(t, st, sb, gh, scripted(0, goodSummary, nil))
	r.Run(t.Context(), id)
	c, err := st.GetCase(t.Context(), "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Attempts()[0].Outcome
	if c.State() != resolution.Done || o == nil || o.Kind != resolution.PullRequestOpened {
		t.Fatalf("state %s outcome %+v", c.State(), o)
	}
}

// A model that fails is Failed{Model}, and its error is the operator's to
// read in the log, not the requester's to read on the issue.
func TestRunnerModelErrorIsAModelFailure(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{commits: true}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}}
	broken := jess.Once(true, func(context.Context, []ac.Message, []ac.ToolSpec) (*ac.LLMResponse, error) {
		return nil, errors.New("openrouter: 402 insufficient credits")
	})
	r, m, logs := newRunner(t, st, sb, gh, broken)
	r.Run(t.Context(), id)
	c, err := st.GetCase(t.Context(), "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Attempts()[0].Outcome
	if c.State() != resolution.Failed || o == nil || o.Kind != resolution.FailedOutcome || o.Class != resolution.FailureModel {
		t.Fatalf("state %s outcome %+v", c.State(), o)
	}
	assertOperatorMessage(t, o.Message, "model failure", id)
	assertLogged(t, logs, "402 insufficient credits")
	if strings.Contains(o.Message, "402 insufficient credits") {
		t.Errorf("the raw model error reached the issue comment: %q", o.Message)
	}
	// The work still reaches the branch, and no pull request is opened from
	// a run the model never finished.
	if len(sb.pushed) != 1 || len(gh.pullRequests()) != 0 {
		t.Errorf("pushed %v prs %v", sb.pushed, gh.pullRequests())
	}
	if got := m.all(); len(got) != 1 || got[0] != "auto/failed" {
		t.Errorf("metrics = %v", got)
	}
}
