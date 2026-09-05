package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/auth"
	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
)

var t0 = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// fakeGitHub is the port double: canned issues, recorded posts. Copied from
// internal/app's tests: two test doubles in two packages is acceptable, a
// shared testutil is not worth it for a struct this size.
type fakeGitHub struct {
	issues   map[string]resolution.IssueDetail
	comments []string
	labels   []string
	prs      int
	failPost bool
}

func (f *fakeGitHub) GetIssue(_ context.Context, repo string, n int) (resolution.IssueDetail, error) {
	d, ok := f.issues[keyOf(repo, n)]
	if !ok {
		return resolution.IssueDetail{}, resolution.ErrIssueNotFound
	}
	return d, nil
}
func (f *fakeGitHub) MintToken(context.Context, resolution.Repository) (resolution.Token, error) {
	return resolution.Token{Value: "t", ExpiresAt: t0.Add(time.Hour)}, nil
}
func (f *fakeGitHub) PostComment(_ context.Context, _ string, _ int, body string) (int64, error) {
	if f.failPost {
		return 0, errors.New("github down")
	}
	f.comments = append(f.comments, body)
	return int64(len(f.comments)), nil
}
func (f *fakeGitHub) OpenPullRequest(context.Context, string, string, string, string, string) (int, error) {
	f.prs++
	return f.prs, nil
}
func (f *fakeGitHub) EnsureLabel(_ context.Context, repo, name string) error {
	f.labels = append(f.labels, repo+":"+name)
	return nil
}

func keyOf(repo string, n int) string { return repo + "#" + strconv.Itoa(n) }

func newServer(t *testing.T, st *store.Store, gh resolution.GitHub) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	h := New(dir, nil, Deps{Store: st, GitHub: gh, Clock: fixedClock{t0}, OperatorLogin: "guy", Version: "test", StartedAt: t0, Concurrency: 2,
		Stop: func(string) bool { return true }})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tok, err := auth.Mint(dir)
	if err != nil {
		t.Fatal(err)
	}
	return srv, tok
}

func do(t *testing.T, srv *httptest.Server, tok, method, path string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func TestStatusCasesRunAndStop(t *testing.T) {
	st := store.OpenTest(t)
	ctx := t.Context()
	r, _ := resolution.NewRepository("guy/repo", 42, "main", t0)
	if err := st.EnrollRepository(ctx, r); err != nil {
		t.Fatal(err)
	}
	req, _ := resolution.NewRequester("guy", resolution.AssociationOwner)
	gh := &fakeGitHub{issues: map[string]resolution.IssueDetail{"guy/repo#9": {Title: "T", Body: "B", Requester: req, Open: true}}}
	srv, tok := newServer(t, st, gh)

	if code, _ := do(t, srv, "", http.MethodGet, "/api/status"); code != http.StatusUnauthorized {
		t.Errorf("no token = %d", code)
	}
	code, body := do(t, srv, tok, http.MethodGet, "/api/status")
	if code != http.StatusOK || body["concurrency"].(float64) != 2 {
		t.Errorf("status = %d %v", code, body)
	}

	code, body = do(t, srv, tok, http.MethodPost, "/api/cases/guy/repo/9/run")
	if code != http.StatusAccepted || body["state"] != "received" {
		t.Fatalf("run new = %d %v", code, body)
	}
	code, _ = do(t, srv, tok, http.MethodPost, "/api/cases/guy/repo/9/run")
	if code != http.StatusAccepted {
		t.Errorf("run received again = %d (approval recorded, still received)", code)
	}
	code, _ = do(t, srv, tok, http.MethodPost, "/api/cases/guy/nope/1/run")
	if code != http.StatusNotFound {
		t.Errorf("run unenrolled = %d", code)
	}

	c, _ := st.GetCase(ctx, "guy/repo", 9)
	if len(c.Approvals()) != 2 || c.Approvals()[0].Source != resolution.SourceOperator {
		t.Errorf("approvals = %+v", c.Approvals())
	}
	code, body = do(t, srv, tok, http.MethodGet, "/api/cases?state=received")
	if code != http.StatusOK || len(body["cases"].([]any)) != 1 {
		t.Errorf("list = %d %v", code, body)
	}
	code, _ = do(t, srv, tok, http.MethodGet, "/api/cases?state=limbo")
	if code != http.StatusBadRequest {
		t.Errorf("bad state = %d", code)
	}
	code, body = do(t, srv, tok, http.MethodGet, "/api/cases/guy/repo/9")
	if code != http.StatusOK || body["branch"] != "autophage/9" {
		t.Errorf("get = %d %v", code, body)
	}

	b, _ := resolution.NewBudget(10, time.Hour, 500)
	_, _ = st.UpdateCase(ctx, "guy/repo", 9, func(c *resolution.Case) error {
		return c.RecordTriage(resolution.Triage{Size: resolution.Small, Rationale: "r", Model: "m", TriagedAt: t0})
	})
	c, _ = st.UpdateCase(ctx, "guy/repo", 9, func(c *resolution.Case) error {
		_, err := c.StartAttempt(resolution.Auto, b, "brief", t0)
		return err
	})
	id := c.OpenAttempt().ID
	code, _ = do(t, srv, tok, http.MethodPost, "/api/cases/guy/repo/9/run")
	if code != http.StatusConflict {
		t.Errorf("run while attempting = %d", code)
	}
	code, body = do(t, srv, tok, http.MethodGet, "/api/attempts/"+id)
	if code != http.StatusOK || body["ordinal"].(float64) != 1 {
		t.Errorf("attempt = %d %v", code, body)
	}
	code, _ = do(t, srv, tok, http.MethodPost, "/api/attempts/"+id+"/stop")
	if code != http.StatusAccepted {
		t.Errorf("stop = %d", code)
	}
	code, _ = do(t, srv, tok, http.MethodGet, "/api/attempts/"+id+"/why")
	if code != http.StatusNotFound {
		t.Errorf("why without run = %d", code)
	}
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 1<<16)
	n, _ := resp.Body.Read(raw)
	_ = resp.Body.Close()
	if !strings.Contains(string(raw[:n]), "autophage_cases{state=\"attempting\"} 1") {
		t.Errorf("metrics lack the state gauge:\n%s", raw[:n])
	}
}

