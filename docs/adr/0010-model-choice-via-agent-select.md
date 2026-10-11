# 10. Model choice via agent-select

Status: Accepted
Date: 2026-10-10

## Context

Each tier named one Aperture id in config. The ids drifted from the gateway (it lists both `glm-5.3` and `zai/glm-5.3`, and only some serve the chat endpoint autophage speaks), a model that went away failed every call of its tier, and the machine on the tailnet with free capacity (trig, which serves Qwen through Aperture as `qwen-27b`) was used by one tier only because someone typed its id. `guygrigsby/agent-select` is an offline router that ranks a host's live registry by capability, limits and exact cost, and says plainly when it has no quality evidence.

## Decision

A tier may set `route = "cost"` or `"performance"`. A routed tier asks `internal/routing` before each use, which reads Aperture's `/v1/models` (`aperture.ListModelInfo` in `llm`), maps every entry to a runtime offer and calls `Engine.Route`. The configured model id stays as the fallback: a registry that is unreachable, slow (5s) or yields no admissible offer logs a warning and runs the configured id.

Host facts the adapter asserts, and nothing else:

- Eligible means the gateway serves the entry on `/v1/chat/completions` and does not require client auth. A listed model is not an executable one.
- Capabilities `text_input`, `text_output`, `tool_use` for eligible entries. That is what the aperture adapter wires, not a claim about any model's quality.
- Limits and prices from the registry, USD per token converted exactly to per million. Tiered and fast-mode rates are not read.
- Providers in `routing.local_providers` (trig) are free: price `0` asserted, because an unpriced offer ranks behind every priced one. Their context window comes from `routing.local_context_tokens`, a per-model map mirroring llama-swap's `context_size` on trig, since the Aperture registry reports none; unknown means never chosen. A 32k model (`qwen`) therefore serves triage but not repair.

Defaults: triage and repair route on cost, so the local Qwens lead and the cheapest remote model follows. Auto and approved attempts do not route. With no quality signals in the snapshot every candidate is unscored and performance mode degrades to cost order, which would hand the highest-stakes tier to the cheapest model. Flip them to `"performance"` once a snapshot carries signals (`routing.snapshot`, adopted at boot). Until then routing is a live-registry filter plus cost order, and every pick logs `estimate_unscored`.

The runtime registry is read per use, with no cache: pick volume is a handful per hour and a stale registry is the failure routing exists to avoid.

## Consequences

- A second local model needs no code: once Aperture lists it under a `local_providers` provider it enters the same free tier.
- A local model that is listed but down still wins. The router has no health signal and the adapter adds no retry (agent-select host contract), so a call failure is a failed triage or repair round, not a failover. Tracked as follow-up work: retry the next `Alternatives` entry on a transport error.
- Triage moves from `zai/glm-5.3` to whatever is cheapest, normally Qwen. Triage quality on a 27B local model is unmeasured.
- Outcome receipts (agent-select `receipt record`) are not wired. They are what would produce `host_observed` signals and let attempts route on performance.
- `go.mod` gains `agent-select` and needs `llm` with `ListModelInfo`.
