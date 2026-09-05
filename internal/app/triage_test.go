package app

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
	"github.com/guygrigsby/autophage/internal/storetest"
)

var t0 = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// movingClock is a clock a test advances by hand, for the backoff windows.
// It locks: the dispatcher's goroutine reads it while the test moves it.
type movingClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *movingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *movingClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// countingTriager fails like fakeTriager and counts the calls, so a test can
// prove the backoff kept a paid model call from going out at all.
type countingTriager struct {
	mu  sync.Mutex
	n   int
	err error
}

func (c *countingTriager) Classify(context.Context, string, string) (resolution.Triage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return resolution.Triage{}, c.err
}

func (c *countingTriager) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// fakeGitHub is the port double: canned issues, recorded posts. The
// dispatcher calls it from its own goroutine while a test reads what it
// recorded, so every field is behind the mutex.
type fakeGitHub struct {
	mu       sync.Mutex
	issues   map[string]resolution.IssueDetail
	comments []string
	labels   []string
	prs      int
	failPost bool
	// branch is what DefaultBranch reports. Empty means the repository's
	// own recorded branch, so nothing is refreshed.
	branch    string
	postCalls int
}

func (f *fakeGitHub) GetIssue(_ context.Context, repo string, n int) (resolution.IssueDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.issues[keyOf(repo, n)]
	if !ok {
		return resolution.IssueDetail{}, errors.New("no such issue")
	}
	return d, nil
}
func (f *fakeGitHub) DefaultBranch(_ context.Context, repo resolution.Repository) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.branch == "" {
		return repo.DefaultBranch, nil
	}
	return f.branch, nil
}
func (f *fakeGitHub) MintToken(context.Context, resolution.Repository) (resolution.Token, error) {
	return resolution.Token{Value: "t", ExpiresAt: t0.Add(time.Hour)}, nil
}
func (f *fakeGitHub) PostComment(_ context.Context, _ string, _ int, body string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.postCalls++
	if f.failPost {
		return 0, errors.New("github down")
	}
	f.comments = append(f.comments, body)
	return int64(len(f.comments)), nil
}
func (f *fakeGitHub) OpenPullRequest(context.Context, string, string, string, string, string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs++
	return f.prs, nil
}
func (f *fakeGitHub) EnsureLabel(_ context.Context, repo, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labels = append(f.labels, repo+":"+name)
	return nil
}

func (f *fakeGitHub) setIssue(repo string, n int, d resolution.IssueDetail) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[keyOf(repo, n)] = d
}

func (f *fakeGitHub) setFailPost(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failPost = v
}

func (f *fakeGitHub) postedComments() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.comments...)
}

func (f *fakeGitHub) ensuredLabels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.labels...)
}

func (f *fakeGitHub) postAttempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.postCalls
}

func keyOf(repo string, n int) string { return repo + "#" + strconv.Itoa(n) }

type fakeTriager struct {
	size resolution.Size
	err  error
}

func (f fakeTriager) Classify(context.Context, string, string) (resolution.Triage, error) {
	if f.err != nil {
		return resolution.Triage{}, f.err
	}
	return resolution.Triage{Size: f.size, Rationale: "because", Model: "fake", TriagedAt: t0}, nil
}

