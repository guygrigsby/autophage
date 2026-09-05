# Project Instructions for AI Agents

This app was generated from the `rookery` template. It is a two-binary
daemon/CLI built on `github.com/guygrigsby/perch`.

## Build & Test

```bash
make build    # daemon + CLI (+ SPA when web/ is present)
make test     # go test (+ vitest when web/ is present)
make check    # one-shot gate: gofmt, vet, golangci-lint, test (+ web build if web/)
make dev      # appd watcher (+ Vite when web/ is present), hot-reload
```

The web steps drop out automatically for a headless app (one scaffolded with
`--no-web`, or with no `web/`). **Run `make check` before claiming any task is
done.** It is the same gate CI runs.

## Architecture

- `cmd/appd` — daemon. Wires `perch/config` + `internal/api` + `perch/daemon.Serve`.
- `cmd/app` — CLI client over `perch/client` (`auth login`, `auth logout`, `whoami`).
- `internal/api` — HTTP routes: `/healthz`, loopback `/api/auth/mint`, auth-gated `/api/whoami`, static SPA.
- `internal/auth` — loopback token mint + SHA-256 hash validate. Customize per app.
- `embed.go` — embeds `web/dist` (the optional Svelte SPA); a headless build ships a no-embed stub.

## Conventions

- Config: `~/.config/app/config.toml`. Token: `~/.config/app/cli.token` (0600).
- Logs: `~/.logs/app/`. Default listen `:8080`.

## Issue Tracking (bd / beads)

Use `bd` for ALL task tracking — do NOT use TodoWrite or markdown TODO lists.

```bash
bd ready             # available work
bd create --title="..." --type=task --priority=2
bd update <id> --claim
bd close <id>
```

## Session Completion

Work is NOT complete until pushed:
1. `make check` passes
2. commit
3. `git push` (and `bd dolt push` if using beads remote)
4. `git status` shows up to date
