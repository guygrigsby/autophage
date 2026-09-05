# Project Instructions for AI Agents

This app was generated from the `rookery` template. It is a two-binary
daemon/CLI built on `github.com/guygrigsby/perch`.

## Build & Test

```bash
make build    # daemon + CLI (+ SPA when web/ is present)
make test     # go test (+ vitest when web/ is present)
make check    # one-shot gate: gofmt, vet, golangci-lint, test (+ web build if web/)
make dev      # autophaged watcher (+ Vite when web/ is present), hot-reload
```

The web steps drop out automatically for a headless app (one scaffolded with
`--no-web`, or with no `web/`). **Run `make check` before claiming any task is
done.** It is the same gate CI runs.

## Architecture

- `cmd/autophaged` — daemon. Wires `perch/config` + `internal/api` + `perch/daemon.Serve`.
- `cmd/autophage` — CLI client over `perch/client` (`auth login`, `auth logout`, `whoami`).
- `internal/api` — HTTP routes: `/healthz`, loopback `/api/auth/mint`, auth-gated `/api/whoami`, static SPA.
- `internal/auth` — loopback token mint + SHA-256 hash validate. Customize per app.
- `embed.go` — embeds `web/dist` (the optional Svelte SPA); a headless build ships a no-embed stub.

## Conventions

- Config: `~/.config/autophage/config.toml`. Token: `~/.config/autophage/cli.token` (0600).
- Logs: `~/.logs/autophage/`. Default listen `:8080`.

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


<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:ca08a54f -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   bd dolt push
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->