func seed(t *testing.T, st *store.Store) *resolution.Case {
	t.Helper()
	r, _ := resolution.NewRepository("guy/repo", 42, "main", t0)
	if err := st.EnrollRepository(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	req, _ := resolution.NewRequester("guy", resolution.AssociationOwner)
	c, _ := resolution.NewCase("guy/repo", 7, req, t0)
	if err := st.CreateCase(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func issue(open bool) resolution.IssueDetail {
	req, _ := resolution.NewRequester("guy", resolution.AssociationOwner)
	return resolution.IssueDetail{Title: "Typo", Body: "teh", Requester: req, Open: open}
}

func TestTriageSizesReceivedCases(t *testing.T) {
	st := storetest.Open(t)
	seed(t, st)
	gh := &fakeGitHub{issues: map[string]resolution.IssueDetail{keyOf("guy/repo", 7): issue(true)}}
	tr := &Triage{Store: st, Triager: fakeTriager{size: resolution.Large}, GitHub: gh, Clock: fixedClock{t0}}
	if err := tr.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	c, _ := st.GetCase(t.Context(), "guy/repo", 7)
	if c.State() != resolution.AwaitingApproval || c.Triage() == nil || c.Triage().Rationale != "because" {
		t.Errorf("case = %s %+v", c.State(), c.Triage())
	}
}

func TestTriageRetriesAfterModelFailure(t *testing.T) {
	st := storetest.Open(t)
	seed(t, st)
	gh := &fakeGitHub{issues: map[string]resolution.IssueDetail{keyOf("guy/repo", 7): issue(true)}}
	clock := &movingClock{t: t0}
	tr := &Triage{Store: st, Triager: fakeTriager{err: errors.New("model down")}, GitHub: gh, Clock: clock, Model: "fake/triage"}
	if err := tr.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	c, _ := st.GetCase(t.Context(), "guy/repo", 7)
	if c.State() != resolution.Received {
		t.Fatalf("state moved on failure: %s", c.State())
	}
	tr.Triager = fakeTriager{size: resolution.Small}
	clock.advance(triageBackoff)
	_ = tr.Run(t.Context())
	c, _ = st.GetCase(t.Context(), "guy/repo", 7)
	if c.State() != resolution.Queued {
		t.Errorf("not retried: %s", c.State())
	}
}

// A model call that keeps failing must not be retried on every sweep: it is
// a paid call, and the production floor is a minute.
func TestTriageBacksOffAFailingModel(t *testing.T) {
	st := storetest.Open(t)
	seed(t, st)
	gh := &fakeGitHub{issues: map[string]resolution.IssueDetail{keyOf("guy/repo", 7): issue(true)}}
	clock := &movingClock{t: t0}
	counted := &countingTriager{err: errors.New("model down")}
	tr := &Triage{Store: st, Triager: counted, GitHub: gh, Clock: clock, Model: "fake/triage"}
	if err := tr.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Every sweep inside the first backoff skips the case entirely.
	for range 5 {
		clock.advance(triageBackoff / 10)
		if err := tr.Run(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := counted.calls(); got != 1 {
		t.Errorf("the model was called %d times inside the backoff, want 1", got)
	}
	clock.advance(triageBackoff)
	if err := tr.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := counted.calls(); got != 2 {
		t.Errorf("the model was called %d times, want a retry once the backoff passed", got)
	}
	// The second failure doubles the wait, so the first backoff's length is
	// no longer enough.
	clock.advance(triageBackoff)
	if err := tr.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := counted.calls(); got != 2 {
		t.Errorf("the model was called %d times, want the doubled backoff to hold", got)
	}
	if c, _ := st.GetCase(t.Context(), "guy/repo", 7); c.State() != resolution.Received {
		t.Errorf("state = %s, want the case to stay Received while it backs off", c.State())
	}
}

// After enough failures the case parks for a human rather than backing off
// for ever: Received is a state nothing surfaces.
func TestTriageParksACaseTheModelWillNotSize(t *testing.T) {
	st := storetest.Open(t)
	seed(t, st)
	gh := &fakeGitHub{issues: map[string]resolution.IssueDetail{keyOf("guy/repo", 7): issue(true)}}
	clock := &movingClock{t: t0}
	tr := &Triage{Store: st, Triager: fakeTriager{err: errors.New("openrouter: 402 insufficient credits")}, GitHub: gh, Clock: clock, Model: "fake/triage"}
	for i := range triageGiveUp {
		if err := tr.Run(t.Context()); err != nil {
			t.Fatal(err)
		}
		c, _ := st.GetCase(t.Context(), "guy/repo", 7)
		if i < triageGiveUp-1 && c.State() != resolution.Received {
			t.Fatalf("parked after %d failures: %s", i+1, c.State())
		}
		clock.advance(triageBackoffCap)
	}
	c, _ := st.GetCase(t.Context(), "guy/repo", 7)
	if c.State() != resolution.AwaitingApproval || c.Triage() == nil {
		t.Fatalf("state %s triage %+v, want the case parked for a human", c.State(), c.Triage())
	}
	got := *c.Triage()
	if got.Size != resolution.Large || got.Model != "fake/triage" {
		t.Errorf("triage = %+v", got)
	}
	if !strings.Contains(got.Rationale, "triage failed 5 times") || !strings.Contains(got.Rationale, "402 insufficient credits") {
		t.Errorf("rationale = %q", got.Rationale)
	}
}
