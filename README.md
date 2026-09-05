# rookery

A GitHub template for small two-binary daemon/CLI apps in Go, built on
[perch](https://github.com/guygrigsby/perch). Scaffold a new local service in
one command: a daemon (`appd`), a CLI client (`app`), an optional embedded
Svelte web UI, loopback token auth, launchd integration, and an agent-ready
toolchain.

## What you get

- **`cmd/appd`** — the daemon. Loads `~/.config/<app>/config.toml`, serves an
  HTTP API and (optionally) an embedded Svelte SPA, and shuts down gracefully
  via perch's signal/serve helpers.
- **`cmd/app`** — the CLI client over perch: `auth login`, `auth logout`,
  `whoami`, talking to the daemon with a loopback-minted bearer token.
- **`internal/auth`** — loopback-only token mint + SHA-256 hash validate.
  Starter code; customize per app.
- **`internal/api`** — routes: `/healthz`, `POST /api/auth/mint` (loopback
  only), bearer-gated `GET /api/whoami`, and the embedded SPA at `/`.
- **`web/`** — a minimal Vite + Svelte 5 SPA. Drop it entirely with `--no-web`.
- **launchd** — `make install-launchd` / `redeploy` / `service-restart` to run
  the daemon as a background service on macOS.
- **agent-ready** — `CLAUDE.md` (symlinked to `AGENTS.md`), a `make check`
  gate, bd issue tracking, and CI that mirrors `make check` + `make test`.

## Create a new app

1. Click **Use this template** on GitHub (or clone this repo).
2. Rename the scaffold to your app:
   ```bash
   scripts/init.sh <name> [--no-web]
   ```
   `<name>` is a lowercase identifier (`^[a-z][a-z0-9]*$`). `init.sh`:
   - rewrites the module path to `github.com/guygrigsby/<name>`,
   - renames the binaries to `<name>d` / `<name>`,
   - retargets the launchd label (`dev.grigsby.<name>d`) and the
     `~/.config/<name>` + `~/.logs/<name>` paths,
   - re-initializes a fresh bd tracker,
   - replaces this README with the app's own (`README.app.md`),
   - and, with `--no-web`, deletes `web/` and swaps in a headless embed stub.
3. Build and verify:
   ```bash
   go mod tidy
   make build
   make test
   ```

`make test` runs the Go tests, the web vitest, and an init smoke test that
renames a throwaway copy and confirms it builds — both with and without web.

## Make targets

`make help` lists everything. The ones you'll reach for:

| target | what it does |
|---|---|
| `build` | SPA + `appd` + `app` |
| `dev` | daemon watcher + Vite, both hot-reload |
| `test` | go test + vitest + init smoke test |
| `check` | one-shot gate: gofmt, `go vet`, golangci-lint, tests, web build |
| `install-launchd` / `redeploy` / `service-restart` | run as a launchd service |

## Conventions

- Config: `~/.config/<app>/config.toml`; token: `~/.config/<app>/cli.token` (0600).
- Logs: `~/.logs/<app>/`. Default listen `:8080` (override via config, env
  `<APP>_LISTEN`, or the `-addr` flag).
- Depends on the public module `github.com/guygrigsby/perch`.

## Layout

```
cmd/appd/            # daemon entrypoint
cmd/app/             # CLI client entrypoint
internal/api/        # HTTP routes
internal/auth/       # loopback token mint + validate
web/                 # Vite + Svelte SPA (optional)
embed.go             # //go:embed web/dist
deploy/              # launchd plist template
scripts/init.sh      # the renamer
scripts/dev-watch.sh # rebuild + bounce the daemon on change
README.app.md        # becomes the app's README after init.sh
```
