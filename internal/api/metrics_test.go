package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/guygrigsby/llm"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/storetest"
)

// catalogue is every metric autophaged publishes. It is the contract the
// Grafana dashboard queries by name, so a name that stops rendering is a
// panel that stops drawing: the test below exercises each hook once and
// proves the whole list appears on /metrics.
var catalogue = []string{
	"autophage_cases",
	"autophage_queue_depth",
	"autophage_attempts_total",
	"autophage_attempt_tokens_total",
	"autophage_attempt_wall_clock_seconds",
	"autophage_deliveries_total",
	"autophage_daemon_info",
	"autophage_daemon_start_time_seconds",
	"autophage_last_delivery_timestamp_seconds",
	"autophage_sweeps_total",
	"autophage_sweep_step_seconds",
	"autophage_sweep_step_errors_total",
	"autophage_github_requests_total",
	"autophage_github_request_seconds",
	"autophage_github_retries_total",
	"autophage_github_rate_limit_remaining",
	"autophage_triages_total",
	"autophage_triage_seconds",
	"autophage_model_requests_total",
	"autophage_model_latency_seconds",
	"autophage_model_tokens_total",
	"autophage_model_cost_usd_total",
	"autophage_attempts_running",
	"autophage_attempt_step_seconds",
	"autophage_attempt_step_errors_total",
	"autophage_attempt_tool_calls_total",
	"autophage_attempt_tool_seconds",
	"autophage_attempt_turns",
	"autophage_attempt_diff_lines",
	"autophage_attempt_stops_total",
	"autophage_comment_posts_total",
}

// exerciseHooks calls every hook once, which is what makes a labelled
// counter or histogram exist at all: an untouched vector renders nothing.
func exerciseHooks() {
	boom := errors.New("refused")

	RunnerMetrics{}.Running(1)
	RunnerMetrics{}.Running(-1)
	RunnerMetrics{}.Step("mint", 250*time.Millisecond, nil)
	RunnerMetrics{}.Step("push", time.Second, boom)
	RunnerMetrics{}.ToolCall("touch", 40*time.Millisecond, nil)
	RunnerMetrics{}.Ended("auto", "pull_request_opened", "none", resolution.Usage{Turns: 7, InputTokens: 100, OutputTokens: 20, WallClock: time.Minute, DiffLines: 42})

	SweepMetrics{}.Sweep()
	SweepMetrics{}.Step("translate", 30*time.Millisecond, nil)
	SweepMetrics{}.Step("triage", time.Second, boom)

	TriageMetrics{}.Triage("small", "ok", 2*time.Second)
	CommentMetrics{}.Post("ok")

	RequestMetrics{}.Request("GET", "200", 120*time.Millisecond)
	RequestMetrics{}.Retry("backoff")
	RequestMetrics{}.RateLimitRemaining(4998)

	TranslateMetrics{}.Delivery("issues", "translated")

	ModelMetrics{}.Observe("moonshotai/kimi-k2", llm.Usage{PromptTokens: 900, CompletionTokens: 120, ReasoningTokens: 40, CacheReadTokens: 300, CacheWriteTokens: 10, Cost: 0.02, Latency: 3 * time.Second}, nil)
	ModelMetrics{}.Observe("moonshotai/kimi-k2", llm.Usage{}, boom)
}

func TestMetricsRendersEveryCatalogueName(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	// autophage_cases is a per-state gauge read from the store, so it
	// renders nothing at all until a case exists.
	r, _ := resolution.NewRepository("guy/repo", 42, "main", t0)
	if err := st.EnrollRepository(ctx, r); err != nil {
		t.Fatal(err)
	}
	req, _ := resolution.NewRequester("guy", resolution.AssociationOwner)
	c, _ := resolution.NewCase("guy/repo", 11, req, t0)
	if err := st.CreateCase(ctx, c); err != nil {
		t.Fatal(err)
	}
	exerciseHooks()

	h := New(t.TempDir(), nil, Deps{Store: st, Clock: fixedClock{t0}, Version: "1.2.3", StartedAt: t0})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	rendered := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if name, _, ok := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " "); ok && strings.HasPrefix(line, "# TYPE ") {
			rendered[name] = true
		}
	}
	var missing []string
	for _, name := range catalogue {
		if !rendered[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("missing from /metrics: %s", strings.Join(missing, " "))
	}
	// The version rides on a label, so a dashboard can show what is
	// running without a second scrape target.
	if !strings.Contains(body, `autophage_daemon_info{version="1.2.3"} 1`) {
		t.Error("autophage_daemon_info does not carry the version")
	}
}
