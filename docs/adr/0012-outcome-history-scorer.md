# 12. Outcome history scorer

Status: Accepted
Date: 2026-10-13

## Context

ADR 0011 left agent-select's `route.History` unwired. Its contract is that only mechanical verdicts are recorded (tests, CI), never a model's own claim. Autophage has one such verdict: CI's conclusive rollup on the head a repair round pushed, already stored as `bump_check_verdicts` and joinable to the round's `bump_repair_outcome_pushes.head_sha` and `bump_repair_runs.model`.

The other tiers have none. Triage is a classification nobody checks. An issue attempt's outcome is the agent opening a pull request; check events are only translated for bumps, and a merge is a human's judgment, not a mechanical one.

## Decision

- Only repair rounds record. A round counts when CI concludes on the exact head it pushed: success is a pass, failure a fail. The verdict that opened the round judged dependabot's head and does not count. No change, budget exhaustion, failures, aborts and a pushed head that something else replaced before CI concluded all record nothing.
- Outcomes are derived, not stored. `store.RepairVerdictsByModel` joins the existing facts and groups by model; the unique `(bump_id, head_sha)` on `bump_check_verdicts` serves the join. A separate outcomes table would hold a second copy of facts these tables already own, and the copy could drift from them. No migration.
- Read per use, like the registry: each `Choose` builds a fresh `route.History` from the query. No boot load, no in-memory state to keep in step with verdicts, no event hook. Pick volume is a handful per hour.
- Scorer order is `route.Chain{history, route.Table(scores)}`. A measured pass rate beats an operator's opinion once a model has `routing.history_min_runs` verdicts on the task (default 5); the table covers every model without that many. A history read failure falls back to the table and adds the warning `history_unavailable`; it never fails the pick.
- History is recorded under the router's task name for the repair tier, `code`, which attempts and approved attempts share. Repair evidence therefore informs every `code` tier routed on quality.

## Consequences

- No tier routes on quality by default (ADR 0011 amendments pin the models), so history changes nothing until an operator sets `route = "quality"` on a tier.
- Chain mixes scales by first hit: a model with history competes on its smoothed pass rate against another model's declared table value. Declared scores should sit on the same 0..1 "how often does CI go green" scale to compare fairly.
- Repair work (fixing a red dependency bump) is narrower than an issue attempt. Its pass rate is the only mechanical evidence there is, and it is applied to attempts on that basis.
- Issue attempts gain history only once their pull requests' CI conclusions are observed; that needs check events translated for cases, which is follow-up work.
