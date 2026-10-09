# 8. Aperture as the model gateway

Status: Accepted
Date: 2026-10-07

## Context

ADR 0006 chose OpenRouter as the provider for every tier: one account and one upstream adapter for any vendor's model, keyed by `OPENROUTER_API_KEY`. Since then the operator stood up Aperture, a model gateway on the tailnet that fronts the vendors the household actually uses, meters spend in one place and authenticates the host rather than a bearer key. Routing autophage's model calls to the vendors directly (via OpenRouter or otherwise) now means two billing paths, two metering stories and a second secret to rotate, for no capability OpenRouter still uniquely provides. Upkeep (ADR 0007) also added a fourth tier, repair, that wants a cheap high-volume route.

## Decision

Aperture is the model provider for every tier. Calls go to the gateway's OpenAI-compatible endpoint on the tailnet; the `llm` module's `aperture` adapter (contributed upstream) names it. The gateway authenticates the host, so `OPENROUTER_API_KEY` is deleted from config, secrets and the runbook: the webhook secret is the only environment key left. Config names one Aperture model id per tier, now four: triage, auto attempt, approved attempt, and repair for upkeep rounds. Defaults: `zai/glm-5.3` triage, `anthropic/claude-sonnet-5` auto and approved, `qwen-27b` repair (the free route on the local Qwen; a repair reads a failing check and an existing diff rather than writing a fix from a description, so it tolerates a smaller model).

## Consequences

- One billing path and one metering story for the household; autophage's per-model Prometheus metrics keep the same labels, now keyed by Aperture id.
- The deploy host must be on the tailnet and resolve Aperture's name; the gateway being down is a new failure mode for every tier at once.
- Repair rounds stop consuming paid tokens and stop counting against any vendor quota.
- Model changes are config edits per tier, as before, against Aperture's `/v1/models` rather than OpenRouter's catalog.
