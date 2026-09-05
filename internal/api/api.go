// Package api builds autophaged's HTTP handler: liveness, loopback token mint,
// an auth-gated whoami, the operator endpoints under /api, the GitHub webhook,
// Prometheus metrics and (optionally) the embedded web SPA.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/guygrigsby/jess/ledger"

	rootapp "github.com/guygrigsby/autophage"
	"github.com/guygrigsby/autophage/internal/auth"
	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
)

// Deps is everything the handlers need. Nil Ledger disables /why with a
// clear error; nil Sweep disables run's immediate sweep.
type Deps struct {
	// Base is the daemon's own context. The detached sweep run starts
	// takes it rather than the request's, so it outlives the response but
	// still ends when the daemon does.
	Base          context.Context
	Store         *store.Store
	GitHub        resolution.GitHub
	Clock         resolution.Clock
	OperatorLogin string
	Webhook       http.Handler
	Sweep         func(context.Context)
	Ledger        ledger.Reader
	Stop          func(attemptID string) bool
	Version       string
	StartedAt     time.Time
	Concurrency   int
}

// New returns the autophaged handler. dir is the per-app config dir (where the auth
// hash lives). static is the embedded SPA filesystem; pass nil (or an FS with
// no index.html) to serve no web UI.
func New(dir string, static fs.FS, d Deps) http.Handler {
	if d.Base == nil {
		d.Base = context.Background()
	}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("POST /api/auth/mint", func(w http.ResponseWriter, r *http.Request) {
		if !auth.IsLoopback(r.RemoteAddr) {
			writeErr(w, fmt.Errorf("%w: mint is loopback only", errForbidden))
			return
		}
		// A reverse proxy in front of the daemon makes every request look
		// loopback, so the address alone does not prove the caller is on
		// this machine. Any of these headers means something forwarded the
		// request, and nothing may forward a mint.
		for _, h := range proxyHeaders {
			if r.Header.Get(h) != "" {
				log.Printf("api: refused a mint carrying %s from %s", h, r.RemoteAddr)
				writeErr(w, fmt.Errorf("%w: mint reached through a proxy", errForbidden))
				return
			}
		}
		token, err := auth.Mint(dir)
		if err != nil {
			writeErr(w, err)
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

	if d.Webhook != nil {
		mux.Handle("POST /webhook/github", d.Webhook)
	}

	if d.Store != nil {
		h := &handlers{d: d}
		mux.Handle("GET /api/status", auth.Middleware(dir, http.HandlerFunc(h.status)))
		mux.Handle("GET /api/cases", auth.Middleware(dir, http.HandlerFunc(h.listCases)))
		mux.Handle("GET /api/cases/{owner}/{repo}/{number}", auth.Middleware(dir, http.HandlerFunc(h.getCase)))
		mux.Handle("POST /api/cases/{owner}/{repo}/{number}/run", auth.Middleware(dir, http.HandlerFunc(h.run)))
		mux.Handle("GET /api/attempts/{id}", auth.Middleware(dir, http.HandlerFunc(h.getAttempt)))
		mux.Handle("POST /api/attempts/{id}/stop", auth.Middleware(dir, http.HandlerFunc(h.stop)))
		mux.Handle("GET /api/attempts/{id}/why", auth.Middleware(dir, http.HandlerFunc(h.why)))
		mux.Handle("GET /metrics", metricsHandler(d.Store))
	}

	// Serve the SPA at / only when a real build is embedded.
	if static != nil && rootapp.HasIndex(static) {
		mux.Handle("GET /", http.FileServerFS(static))
	}

	return mux
}

type handlers struct{ d Deps }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr maps the closed error taxonomy to HTTP.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, resolution.ErrIssueNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	case errors.Is(err, resolution.ErrRefused), errors.Is(err, store.ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "conflict", "detail": err.Error()})
	case errors.Is(err, errForbidden):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "detail": err.Error()})
	case errors.Is(err, resolution.ErrInvalid), errors.Is(err, errInvalidRequest):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "detail": err.Error()})
	case errors.Is(err, errUpstream):
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream_unavailable", "detail": err.Error()})
	default:
		log.Printf("api: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
	}
}

var (
	errInvalidRequest = errors.New("invalid request")
	errUpstream       = errors.New("upstream unavailable")
	errForbidden      = errors.New("forbidden")
)

// proxyHeaders are the headers something in front of the daemon adds. Their
// presence on a loopback request means the request is not from this machine.
var proxyHeaders = []string{"Tailscale-Funnel-Request", "Tailscale-User-Login", "X-Forwarded-For"}
