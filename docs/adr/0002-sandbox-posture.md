# 2. Sandbox posture: tools in the container, no network, no secrets

Status: Accepted
Date: 2026-09-04

## Context

The agent reads attacker-controlled text (issue bodies on public repositories, repository contents) and produces commits, comments and pull requests under a token with write access. That is the lethal trifecta: private data, untrusted content and an exfiltration channel. Prompt injection is the primary threat, not a footnote.

gyr's ADR 0004 removed the `bash` tool because a confirm-per-call gate cannot render or audit an opaque shell line. A coding run is hundreds of calls including `bash` for tests, and the unit of approval here is the issue, not the call. The gate model has to bend.

## Decision

The container is the security boundary, not the gate.

- Every tool executes inside a rootless podman container. A toolbox binary in the image serves agentcore's tools over MCP stdio, rooted at `/work`; the daemon dials it with `podman exec -i` and adapts the tools through the MCP adapter. No agent-chosen path is ever resolved on the host.
- The container holds no secrets: no model key, no GitHub token, no home directory, no other case's workspace, no database, no ledger.
- `--network=none` for the whole agent phase. Dependencies are warmed from lockfiles into per-repository cache volumes by a separate prep container with network and no agent.
- Push happens on the host by the daemon, to one branch on one repository, with an installation token scoped to that repository and passed as a per-command header.
- Hardening: non-root user, `--cap-drop=all`, `--security-opt=no-new-privileges`, read-only root with writable `/work`, `/tmp` and cache volumes, memory, cpu and pids limits, hard wall-clock kill, fresh container per attempt.
- Inside the sandbox the jess gate is `AllowAll()`. The ledger still records every call durably in Postgres; "no durable record, no action" holds unchanged.
- gyr's `internal/mcp` is extracted into `jess/mcp` so gyr and autophage share one adapter rather than a copy.

## Consequences

- The exfiltration leg is removed for the agent phase. What leaves is the diff and the summary, both landing on the repository the content came from.
- The agent cannot add a new dependency in v1. It sees the fetch fail and says so in the summary. Revisit with a registry allowlist if this bites often.
- The model provider sees the repository. Accepted for any provider on any repository.
- The agent can write anything it read into the PR. Bounded to repository content; the PR is reviewed before merge regardless.
- `bash` is allowed here where gyr forbids it. Different boundary, different rule: gyr's bash ran on the operator's machine behind a tap; this bash runs in a disposable container with nothing to reach.
- Every `podman exec` is a process spawn per tool call. Acceptable at the call rates involved; the toolbox is a long-lived process per attempt, so the exec cost is paid once and calls go over stdio.

## 2026-09-05: prep-phase egress, and what is still open

Added after the sandbox implementation review. The decision above stands; this
records what the networked prep container can still reach and why that is
accepted for now.

The prep container is the one phase with a network, and it runs on lockfiles a
previous attempt may have committed. That is a real, if narrow, exfiltration
and fetch channel: an agent that writes a lockfile in one attempt makes the
next attempt's prep container connect to whatever hosts that lockfile names,
before any human has reviewed the pull request.

Narrowed for Go: the prep container runs with `GOPROXY=https://proxy.golang.org`
with no `,direct` fallback, `GOFLAGS=-mod=readonly` and `GOSUMDB=sum.golang.org`,
so a `go.mod` cannot send the fetch to an arbitrary host and the phase cannot
rewrite `go.mod` to widen its own reach.

Accepted residual: npm and uv still follow the URLs in their own lockfiles
(`resolved` in `package-lock.json`, `url` in `uv.lock`), so a committed
lockfile can still name a host of the attacker's choosing for those two. The
fix is an egress allowlist for the prep container (a podman network with
nftables rules or a forward proxy), tracked as beads issue `autophage-e43`.
Until then the exposure is bounded by what the prep phase holds, which is the
repository's own source and no secrets: no model key, no installation token,
no database.

Also accepted: memory, cpu and pids are bounded per container, disk is not. An
attempt can fill the host's disk from `bash`, and the caches it fills are read
by the next attempt on the same repository. Quotas and a cache-discard policy
are tracked as beads issue `autophage-7d4`.
