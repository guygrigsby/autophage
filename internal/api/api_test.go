package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/guygrigsby/autophage/internal/auth"
)

func TestHealthz(t *testing.T) {
	h := New(t.TempDir(), nil, Deps{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
}

func TestMintLoopbackOnly(t *testing.T) {
	h := New(t.TempDir(), nil, Deps{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/mint", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("remote mint: code = %d, want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/auth/mint", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback mint: code = %d, want 200", rec.Code)
	}
	var got struct{ Token string }
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Token == "" {
		t.Error("mint returned empty token")
	}
}

// TestMintRefusesProxiedRequests proves the loopback check is not the whole
// story. Put any reverse proxy in front of the daemon (tailscale serve,
// nginx) and every request arrives from 127.0.0.1, so a Funnel path that
// reached /api would mint an operator token for the public internet. The
// headers such a proxy adds are the evidence that the request is not from
// the machine itself.
func TestMintRefusesProxiedRequests(t *testing.T) {
	for _, header := range []string{"Tailscale-Funnel-Request", "Tailscale-User-Login", "X-Forwarded-For"} {
		h := New(t.TempDir(), nil, Deps{})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/auth/mint", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set(header, "1")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: code = %d, want 403", header, rec.Code)
		}
		var got map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if got["error"] != "forbidden" {
			t.Errorf("%s: error = %q, want forbidden", header, got["error"])
		}
	}
}

func TestWhoamiRequiresToken(t *testing.T) {
	dir := t.TempDir()
	h := New(dir, nil, Deps{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/whoami", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: code = %d, want 401", rec.Code)
	}

	tok, err := auth.Mint(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("with token: code = %d, want 200", rec.Code)
	}
}

// The deploy webhook exists only when wired: a daemon without push-to-deploy
// configured must not answer it.
func TestDeployWebhookRoutedOnlyWhenWired(t *testing.T) {
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	with := New(t.TempDir(), nil, Deps{Deploy: stub})
	r := httptest.NewRequest("POST", "/webhook/deploy", nil)
	w := httptest.NewRecorder()
	with.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Errorf("wired: %d", w.Code)
	}
	without := New(t.TempDir(), nil, Deps{})
	w = httptest.NewRecorder()
	without.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("unwired: %d, want 404", w.Code)
	}
}
