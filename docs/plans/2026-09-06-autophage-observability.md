# autophage observability plan

**Goal:** Every stage of the daemon is measurable from Prometheus and visible on one Grafana dashboard: deliveries, sweeps, triage, GitHub calls, model calls, attempt steps, tool calls and outcomes.

**Architecture:** Each package that emits keeps a small metrics interface with a no-op default (`app.SweepMetrics`, `app.TriageMetrics`, `app.CommentMetrics`, `github.RequestMetrics`, `agent.Metrics` extended, `agent.ModelMetrics`); `internal/api` implements all of them over Prometheus vectors and registers them in `metricsHandler`; `cmd/autophaged/main.go` wires the implementations. No package outside `internal/api` imports Prometheus. Model calls are metered through `llm.Meter` set on every adapter `agent.Models` builds.

**Tech Stack:** Go 1.26, `github.com/prometheus/client_golang`, `llm.Meter`, Grafana file provisioning and Prometheus static configs in the `training` repo (`deploy/monitoring`).

**Spec:** the metric catalogue below is the contract for both tasks. Names and labels are final; the dashboard queries them verbatim.

## Global Constraints

- Repo `/Users/guygrigsby/projects/autophage` on `main` for Task 1; repo `/Users/guygrigsby/projects/training` (`deploy/monitoring/`) on `main` for Task 2. Both commit straight to `main`.
- Only `internal/api` imports Prometheus. Metrics interfaces have a no-op default so tests and the e2e need no wiring.
- Label cardinality is bounded: `model` is the configured OpenRouter id (three values), `tool` is the toolbox's tool name, `step`, `state`, `outcome`, `result`, `status`, `method`, `size`, `kind`, `direction` are closed vocabularies. Never a repository, issue number, attempt id or error text as a label.
- No em or en dashes and no Oxford commas anywhere. Commit messages terse, verb-first, prefixed `metrics:` (autophage) or `monitoring:` (training). No Claude or Anthropic attribution, no `Co-Authored-By` or `Claude-Session` trailers. `make check` green (autophage) before every commit. `for i := range n` for counts. Do not push.

## Metric catalogue

Existing, kept as is: `autophage_cases{state}` (gauge), `autophage_queue_depth` (gauge), `autophage_attempts_total{kind,outcome}` (counter), `autophage_attempt_tokens_total{direction}` (counter), `autophage_attempt_wall_clock_seconds` (histogram, 30s to 4h).

Existing but never incremented, wire it: `autophage_deliveries_total{event,result}` (counter), incremented by the translator once per processing row, `result` in `translated|ignored|rejected`.

New:

| Metric | Type | Labels | Emitted by |
|---|---|---|---|
| `autophage_daemon_info` | gauge, always 1 | version | api, at registration |
| `autophage_daemon_start_time_seconds` | gauge | | api, at registration |
| `autophage_last_delivery_timestamp_seconds` | gauge, 0 when none | | api, from the store on scrape |
| `autophage_sweeps_total` | counter | | dispatcher, per sweep |
| `autophage_sweep_step_seconds` | histogram, 0.05s to 300s | step | dispatcher, per step (`translate`, `cancel`, `enroll`, `triage`, `comment`, `schedule`) |
| `autophage_sweep_step_errors_total` | counter | step | dispatcher, when a step returns an error |
| `autophage_github_requests_total` | counter | method, status | GitHub retry transport, per response (`status` is the numeric code as a string; a transport error is `error`) |
| `autophage_github_request_seconds` | histogram, 0.05s to 60s | method | GitHub retry transport, per attempt |
| `autophage_github_retries_total` | counter | reason | GitHub retry transport, `reason` in `retry_after|rate_limit|backoff` |
| `autophage_github_rate_limit_remaining` | gauge | | GitHub retry transport, last seen `X-RateLimit-Remaining` |
| `autophage_triages_total` | counter | size, result | app.Triage, `size` in `small|large|none`, `result` in `ok|error|parked` (`parked` is the give-up after repeated failures) |
| `autophage_triage_seconds` | histogram, 0.5s to 120s | | app.Triage, per model call |
| `autophage_model_requests_total` | counter | model, result | llm.Meter per adapter, `result` in `ok|error` |
| `autophage_model_latency_seconds` | histogram, 0.5s to 600s | model | llm.Meter |
| `autophage_model_tokens_total` | counter | model, direction | llm.Meter, `direction` in `prompt|completion|reasoning|cache_read|cache_write` |
| `autophage_model_cost_usd_total` | counter | model | llm.Meter, from `Usage.Cost` |
| `autophage_attempts_running` | gauge | | runner, register and unregister |
| `autophage_attempt_step_seconds` | histogram, 0.1s to 1h | step | runner, `step` in `mint|prepare|start|tools|run|remint|push|conflicts|pull_request|teardown` |
| `autophage_attempt_step_errors_total` | counter | step | runner, when a step fails |
| `autophage_attempt_tool_calls_total` | counter | tool, result | budgetTool, `result` in `ok|error` |
| `autophage_attempt_tool_seconds` | histogram, 0.05s to 600s | tool | budgetTool |
| `autophage_attempt_turns` | histogram, 1 to 256 | | runner, at outcome |
| `autophage_attempt_diff_lines` | histogram, 1 to 4096 | | runner, at outcome |
| `autophage_attempt_stops_total` | counter | stop | runner, `stop` in the `Stop` vocabulary plus `none` |
| `autophage_comment_posts_total` | counter | result | app.Commenter, `result` in `ok|error` |

