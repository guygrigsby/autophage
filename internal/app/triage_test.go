package app

import (
	"context"
	"errors"
	"strconv"
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
	tr := &Triage{Store: st, Triager: fakeTriager{err: errors.New("model down")}, GitHub: gh, Clock: fixedClock{t0}}
	if err := tr.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	c, _ := st.GetCase(t.Context(), "guy/repo", 7)
	if c.State() != resolution.Received {
		t.Fatalf("state moved on failure: %s", c.State())
	}
	tr.Triager = fakeTriager{size: resolution.Small}
	_ = tr.Run(t.Context())
	c, _ = st.GetCase(t.Context(), "guy/repo", 7)
	if c.State() != resolution.Queued {
		t.Errorf("not retried: %s", c.State())
	}
}
