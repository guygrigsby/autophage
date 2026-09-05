// Package api builds appd's HTTP handler: liveness, loopback token mint, an
// auth-gated whoami, and (optionally) the embedded web SPA.
package api

import (
	"encoding/json"
	"io/fs"
	"net/http"

	rootapp "github.com/guygrigsby/rookery"
	"github.com/guygrigsby/rookery/internal/auth"
)

// New returns the appd handler. dir is the per-app config dir (where the auth
// hash lives). static is the embedded SPA filesystem; pass nil (or an FS with
// no index.html) to serve no web UI.
func New(dir string, static fs.FS) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("POST /api/auth/mint", func(w http.ResponseWriter, r *http.Request) {
		if !auth.IsLoopback(r.RemoteAddr) {
			http.Error(w, "mint is loopback-only", http.StatusForbidden)
			return
		}
		token, err := auth.Mint(dir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
	})

	mux.Handle("GET /api/whoami", auth.Middleware(dir, http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"authenticated": true})
		})))

	// Serve the SPA at / only when a real build is embedded.
	if static != nil && rootapp.HasIndex(static) {
		mux.Handle("GET /", http.FileServerFS(static))
	}

	return mux
}
