package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

// fakeGitHub answers the handful of endpoints the client uses and records
// what it saw.
type fakeGitHub struct {
	mux           *http.ServeMux
	tokenReqs     []map[string]any
	comments      []map[string]any
	pulls         []map[string]any
	labels        []map[string]any
	labelMissing  bool
	retryOnce     atomic.Bool
	mintRetryOnce atomic.Bool
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *httptest.Server) {
	f := &fakeGitHub{mux: http.NewServeMux(), labelMissing: true}
	f.mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "autophage-test" {
			t.Errorf("token mint user agent = %q", r.Header.Get("User-Agent"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.tokenReqs = append(f.tokenReqs, body)
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "no jwt", http.StatusUnauthorized)
			return
		}
		if f.mintRetryOnce.CompareAndSwap(true, false) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"message":"slow down"}`, http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_test", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	})
	f.mux.HandleFunc("GET /repos/guy/repo/issues/7", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "autophage-test" {
			t.Errorf("user agent = %q", r.Header.Get("User-Agent"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "title": "Typo", "body": "teh", "state": "open", "author_association": "OWNER", "user": map[string]any{"login": "guy"}})
	})
	f.mux.HandleFunc("POST /repos/guy/repo/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		if f.retryOnce.CompareAndSwap(true, false) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"message":"slow down"}`, http.StatusForbidden)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.comments = append(f.comments, body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 99001})
	})
	f.mux.HandleFunc("POST /repos/guy/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.pulls = append(f.pulls, body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 12})
	})
	f.mux.HandleFunc("GET /repos/guy/repo/labels/approved", func(w http.ResponseWriter, r *http.Request) {
		if f.labelMissing {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "approved"})
	})
	f.mux.HandleFunc("POST /repos/guy/repo/labels", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.labels = append(f.labels, body)
		f.labelMissing = false
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(body)
	})
	f.mux.HandleFunc("GET /repos/guy/repo", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"full_name": "guy/repo", "default_branch": "trunk"})
	})
	srv := httptest.NewServer(f.mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := NewClient(ClientConfig{AppID: 1, PrivateKeyPEM: testKey(t), BaseURL: srv.URL + "/", UserAgent: "autophage-test",
		Installations: func(context.Context, string) (int64, error) { return 42, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMintTokenScopedToRepository(t *testing.T) {
	f, srv := newFakeGitHub(t)
	c := newTestClient(t, srv)
	repo, _ := resolution.NewRepository("guy/repo", 42, "main", time.Now())
	tok, err := c.MintToken(t.Context(), repo)
	if err != nil || tok.Value != "ghs_test" || tok.ExpiresAt.Before(time.Now()) {
		t.Fatalf("token = %+v %v", tok, err)
	}
	if len(f.tokenReqs) != 1 {
		t.Fatalf("token requests = %d", len(f.tokenReqs))
	}
	repos, _ := f.tokenReqs[0]["repositories"].([]any)
	if len(repos) != 1 || repos[0] != "repo" {
		t.Errorf("scope = %v", f.tokenReqs[0])
	}
	if branch, err := c.DefaultBranch(t.Context(), repo); err != nil || branch != "trunk" {
		t.Errorf("default branch = %q %v", branch, err)
	}
}

func TestMintTokenRetriesOnRateLimit(t *testing.T) {
	f, srv := newFakeGitHub(t)
	f.mintRetryOnce.Store(true)
	c := newTestClient(t, srv)
	repo, _ := resolution.NewRepository("guy/repo", 42, "main", time.Now())
	tok, err := c.MintToken(t.Context(), repo)
	if err != nil || tok.Value != "ghs_test" {
		t.Fatalf("token = %+v %v", tok, err)
	}
	if len(f.tokenReqs) != 2 {
		t.Fatalf("token requests = %d, want 2", len(f.tokenReqs))
	}
}

// TestRetriesRateLimitWithoutHeaders proves a 429 that carries neither
// Retry-After nor x-ratelimit-reset is still retried after a backoff.
// GitHub's secondary rate limits answer exactly like that, and returning
// the 429 straight through turned every one of them into a failed attempt.
func TestRetriesRateLimitWithoutHeaders(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, `{"message":"slow down"}`, http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(srv.Close)

	tr := &retryTransport{next: http.DefaultTransport, userAgent: "autophage-test"}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 after the retry", resp.StatusCode)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("requests = %d, want 2", n)
	}
	if elapsed := time.Since(start); elapsed < baseBackoff {
		t.Errorf("retried after %s, want at least the %s backoff", elapsed, baseBackoff)
	}
}

// TestRateLimitResetHeader proves x-ratelimit-reset is honored when
// Retry-After is absent, capped so a far-future reset cannot park a request
// for the rest of the hour.
func TestRateLimitResetHeader(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for _, tc := range []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"absent", "", 0},
		{"unparseable", "soon", 0},
		{"in the past", "1699999990", 0},
		{"seven seconds out", "1700000007", 7 * time.Second},
		{"an hour out", "1700003600", 60 * time.Second},
	} {
		if got := rateLimitReset(tc.header, now); got != tc.want {
			t.Errorf("rateLimitReset(%q) = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestGetIssueNotFoundMapsToSentinel(t *testing.T) {
	_, srv := newFakeGitHub(t)
	c := newTestClient(t, srv)
	// Issue 999 has no registered route; the fake mux's default 404 stands
	// in for GitHub's own 404 on an unknown issue number.
	_, err := c.GetIssue(t.Context(), "guy/repo", 999)
	if !errors.Is(err, resolution.ErrIssueNotFound) {
		t.Fatalf("err = %v, want it to wrap resolution.ErrIssueNotFound", err)
	}
}

func TestGetIssueCommentPullLabel(t *testing.T) {
	f, srv := newFakeGitHub(t)
	c := newTestClient(t, srv)
	ctx := t.Context()
	d, err := c.GetIssue(ctx, "guy/repo", 7)
	if err != nil || d.Title != "Typo" || d.Body != "teh" || !d.Open || d.Requester.Login != "guy" || d.Requester.Trust != resolution.Trusted {
		t.Fatalf("issue = %+v %v", d, err)
	}
	f.retryOnce.Store(true)
	id, err := c.PostComment(ctx, "guy/repo", 7, "hello @everyone")
	if err != nil || id != 99001 {
		t.Fatalf("comment = %d %v", id, err)
	}
	if body := f.comments[0]["body"]; body != "hello @\u200beveryone" {
		t.Errorf("mentions not neutralised: %q", body)
	}
	pr, err := c.OpenPullRequest(ctx, "guy/repo", "autophage/7", "main", "autophage: fix @everyone", "Fixes #7 for @guy")
	if err != nil || pr != 12 {
		t.Fatalf("pr = %d %v", pr, err)
	}
	if f.pulls[0]["head"] != "autophage/7" || f.pulls[0]["base"] != "main" {
		t.Errorf("pull = %v", f.pulls[0])
	}
	// The title comes from the issue title, which the requester wrote.
	if f.pulls[0]["title"] != "autophage: fix @\u200beveryone" || f.pulls[0]["body"] != "Fixes #7 for @\u200bguy" {
		t.Errorf("mentions not neutralised on the pull request: %v", f.pulls[0])
	}
	if err := c.EnsureLabel(ctx, "guy/repo", "approved"); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureLabel(ctx, "guy/repo", "approved"); err != nil {
		t.Fatal(err)
	}
	if len(f.labels) != 1 {
		t.Errorf("label created %d times", len(f.labels))
	}
}

// TestForbiddenWithoutThrottleIsNotRetried proves a 403 that is not a rate
// limit (no Retry-After, quota remaining) comes straight back. GitHub sends
// X-RateLimit-Reset on every response, and treating that as a throttle
// parked the enrollment sweep for a minute per attempt on an archived
// repository's permanent 403.
func TestForbiddenWithoutThrottleIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		http.Error(w, `{"message":"Repository was archived so is read-only."}`, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	tr := &retryTransport{next: http.DefaultTransport, userAgent: "autophage-test"}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || calls.Load() != 1 {
		t.Fatalf("status %d after %d calls, want one 403", resp.StatusCode, calls.Load())
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("waited %v on a permanent 403", time.Since(start))
	}
}

// TestForbiddenPrimaryRateLimitIsRetried keeps the throttle case: a 403 with
// the quota exhausted waits for the reset and tries again.
func TestForbiddenPrimaryRateLimitIsRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Second).Unix(), 10))
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(srv.Close)
	tr := &retryTransport{next: http.DefaultTransport, userAgent: "autophage-test"}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("status %d after %d calls, want 200 after a retry", resp.StatusCode, calls.Load())
	}
}

// TestEnsureLabelOnArchivedRepositoryIsReadOnly maps the archived
// repository's 403 to resolution.ErrRepositoryReadOnly so enrollment can
// stop retrying it.
func TestEnsureLabelOnArchivedRepositoryIsReadOnly(t *testing.T) {
	f, srv := newFakeGitHub(t)
	f.mux.HandleFunc("GET /repos/guy/archived/labels/approved", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	f.mux.HandleFunc("POST /repos/guy/archived/labels", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4999")
		http.Error(w, `{"message":"Repository was archived so is read-only."}`, http.StatusForbidden)
	})
	c := newTestClient(t, srv)
	err := c.EnsureLabel(t.Context(), "guy/archived", "approved")
	if !errors.Is(err, resolution.ErrRepositoryReadOnly) {
		t.Fatalf("err = %v, want ErrRepositoryReadOnly", err)
	}
}