// TestRunMapsIssueNotFoundToNotFound proves an issue absent on GitHub
// (GetIssue's ErrIssueNotFound) maps to not_found, not upstream_unavailable:
// only a genuine upstream failure (network, auth, rate limit) should read as
// upstream_unavailable.
func TestRunMapsIssueNotFoundToNotFound(t *testing.T) {
	st := store.OpenTest(t)
	ctx := t.Context()
	r, _ := resolution.NewRepository("guy/repo", 42, "main", t0)
	if err := st.EnrollRepository(ctx, r); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{issues: map[string]resolution.IssueDetail{}}
	srv, tok := newServer(t, st, gh)

	code, body := do(t, srv, tok, http.MethodPost, "/api/cases/guy/repo/404/run")
	if code != http.StatusNotFound || body["error"] != "not_found" {
		t.Errorf("run missing issue = %d %v", code, body)
	}
}

// TestRunRefusesClosedIssueOrRemovedRepository proves run is a conflict, not
// a silent success, for the two cases the aggregate itself cannot check
// before the GitHub call: an issue already closed upstream, and a repository
// no longer enrolled.
func TestRunRefusesClosedIssueOrRemovedRepository(t *testing.T) {
	st := store.OpenTest(t)
	ctx := t.Context()
	r, _ := resolution.NewRepository("guy/repo", 42, "main", t0)
	if err := st.EnrollRepository(ctx, r); err != nil {
		t.Fatal(err)
	}
	req, _ := resolution.NewRequester("guy", resolution.AssociationOwner)
	gh := &fakeGitHub{issues: map[string]resolution.IssueDetail{"guy/repo#5": {Title: "T", Body: "B", Requester: req, Open: false}}}
	srv, tok := newServer(t, st, gh)

	code, body := do(t, srv, tok, http.MethodPost, "/api/cases/guy/repo/5/run")
	if code != http.StatusConflict || body["error"] != "conflict" {
		t.Errorf("run closed issue = %d %v", code, body)
	}
	if _, err := st.GetCase(ctx, "guy/repo", 5); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("closed-issue run should not have created a case: err = %v", err)
	}

	if err := st.RemoveRepository(ctx, "guy/repo", t0); err != nil {
		t.Fatal(err)
	}
	code, body = do(t, srv, tok, http.MethodPost, "/api/cases/guy/repo/5/run")
	if code != http.StatusConflict || body["error"] != "conflict" {
		t.Errorf("run removed repository = %d %v", code, body)
	}
}
