package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/storetest"
)

// recordRequests records every hook the retry transport fires. The transport
// is shared by the token mint and the repository calls, so a client test
// drives it from more than one goroutine in principle: it locks.
type recordRequests struct {
	mu        sync.Mutex
	requests  []string
	retries   []string
	remaining []int
	timed     int
}

func (r *recordRequests) Request(method, status string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d > 0 {
		r.timed++
	}
	r.requests = append(r.requests, method+" "+status)
}

func (r *recordRequests) Retry(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retries = append(r.retries, reason)
}

func (r *recordRequests) RateLimitRemaining(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.remaining = append(r.remaining, n)
}

func (r *recordRequests) timedCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.timed
}

func (r *recordRequests) all() ([]string, []string, []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...), append([]string(nil), r.retries...), append([]int(nil), r.remaining...)
}

// rtFunc is a RoundTripper made of a function, so a test can answer with an
// exact status, header set or transport error.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func response(code int, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(strings.NewReader("{}"))}
}

func TestTransportRecordsEveryResponse(t *testing.T) {
	m := &recordRequests{}
	tr := &retryTransport{next: rtFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, http.Header{"X-Ratelimit-Remaining": []string{"4321"}}), nil
	}), userAgent: "autophage-test", metrics: m}

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.com/repos/guy/repo", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	requests, retries, remaining := m.all()
	if len(requests) != 1 || requests[0] != "GET 200" {
		t.Errorf("requests = %v, want [GET 200]", requests)
	}
	if len(retries) != 0 {
		t.Errorf("retries = %v, want none", retries)
	}
	if len(remaining) != 1 || remaining[0] != 4321 {
		t.Errorf("rate limit remaining = %v, want [4321]", remaining)
	}
	if m.timedCalls() != 1 {
		t.Errorf("the request was not timed: %d", m.timedCalls())
	}
}

// A transport error never becomes a response, so it is counted under the
// status "error": the alternative is a request the dashboard cannot see at
// all, which is the failure an operator most wants to find.
func TestTransportRecordsATransportError(t *testing.T) {
	m := &recordRequests{}
	tr := &retryTransport{next: rtFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	}), metrics: m}

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.github.com/repos/guy/repo/issues/7/comments", nil)
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatal("want the transport error back")
	}
	if requests, _, _ := m.all(); len(requests) != 1 || requests[0] != "POST error" {
		t.Errorf("requests = %v, want [POST error]", requests)
	}
}

func TestTransportCountsARetry(t *testing.T) {
	m := &recordRequests{}
	var n int
	tr := &retryTransport{next: rtFunc(func(*http.Request) (*http.Response, error) {
		n++
		if n == 1 {
			return response(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"1"}}), nil
		}
		return response(http.StatusOK, nil), nil
	}), metrics: m}

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.com/repos/guy/repo", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	requests, retries, _ := m.all()
	if strings.Join(requests, ",") != "GET 429,GET 200" {
		t.Errorf("requests = %v", requests)
	}
	if len(retries) != 1 || retries[0] != reasonRetryAfter {
		t.Errorf("retries = %v, want [%s]", retries, reasonRetryAfter)
	}
}

// The reason label is the header that decided the wait. Table-driven rather
// than three retrying transports, because each of those would sleep the
// wait it is asserting on.
func TestRetryWaitNamesTheReason(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for _, tc := range []struct {
		name   string
		header http.Header
		reason string
		wait   time.Duration
	}{
		{"retry after", http.Header{"Retry-After": []string{"7"}}, reasonRetryAfter, 7 * time.Second},
		{"rate limit reset", http.Header{"X-Ratelimit-Reset": []string{"1700000030"}}, reasonRateLimit, 30 * time.Second},
		{"neither", http.Header{}, reasonBackoff, baseBackoff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wait, reason := retryWait(response(http.StatusForbidden, tc.header), now, 0)
			if reason != tc.reason || wait != tc.wait {
				t.Errorf("retryWait = %s %s, want %s %s", wait, reason, tc.wait, tc.reason)
			}
		})
	}
}

// The transport is built inside NewClient, so the only way an operator gets
// these numbers is ClientConfig carrying the metrics down to it.
func TestClientConfigPlumbsRequestMetrics(t *testing.T) {
	_, srv := newFakeGitHub(t)
	m := &recordRequests{}
	c, err := NewClient(ClientConfig{AppID: 1, PrivateKeyPEM: testKey(t), BaseURL: srv.URL + "/", UserAgent: "autophage-test",
		Installations: func(context.Context, string) (int64, error) { return 42, nil }, Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetIssue(t.Context(), "guy/repo", 7); err != nil {
		t.Fatal(err)
	}
	requests, _, _ := m.all()
	if strings.Join(requests, ",") != "POST 201,GET 200" {
		t.Errorf("requests = %v, want the token mint and the issue read", requests)
	}
}

// recordDeliveries records the translator's per-processing-row hook.
type recordDeliveries struct{ rows []string }

func (r *recordDeliveries) Delivery(event, result string) {
	r.rows = append(r.rows, event+"/"+result)
}

func TestTranslatorCountsEveryProcessingRow(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	m := &recordDeliveries{}
	tr := newTranslator(st)
	tr.Metrics = m

	deliver(t, st, "d-inst", "installation", "installation_created.json")
	deliver(t, st, "d-open", "issues", "issues_opened.json")
	if err := tr.ProcessPending(ctx); err != nil {
		t.Fatal(err)
	}
	// Pending deliveries come back oldest first and by id inside a
	// timestamp, so the close is processed before the second open.
	deliver(t, st, "d-open-again", "issues", "issues_opened.json")
	deliver(t, st, "d-close", "issues", "issues_closed.json")
	if err := tr.ProcessPending(ctx); err != nil {
		t.Fatal(err)
	}
	deliver(t, st, "d-close-again", "issues", "issues_closed.json")
	if err := tr.ProcessPending(ctx); err != nil {
		t.Fatal(err)
	}
	want := "installation/translated,issues/translated,issues/translated,issues/ignored,issues/rejected"
	if strings.Join(m.rows, ",") != want {
		t.Errorf("deliveries = %v, want %s", m.rows, want)
	}
}
