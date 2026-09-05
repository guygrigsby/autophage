# Rookery — design

**Date:** 2026-05-27
**Status:** approved (brainstorm), pending implementation plan
**Repo:** `github.com/guygrigsby/rookery` (private), GitHub "Use this template"

## Context

Sub-project 2 of 2 in the scaffold-dedup effort (sub-project 1 = the `perch`
library, shipped at `github.com/guygrigsby/perch` v0.1.0). rookery is the
project template new grigsby apps start from: a two-binary daemon/CLI app
(`appd` + `appctl`) that consumes perch, with an optional Vite+Svelte web
layer and the Makefile / launchd / dev-watch tooling already proven in pluma
and talon.

Audience is private grigsby projects, and the **primary consumer is an AI
agent** (Claude Code). Agentic affordances are first-class, not an
afterthought: clear orientation, a one-shot quality gate, issue tracking, and
CI that gives agent-authored PRs a hard pass/fail signal.

## Goals

- A `Use this template` repo that, after `scripts/init.sh <name>`, yields a
  buildable, testable two-binary app with `make build && make test` green.
- Consume perch for the transport/config/daemon plumbing — do not reimplement.
- Ship the auth half perch omitted (loopback token mint + validate) as
  customizable starter code.
- Be agent-ready out of the box (see Agentic Affordances).

## Non-goals

- Not OSS-generic; conventions are hardcoded (`dev.grigsby.*`, `~/.config`,
  `~/.logs`).
- Not a web framework; the SPA is a minimal status page, not a UI kit.
- No multi-app monorepo support; one app per repo.

## Architecture

Two binaries under `cmd/`, both thin wrappers over perch:

- **`appd`** — the daemon. Loads config (`perch/config`), resolves the listen
  address (`perch/daemon.ResolveAddr`, default `:8080`), mounts the API and
  (when present) the embedded web SPA, and runs under
  `perch/daemon.SignalContext` + `perch/daemon.Serve` for graceful shutdown.
- **`appctl`** — the CLI client. Built on `perch/client.Root(...)`; subcommands
  use `perch/client.ResolveToken` + `perch/client.Client` to call `appd`.

`appd`/`appctl` are the template's placeholder names; `init.sh` renames them
per app. The perch dependency is pinned in `go.mod` (`require
github.com/guygrigsby/perch v0.1.0`). perch is public, so `go get` resolves
it through the module proxy with no auth — no `GOPRIVATE` or token setup.

## Components & file structure

```
cmd/appd/main.go          # wire config -> mux -> daemon.Serve
cmd/appctl/main.go        # perch client.Root + register subcommands
internal/api/api.go       # http.Handler: routes + mounting
internal/api/api_test.go
internal/auth/auth.go     # token mint, hash-persist, validate, loopback guard
internal/auth/auth_test.go
web/                      # Vite+Svelte SPA (optional)
  package.json, vite.config.ts, src/, index.html
  dist/.gitkeep           # committed so //go:embed + `go build ./...` work pre-build
embed.go                  # //go:embed all:web/dist ; exposes the static FS
deploy/dev.grigsby.appd.plist.template
scripts/dev-watch.sh      # rebuild appd + bounce on Go change
scripts/init.sh           # rename/retarget the template (see contract below)
Makefile
config.example.toml
go.mod                    # module github.com/guygrigsby/rookery
CLAUDE.md                 # agent project instructions
AGENTS.md                 # symlink -> CLAUDE.md
.golangci.yml
.github/workflows/ci.yml
README.md
```

Each file has one responsibility; `internal/api` owns routing/handlers,
`internal/auth` owns the token lifecycle, `cmd/*` own only wiring.

### appd routes (`internal/api`)

- `GET /healthz` — no auth; liveness (`200 ok`). Agent/probe verification hook.
- `POST /api/auth/mint` — **loopback-only** (rejects any request whose
  `RemoteAddr` is not 127.0.0.1/::1), no bearer required; calls
  `auth.Mint`, returns the plaintext token once as JSON.
- `GET /api/whoami` — bearer-auth'd via `auth` middleware; returns a small JSON
  identity payload. Exists to demonstrate the validate path end to end.
- `GET /` + assets — serves the embedded `web/dist` FS when the web build is
  present; in `--no-web` builds this route is absent.

### internal/auth

- `Mint(dir string) (token string, err error)` — generate 32 random bytes,
  base64url-encode as the token, write its SHA-256 hash to
  `<dir>/cli-token.hash` (mode 0600), return the plaintext once.
- `Validate(dir, token string) (bool, error)` — hash the presented token and
  constant-time-compare against the stored hash; false when no hash file.
- `Middleware(dir string, next http.Handler) http.Handler` — extracts the
  `Authorization: Bearer` token, 401s when `Validate` is false.
- `IsLoopback(remoteAddr string) bool` — guard used by the mint route.
- `dir` is `perch/config.Dir(app)`; auth state lives beside the CLI token.

### appctl subcommands (exercise the whole loop)

- `auth login` — `POST /api/auth/mint`, write the returned token to
  `perch/client.TokenPath(app)` (0600), print confirmation.
- `auth logout` — remove the token file.
- `whoami` — `GET /api/whoami` with `ResolveToken`, print the response.

### web (optional)

