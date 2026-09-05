# 1. jess as the agent harness

Status: Accepted
Date: 2026-09-04

## Context

autophage needs a coding agent it can run unattended against an issue, under a budget, with a record of what it did, on models beyond one vendor. Candidates that run headless and take a non-Anthropic model: jess (ours), opencode, OpenHands, pi, Codex CLI, goose, Kimi Code CLI, crush and aider.

Two shapes fall out. An in-process harness (jess) where the daemon owns the loop: the model key never leaves the daemon, the ledger is enforced rather than observed, and the gate is a function call. An external agent (everything else) that executes its own tools, so it runs inside the sandbox holding the key unless a proxy fronts the model, and provenance is whatever the event stream exposes.

agentcore, which jess wraps, already ships `read`, `write`, `edit`, `grep`, `glob`, `ls` and `bash`, all rooted to a work dir. jess adds the fail-closed gate with an `Approver` hook, a durable ledger with a Postgres backend, `WithMaxTurns` and `WithMaxToolErrors`, skills from a `SKILL.md` layout and subagents. gyr already built the async approver bridge, the MCP adapter, model tiering and the rookery daemon shape on top of it. The `llm` module has native Anthropic, Kimi and DeepSeek adapters behind one port.

## Decision

jess is the harness. The daemon builds one jess agent per attempt, drives it with `jess.Stream`, and translates the run summary and events into an Outcome. Only `internal/agent` imports jess, agentcore and llm.

## Consequences

- Full provenance per attempt for free: `autophage why <attempt>` reads the ledger chain by run id.
- The model key and GitHub credentials stay in the daemon by construction.
- The loop quality of agentcore on long coding tasks is unproven. Nothing in the design changes if it is weak; the fix rate would. Address by iterating on the brief, the steers and the tools rather than swapping the harness.
- opencode remains the fallback: it has the seam a daemon needs (HTTP, SSE, a permission endpoint, abort). Swapping means replacing `internal/agent` and accepting the key-in-container problem.

## Alternatives

- opencode: mature loop, per-call permission endpoint, Moonshot first-class. Loses enforced provenance and puts the key in the sandbox. The only serious alternative.
- OpenHands: the closest product (its resolver is this feature) but heavy: Python, docker runtime images, config split across four hierarchies, open bugs on custom provider URLs in headless mode.
- pi, Codex CLI, goose, Kimi Code CLI, crush, aider: no gate hook a daemon can answer, text or event-only seams, no budget controls.
- Claude Code headless: ruled out by the requirement to run on non-Anthropic models.
