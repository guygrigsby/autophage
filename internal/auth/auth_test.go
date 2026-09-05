package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestMintPersistsHashAndValidates(t *testing.T) {
	dir := t.TempDir()
	tok, err := Mint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 20 {
		t.Errorf("token looks too short: %q", tok)
	}
	info, err := os.Stat(filepath.Join(dir, "cli-token.hash"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("hash mode = %o, want 600", info.Mode().Perm())
	}
	ok, err := Validate(dir, tok)
	if err != nil || !ok {
		t.Errorf("Validate(minted) = %v, %v; want true, nil", ok, err)
	}
	if ok, _ := Validate(dir, "wrong"); ok {
		t.Error("Validate(wrong) = true; want false")
	}
}

func TestValidateNoHashFileIsFalse(t *testing.T) {
	ok, err := Validate(t.TempDir(), "anything")
	if err != nil {
		t.Fatalf("missing hash must not error: %v", err)
	}
	if ok {
		t.Error("Validate with no hash file = true; want false")
	}
}

func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:5000": true,
		"[::1]:5000":     true,
		"10.0.0.5:5000":  false,
		"":               false,
	}
	for addr, want := range cases {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestMiddleware(t *testing.T) {
	dir := t.TempDir()
	tok, _ := Mint(dir)
	h := Middleware(dir, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: code = %d, want 401", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("valid token: code = %d, want 200", rec.Code)
	}
}