Minimal Vite+Svelte SPA: one page that fetches `/api/whoami` (or `/healthz`)
and renders status. Built to `web/dist`, embedded by `embed.go` via
`//go:embed all:web/dist`. A committed `web/dist/.gitkeep` keeps the embed
directory present so `go build ./...` (and the init.sh smoke test) succeed
without a prior `web-build`; `make web-build` populates the real assets.
`embed.go` exposes the static FS through an accessor; `appd` mounts `/` only
when the FS contains a real `index.html`, so an unpopulated (gitkeep-only)
dist serves no stale page. `init.sh --no-web` deletes `web/` and overwrites
`embed.go` with a stub whose accessor returns an empty FS (no `//go:embed`
directive, so the missing `web/` does not break the build) — no build tags
involved.

## init.sh contract

`scripts/init.sh <name> [--no-web]`:

1. Rewrite the module path `github.com/guygrigsby/rookery` →
   `github.com/guygrigsby/<name>` across `go.mod` and all `.go` imports.
2. Rename `cmd/appd` → `cmd/<name>d`, `cmd/appctl` → `cmd/<name>ctl`.
3. Replace the perch `app` id string (the literal `"app"` passed to
   `client.Root`/`config.Dir`/`auth`) with `<name>`.
4. Retarget tooling: `dev.grigsby.appd` → `dev.grigsby.<name>d` (plist label +
   filename), log dir `~/.logs/app` → `~/.logs/<name>`, and the `APP`/`BINARY`
   vars + binary names in the Makefile, `dev-watch.sh`, and plist template.
5. Substitute the app name in `CLAUDE.md`.
6. Re-initialize issue tracking: remove the template's `.beads/`, run
   `bd init` for a clean per-app tracker (skip with a notice if `bd` absent).
7. `--no-web`: `rm -rf web/` and overwrite `embed.go` with the empty-FS stub
   (no `//go:embed`), so the missing `web/` cannot break the build.
8. Remove the template's own `docs/specs` + `docs/superpowers` and re-init git
   history is **out of scope** — leave docs; the user prunes if desired.

Success criterion: immediately after `init.sh foo`, `make build && make test`
pass (both web and `--no-web`).

## Makefile

pluma's spine generalized over `APP ?= app` / `BINARY = $(APP)` etc., now with
two binaries:

- `build` — `web-build` (if web present) + `server-build`; produces `appd`
  embedding the web FS.
- `server-build` (`cmd/appd`), `ctl-build` (`cmd/appctl`), `web-build`,
  `web-dev`.
- `install` — copies **both** `appd` and `appctl` to `INSTALL_DIR`
  (`~/.local/bin`).
- `install-launchd` / `uninstall-launchd` / `redeploy` / `service-restart` —
  exactly the pluma launchd targets; plist points at `appd`.
- `dev` — `dev-watch.sh` (rebuild+bounce appd) alongside `web-dev` (Vite).
- `test` — `server-test` + `web-test`.
- `check` — the one-shot agent gate (see below).
- `clean`.

All targets carry `## ` help text; `help` greps them.

## Agentic affordances

The primary consumer is an agent, so:

- **`CLAUDE.md` + `AGENTS.md`** — orientation stub: build/test/run/check
  commands, an architecture map (appd/appctl/perch/auth/web), conventions, and
  the mandatory session/PR protocol (run `make check`; commit; push; never
  leave work unpushed). `AGENTS.md` is a symlink to `CLAUDE.md` for cross-tool
  support. `init.sh` substitutes the app name.
- **`make check`** — one target an agent runs before claiming done:
  `gofmt -l .` (fail if it prints anything), `go vet ./...`, `go test ./...`,
  `golangci-lint run` (if installed), and `web-build` when web is present.
  `.golangci.yml` ships a modest, non-pedantic config.
- **bd (beads)** — documented in `CLAUDE.md` as the issue tracker (no
  TodoWrite/markdown), with `init.sh` giving each app a fresh `.beads/`.
- **GitHub Actions CI** — `.github/workflows/ci.yml`: `setup-go` 1.26 +
  `setup-node`, then `make check`. perch is public, so no `GOPRIVATE`, secret,
  or git-insteadOf setup is needed — `go build` resolves the dep through the
  proxy. CI mirrors `make check` so the precommit rule has teeth.

## Conventions (inherited from perch)

- Config: `~/.config/<app>/config.toml`; token: `~/.config/<app>/cli.token`
  (0600); auth hash: `~/.config/<app>/cli-token.hash` (0600).
- Logs: `~/.logs/<app>/`. Env: `<APP>_ADDR_URL`, `<APP>_API_TOKEN`.
- Default listen `:8080`; the app owns its port.

## Testing

- **Go unit:** `internal/auth` (mint writes a 0600 hash + returns plaintext;
  validate true/false; loopback guard rejects a non-loopback RemoteAddr;
  logout removes the file) and `internal/api` (healthz 200; whoami 401 without
  token, 200 with a minted token — driven through `httptest` + a temp config
  dir via `XDG_CONFIG_HOME`).
- **init.sh smoke (key gate):** copy the repo to a temp dir, run
  `init.sh demoapp`, then `go build ./...` + `go vet ./...`; repeat with
  `init.sh demoapp --no-web`. Both must pass. Implemented as a shell test
  invoked by `make test` (guarded so it's skippable without bd).
- **web:** a single vitest smoke that the status component renders (optional,
  only when web present).
- **CI** runs `make check`, exercising all of the above on every push/PR.

## Out of scope

- Promoting auth-mint into perch (kept as app-customizable starter for now;
  revisit if a second app wants it verbatim → perch v0.2.0).
- Browser auto-open, TLS, multi-user auth, DB/storage layers — app-specific.
