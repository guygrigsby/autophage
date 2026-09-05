# autophage design

Date: 2026-09-04. Companions: [context map](2026-09-04-autophage-context-map.md), [domain model](2026-09-04-autophage-domain-model.md), ADRs 0001 to 0006 in `docs/adr/`.

## What it is

A daemon on trig that watches issues on enrolled GitHub repositories through a GitHub App. For each new issue it decides whether it may act (trust gate), how big the work is (triage), and runs a jess coding agent in a podman sandbox under a budget. The output is a branch and a pull request, or a comment with findings and a request for approval. Approval is the `approved` label on the issue.

## Flow

1. `issues.opened` arrives. The delivery is stored byte-exact and 200 is returned.
2. Translation: `NewCase` with the requester's `author_association`. Trusted goes to Received, Untrusted to Gated. Gated is silent: no comment, no label.
3. Received is triaged: a mid-tier model reads title and body and returns Small or Large with a rationale. Small goes to Queued under the `auto` budget. Large goes to AwaitingApproval and the rationale is posted as a comment.
4. `issues.labeled` with `approved` records an Approval. Gated, AwaitingApproval and Failed go to Queued under the `approved` budget.
5. The dispatcher starts attempts for Queued cases within the concurrency limit.
6. An attempt: mint a token scoped to the repo, clone or fetch and rebase the case branch onto the default branch, warm dependencies in a networked prep container, start the agent container with no network, dial the toolbox, build the brief, run jess under the budget with steers, force a summary turn, commit and push, open the PR or post the summary, record the outcome, tear down.
7. Outcome drives the state: PullRequestOpened to Done; BudgetExhausted to AwaitingApproval with the branch pushed and findings posted; Failed to Failed with the error posted.
8. `issues.closed` closes the case and aborts a running attempt.

## Trust gate

`author_association` of Owner, Member or Collaborator is Trusted. Contributor and below is Untrusted. Trust is a verdict made at receipt and stored with the evidence. The label is the trust anchor: GitHub only lets triage-or-better add labels, so an Approval is trusted by construction. Approving an untrusted issue means accepting its body as agent input.

Removing and re-adding the label is the retry. There is no other retry mechanism.

## Triage

Only trusted cases. Structured output: a `size` enum plus `rationale` text. The rationale is posted verbatim and never executed. A trusted author's issue body is the whole injection surface here, and triage output cannot do anything except pick a budget.

## Attempt and budget

A Budget is turns, wall clock and diff lines. Two named budgets in config, `auto` and `approved`. The Agent adapter enforces them: turns via jess `WithMaxTurns`, wall clock via context deadline, diff lines by running `git diff --shortstat <baseSha>` in the container after each tool call. At 80% of turns or wall clock it injects a steer: wrap up, commit what you have, then write the summary. Every attempt ends with one forced summary turn in the fixed shape (what I found, what I did, what is left, what I would do with more budget). That summary is the issue comment and the PR body.

On exhaustion the branch is pushed anyway. The approved attempt resumes on the same branch, rebased onto the default branch first, with the prior summary in its brief. Rebase conflicts are the agent's first job and the brief says so.

The brief states the situation plainly: who autophage is, that the run is unattended, the requester's trust, the triage size, the branch, the exact budget numbers, what happens on exhaustion, and the summary shape. The issue text is fenced as untrusted input. Full content rules in the domain model under Brief.

## Sandbox

The three legs of the lethal trifecta and what is done to each:

- Private data: the container holds only this repository's checkout. No secrets, no home directory, no other cases, no database, no ledger. The model key and the App private key live in the daemon process only. No tool runs on the host; every tool executes inside the container through the toolbox, so no agent-chosen path is ever resolved on the host.
- Untrusted content: the issue and the repository are the job. No web fetch tool, no MCP servers beyond the toolbox, triage output is an enum, and the trust gate decides who can put content in front of the agent.
- Exfiltration: `--network=none` for the whole agent phase. Dependencies are warmed beforehand from lockfiles into per-repository cache volumes by a prep container that has network but no agent. Push is done by the daemon to one branch on one repository with a token the container never held. What leaves is the diff and the summary, both landing on the repository the content came from.

Toolbox: a Go binary in the image that serves agentcore's `read`, `write`, `edit`, `grep`, `glob`, `ls` and `bash` tools over MCP stdio, rooted at `/work`. The daemon dials it with `podman exec -i <container> autophage-toolbox` and adapts the tools with the MCP adapter extracted from gyr's `internal/mcp` into `jess/mcp` (see ADR 0002). Inside the sandbox the jess gate is `AllowAll()`; the container is the boundary and the ledger records every call durably.

Container: rootless podman, non-root user, `--cap-drop=all`, `--security-opt=no-new-privileges`, read-only root with writable `/work`, `/tmp` and the cache volumes, memory, cpu and pids limits, a hard wall-clock kill. Fresh container per attempt. The workspace on host disk persists across attempts of one case.

