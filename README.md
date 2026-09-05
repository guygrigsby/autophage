# autophage

A daemon on trig that watches issues on enrolled GitHub repositories through a GitHub App. For each new issue it decides whether it may act (trust gate), how big the work is (triage) and runs a jess coding agent in a podman sandbox under a budget. The output is a branch and a pull request, or a comment with findings and a request for approval. Approval is the `approved` label on the issue.

`issues.opened` arrives and is stored byte-exact. A trusted requester (Owner, Member or Collaborator association) goes to Received and is triaged: a model call sizes the issue Small or Large. Small is queued under the `auto` budget; Large waits for approval with its rationale posted as a comment. An untrusted requester goes to Gated, silently, until the `approved` label is added. Approval, from a label or from the operator, queues the case under the `approved` budget. The dispatcher starts attempts for queued cases within a concurrency limit; each attempt runs to a pull request, a budget exhaustion or a failure, and the outcome drives the case's next state.

This build wires the trust gate, triage state machine, scheduler, comment outbox, operator API, CLI and metrics. The sandboxed agent runner that actually executes attempts is a placeholder here: every started attempt ends immediately with `Failed{Infra}` until the agent plan (Plan E) lands.

Two binaries built on [perch](https://github.com/guygrigsby/perch):

- `autophaged`: the daemon. Serves the operator API under `/api`, the GitHub webhook at `/webhook/github` and Prometheus metrics at `/metrics`.
- `autophage`: the CLI client.

## Quick start

```bash
make build
./autophaged &                 # starts on 127.0.0.1:8080
./autophage auth login         # mint + store a token
./autophage status             # authenticated call
```

## Building

`go.mod` carries a `replace github.com/guygrigsby/jess => ../jess` directive: the sandbox plan needs `jess/mcp`, which exists only at the sibling checkout until jess is tagged. A later change adds the same for `github.com/guygrigsby/llm` once the OpenRouter adapter lands there. Building this repo means the `jess` checkout (and later `llm`) must exist as a sibling directory of `autophage/` on disk. The trig runbook drops both replace directives once real tags exist upstream.

## CLI

- `autophage status`: cases by state, running attempts, queue depth.
- `autophage cases [--state] [--repository] [--limit]`: list cases, newest first.
- `autophage case owner/repo#N`: one case with its triage, approvals, attempts and transitions.
- `autophage run owner/repo#N`: manually start an existing issue.
- `autophage stop attempt-id`: stop a running attempt.
- `autophage why attempt-id`: the jess ledger chain for an attempt's run.
- `autophage auth login` / `auth logout`, `autophage whoami`: token management.

## Config

Copy `config.example.toml` to `~/.config/autophage/config.toml`. Secrets are never read from the file: the webhook secret and the OpenRouter key come from `AUTOPHAGE_GITHUB_WEBHOOK_SECRET` and `OPENROUTER_API_KEY`.

## Deploy on trig (Linux, systemd)

Postgres runs natively (one database holds autophage's tables and jess's ledger tables). Write `~/.config/autophage/env` (mode 0600, values from the op cache) with:

```
AUTOPHAGE_GITHUB_WEBHOOK_SECRET=...
OPENROUTER_API_KEY=...
```

Then:

```bash
make install-systemd    # writes the user unit, enables and starts it
make redeploy-systemd   # rebuild, reinstall, restart in place
```

Expose the webhook publicly with Tailscale Funnel and nothing else:

```bash
tailscale funnel --bg --set-path /webhook/github http://127.0.0.1:8080/webhook/github
```

`/metrics` is reachable over the tailnet only, never through Funnel.

## macOS dev (launchd)

`make install-launchd`, `make redeploy` (or `make redeploy-launchd`), `make service-restart`, `make uninstall-launchd`. `make dev` runs the daemon with hot reload.

## Make targets

`make help` lists everything. The important ones: `build`, `test`, `check` (the quality gate), `dev` and the deploy targets above.

## Docs

- [Design](docs/specs/2026-09-04-autophage-design.md), [domain model](docs/specs/2026-09-04-autophage-domain-model.md), [contracts](docs/specs/2026-09-04-autophage-contracts.md), [context map](docs/specs/2026-09-04-autophage-context-map.md).
- ADRs in `docs/adr/`: [0001 jess as agent harness](docs/adr/0001-jess-as-agent-harness.md), [0002 sandbox posture](docs/adr/0002-sandbox-posture.md), [0003 trust gate and approval label](docs/adr/0003-trust-gate-and-approval-label.md), [0004 GitHub App webhooks and deployment](docs/adr/0004-github-app-webhooks-and-deployment.md), [0005 budgeted attempt with triage](docs/adr/0005-budgeted-attempt-with-triage.md), [0006 OpenRouter as model provider](docs/adr/0006-openrouter-as-model-provider.md).