## File structure

Task 1 (autophage):
- Modify `internal/api/metrics.go`: all vectors, registration, the info and start-time gauges, the last-delivery gauge func.
- Modify `internal/api/metrics_runner.go`: `RunnerMetrics` implements the extended `agent.Metrics`; add `SweepMetrics`, `TriageMetrics`, `CommentMetrics`, `RequestMetrics`, `ModelMetrics` implementations (one file `internal/api/metrics_hooks.go`).
- Modify `internal/app/dispatcher.go` (`SweepMetrics` field and calls), `internal/app/triage.go` (`TriageMetrics`), `internal/app/commenter.go` (`CommentMetrics`), `internal/github/client.go` (`RequestMetrics` on the retry transport, plumbed through `ClientConfig`), `internal/github/translate.go` (`DeliveriesTotal` through a `TranslateMetrics` interface, or reuse `SweepMetrics`), `internal/agent/runner.go` (extended `Metrics`: `Running(delta int)`, `Step(step string, d time.Duration, err error)`, `Ended(kind, outcome, stop string, usage)`), `internal/agent/budget_tool.go` (`ToolCall(tool string, d time.Duration, err error)` through the same `Metrics`), `internal/agent/models.go` (`ModelMetrics` interface `Observe(model string, u llm.Usage, err error)`; `Models.Tier` sets `Config.Meter`; note the adapter's Meter sees only successful calls unless llm reports errors through it, so count errors where `Generate` returns one if the Meter cannot).
- Modify `cmd/autophaged/main.go`: wire every implementation.
- Tests: each package proves its hooks fire with the right labels through a recording fake; `internal/api` proves every catalogue metric renders on `/metrics` after one call each (a golden of names, not values).

Task 2 (training, `deploy/monitoring`):
- Modify `prometheus/prometheus.yml`: job `autophage`, `scheme: https`, target `100.72.89.139:443` (the tailnet service VIP), `tls_config: { server_name: autophage.guy.ts.net }`, `labels: { host: trig }`, comment naming the service and why the IP (the container has no MagicDNS).
- Create `grafana/provisioning/dashboards/autophage.json`: uid `autophage`, title `autophage`, tags `autophage`, refresh 30s, time now-6h, rows: Overview (cases by state, queue depth, attempts running, deliveries per minute, last delivery age, daemon version), Attempts (outcomes over time stacked by outcome, wall clock p50 and p95, step p95 by step, stops, turns and diff line distributions), Tools (calls per tool, error rate, p95 seconds), Model (requests and errors by model, latency p95, tokens per direction, cost per hour and total), GitHub (requests by status, rate limit remaining, retries by reason, request p95), Sweeps and triage (step p95, step errors, triages by size and result, triage p95, comment posts). Every panel queries the catalogue names verbatim with the `prometheus` datasource uid; match the panel types and gridPos conventions of `adpip.json`.

---

### Task 1: Instrument the daemon

**Files:** as listed above.

**Interfaces:**
- Produces: the metrics interfaces named above, each with an exported no-op (`app.NopSweepMetrics{}` and so on) used as the default when the field is nil.
- Consumes: `llm.Meter` and `llm.Usage` (`Provider`, `Model`, `PromptTokens`, `CompletionTokens`, `ReasoningTokens`, `CacheReadTokens`, `CacheWriteTokens`, `Latency`, `Cost`).

- [ ] **Step 1:** For each emitting package, write the failing test with a recording fake, run it, implement the interface and the calls, run to green.
- [ ] **Step 2:** `internal/api`: vectors, hook implementations, registration, the render test.
- [ ] **Step 3:** wire `cmd/autophaged/main.go`; `go build ./...`; `make check`.
- [ ] **Step 4:** commit `metrics: instrument sweeps, GitHub, model, triage, tool calls and attempt steps`.

### Task 2: Scrape job and dashboard

**Files:** as listed above.

- [ ] **Step 1:** the Prometheus job; `promtool check config` if available.
- [ ] **Step 2:** the dashboard JSON, validated as JSON and against the catalogue (every metric name in the JSON exists in the catalogue).
- [ ] **Step 3:** commit `monitoring: autophage scrape job and dashboard`.