Prep container: same image, network allowed, runs the lockfile-driven fetch for each toolchain detected (`go.mod`: `go mod download`; `package-lock.json` or `pnpm-lock.yaml`: `npm ci` or `pnpm install --frozen-lockfile`; `uv.lock`: `uv sync --frozen`). Cache volumes are named per repository.

Image: one `autophage-sandbox` image with Go, Node, Python with uv, git, make and the toolbox binary. Built on trig by `make image`. Toolchain versions are open.

Residual: the model provider sees the repository (any provider for any repository, decided). The agent can write anything it read into the PR, bounded to repository content. Posted comments have `@mentions` neutralised.

## GitHub boundary

A GitHub App named autophage installed on the account; selected repositories are the enrollment. Permissions: contents write, issues write, pull requests write, metadata read. Events: `issues` (opened, labeled, closed), `installation`, `installation_repositories`. Every delivery is HMAC-verified before anything else.

Inbound: store the delivery (delivery id unique, raw payload as BYTEA, event and action) and return 200. A worker translates stored deliveries into commands: `IssueOpened`, `LabelAdded`, `IssueClosed`, `RepoEnrolled`, `RepoRemoved`. Everything else is recorded and ignored. Processing is its own fact row per delivery so a crash mid-translation replays. Events whose sender is the bot itself are dropped.

Outbound port in Resolution's types: `MintToken(repository)` (App JWT to an installation token scoped to that single repository, one hour), `PostComment`, `OpenPullRequest`, `EnsureLabel(repository, "approved")` on enrollment. Push is `git push` on the host with the token as a per-command header; nothing lands in `.git/config`. Every call honors `Retry-After` and secondary rate limits with backoff. A failed post is retried, never dropped, and never aborts the attempt that produced it.

Ingress: Tailscale Funnel on trig exposes `POST /webhook/github`. The tailnet ACL must permit Funnel for trig; verifying that is the first deployment task.

## Models

OpenRouter for every tier. The `llm` module gains an `openrouter` adapter (chat completions wire format, OpenRouter headers, reasoning passthrough), contributed upstream. Config names one OpenRouter model id per tier: `[model.triage]`, `[model.auto]`, `[model.approved]`. The key is `OPENROUTER_API_KEY` from the op cache. Any provider for any repository, public or private.

## Process

`autophaged` on trig under systemd `--user` with linger, scaffolded from rookery `--no-web` plus a systemd unit template. Inside: the webhook handler, a dispatcher woken by Postgres `LISTEN/NOTIFY` on new deliveries and case transitions, and a bounded worker pool. No polling loop.

Daemon restart: on boot every attempt without an outcome is recorded `Aborted{DaemonRestart}`; the case re-queues once, then fails. Postgres runs natively on trig; one database holds autophage's tables and jess's ledger tables so `autophage why` joins on run id. Prometheus metrics on the daemon's listen address for bee's Prometheus to scrape over the tailnet: cases by state, attempts by outcome, tokens, wall clock, queue depth. Logs to the journal.

Errors are handled where recovery exists: GitHub API errors back off and retry in place; podman and git failures end the attempt `Failed{Infra}` with the branch pushed if anything was committed; model errors go through jess's retries then `Failed{Model}`; the agent declaring it cannot is `Failed{Agent}`. Nothing propagates to main.

## CLI

`autophage` over perch loopback auth: `status`, `cases [--state]`, `run owner/repo#N` (manual start for an existing issue), `stop <attempt>`, `why <attempt>` (the jess ledger chain for that run).

## Config

```toml
[github]
app_id          = 0
private_key     = "~/.config/autophage/app.pem"
# webhook secret from AUTOPHAGE_GITHUB_WEBHOOK_SECRET

[listen]
addr = "127.0.0.1:8080"

[db]
url = "postgres:///autophage"

[model.triage]
model = "…"          # OpenRouter id
[model.auto]
model = "…"
[model.approved]
model = "…"
# key from OPENROUTER_API_KEY

[budget.auto]
turns      = 0
wall_clock = "0m"
diff_lines = 0
[budget.approved]
turns      = 0
wall_clock = "0m"
diff_lines = 0

[sandbox]
image          = "localhost/autophage-sandbox:latest"
workspaces_dir = "~/.local/share/autophage/workspaces"
concurrency    = 2

[label]
approved = "approved"
```

## Testing

Table-driven over the Case transition table: every listed transition passes and every unlisted one is refused. Budget, Requester and Brief constructors. Brief builder golden files. The GitHub adapter against recorded webhook payloads including a bad HMAC and a redelivery. The repository layer against real Postgres via testcontainers. The sandbox adapter against real podman, skipped where absent. One end-to-end test with `jess.Once` as the model driving a real container over a fixture repository, asserting a branch, a PR call and a ledger chain. All under `make check`.

## Deferred

- Adding a dependency during an attempt (would need a registry allowlist instead of no network).
- Iterating on CI failures on the opened PR.
- Comment-driven follow-ups on an issue.
- Swift and iOS repositories.
- Reopened issues.
- Notifications beyond GitHub (gyr over Telegram).
- Per-repository configuration files.

## Open

See the context map's list.
