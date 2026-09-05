package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/guygrigsby/rookery/internal/auth"
)

func TestHealthz(t *testing.T) {
	h := New(t.TempDir(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
}

func TestMintLoopbackOnly(t *testing.T) {
	h := New(t.TempDir(), nil)

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

func TestWhoamiRequiresToken(t *testing.T) {
	dir := t.TempDir()
	h := New(dir, nil)

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
