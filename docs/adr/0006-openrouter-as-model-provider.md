# 6. OpenRouter as the model provider

Status: Accepted
Date: 2026-09-04

## Context

The operator wants Kimi K3 and other non-Anthropic models per tier without a provider account and adapter per vendor. The `llm` module's rule is native per-provider adapters behind one port, no OpenAI-compatibility shim over a different provider.

## Decision

OpenRouter is the provider for every tier. `llm` gains `llm/openrouter`, a native adapter for OpenRouter's own API (chat completions wire format, OpenRouter headers, reasoning passthrough, usage accounting), contributed upstream rather than kept in autophage. Config names one OpenRouter model id per tier: triage, auto attempt, approved attempt. The key is `OPENROUTER_API_KEY` from the op cache.

## Consequences

- One account, one key, any model per tier by config.
- OpenRouter is the adapter's native API, so this does not violate `llm`'s no-shim rule; it is a provider whose API happens to be OpenAI-shaped, like Kimi's.
- Provider routing, fallbacks and per-model quirks are OpenRouter's problem, not the daemon's.
- Every repository's content goes through OpenRouter to whichever upstream serves the model. Accepted for any repository.
