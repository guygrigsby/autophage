//go:build e2e

package e2e

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// The repository, issue and installation the whole test is about. The fake's
// routes are built from them, so a change here moves the endpoints with it.
const (
	repository     = "guy/repo"
	issueNumber    = 1
	installationID = 42
	issueTitle     = "Add FIX.md"
	issueBody      = "The repository needs a FIX.md file at its root saying the fix landed."
	approvedLabel  = "approved"
	requesterLogin = "guy"
)

// testKey is the App private key the client signs its JWT with. Generated
// per run: nothing here is a real credential.
func testKey(t *testing.T) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

// fakeGitHub answers the endpoints internal/github's client calls and records
// what it saw. It is the shape of internal/github/client_test.go's double,
// copied because a test file cannot be imported across packages, plus the
// recording the pull request assertion needs. Every recorded slice is read
// from the test goroutine while the daemon's goroutines write it, so all of
// it is under the mutex.
type fakeGitHub struct {
	mu       sync.Mutex
	tokens   []string
	pulls    []map[string]any
	comments []map[string]any
	labels   []map[string]any
	// labelMissing is the state EnsureLabel drives: the first GET is a 404,
	// the POST that follows creates it, and later GETs find it.
	labelMissing bool
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *httptest.Server) {
	t.Helper()
	f := &fakeGitHub{labelMissing: true}
	mux := http.NewServeMux()

	mux.HandleFunc(fmt.Sprintf("POST /app/installations/%d/access_tokens", installationID), func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, `{"message":"no jwt"}`, http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		token := fmt.Sprintf("ghs_e2e_%d", len(f.tokens)+1)
		f.tokens = append(f.tokens, token)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": token, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	})

	mux.HandleFunc("GET /repos/"+repository, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"full_name": repository, "default_branch": defaultBranch})
	})

	mux.HandleFunc(fmt.Sprintf("GET /repos/%s/issues/%d", repository, issueNumber), func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": issueNumber, "title": issueTitle, "body": issueBody, "state": "open",
			"author_association": "OWNER", "user": map[string]any{"login": requesterLogin},
		})
	})

	mux.HandleFunc(fmt.Sprintf("POST /repos/%s/issues/%d/comments", repository, issueNumber), func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.comments = append(f.comments, body)
		id := int64(90000 + len(f.comments))
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
	})

	mux.HandleFunc("POST /repos/"+repository+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.pulls = append(f.pulls, body)
		number := 100 + len(f.pulls)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": number})
	})

	mux.HandleFunc(fmt.Sprintf("GET /repos/%s/labels/%s", repository, approvedLabel), func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		missing := f.labelMissing
		f.mu.Unlock()
		if missing {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": approvedLabel})
	})

	mux.HandleFunc("POST /repos/"+repository+"/labels", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.labels = append(f.labels, body)
		f.labelMissing = false
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(body)
	})

	// Anything else is a route the daemon needs and this fake does not serve:
	// say so in the log rather than letting the mux's bare 404 look like a
	// GitHub "not found" answer the client is meant to handle.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fake github: unrouted %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeGitHub) pullRequests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.pulls...)
}

func (f *fakeGitHub) mintedTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tokens...)
}
