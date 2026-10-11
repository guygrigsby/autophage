# 11. Lean agent-select

Status: Accepted
Date: 2026-10-10

## Context

ADR 0010 integrated the first agent-select (`routing` package v0.1.x), whose snapshot, evidence and contract machinery never produced a quality ranking: the benchmark evidence it required was not admitted, so every candidate was unscored and only cost ordering worked. It also pulled in heavy contracts and dependencies for what autophage uses.

## Decision

Adopt agent-select v0.2.0, the lean `route` package (candidates and a task in, ordering with reasons out; quality from a `Scorer`). `internal/routing` maps the registry to candidates and calls `route.Rank`. Quality comes from `[routing.scores]`, an operator-declared table, so a tier set to `route = "quality"` ranks by it. Mode `performance` is renamed `quality`; the snapshot config and the runtime generation hash are gone. Defaults are unchanged: triage and repair route on cost, attempts stay on their configured ids until the operator declares scores.

## Consequences

- Attempts can route on quality today by declaring scores, with no benchmark pipeline.
- Declared scores are opinion. An outcome-driven `route.History` scorer is available in the library but not wired: recording attempt outcomes is follow-up work.
- Results carry no snapshot hash; the log line names the pick, its score basis and warnings.
- agent-select's imports leave `go.mod`: no canonicalization library, no SQLite or Postgres drivers from it.
