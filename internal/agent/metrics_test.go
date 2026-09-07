package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guygrigsby/jess"
	"github.com/guygrigsby/llm"
	ac "github.com/voocel/agentcore"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/storetest"
)

// recordModels records the model hook: one row per call, "<model>/<result>".
type recordModels struct {
	mu    sync.Mutex
	calls []string
	usage []llm.Usage
}

func (r *recordModels) Observe(model string, u llm.Usage, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := "ok"
	if err != nil {
		result = "error"
	}
	r.calls = append(r.calls, model+"/"+result)
	r.usage = append(r.usage, u)
}

func (r *recordModels) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// failingTool is a tool whose Execute always fails, so the tool metric's
// error result has something to record.
type failingTool struct{}

func (failingTool) Name() string           { return "boom" }
func (failingTool) Description() string    { return "always fails" }
func (failingTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (failingTool) Execute(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("tool: no such file")
}

// The pipeline's step vocabulary, which the dashboard groups by. Every one
// of these has to fire on an attempt that runs end to end.
var attemptSteps = []string{"mint", "prepare", "start", "tools", "run", "remint", "push", "conflicts", "pull_request", "teardown"}

func TestRunnerRecordsEveryStepAndTheRunningGauge(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{commits: true}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}}
	r, m, _ := newRunner(t, st, sb, gh, scripted(1, goodSummary, nil))
	r.Run(t.Context(), id)

	steps := m.allSteps()
	for _, want := range attemptSteps {
		if !contains(steps, want) {
			t.Errorf("step %q never reported: %v", want, steps)
		}
		if contains(steps, want+"!") {
			t.Errorf("step %q reported a failure it did not have: %v", want, steps)
		}
	}
	if high, now := m.runningGauge(); high != 1 || now != 0 {
		t.Errorf("running gauge peaked at %d and ended at %d, want 1 and 0", high, now)
	}
	if got := m.allStops(); len(got) != 1 || got[0] != "none" {
		t.Errorf("stops = %v, want [none]", got)
	}
	// The tools the agent called are counted by name and result.
	if got := m.allTools(); len(got) != 1 || got[0] != "touch/ok" {
		t.Errorf("tool calls = %v, want [touch/ok]", got)
	}
}

// A model that refuses fails the run step and ends the attempt with the
// model_error stop. The Meter on the adapter never sees a call that made no
// usage, so this is also the one place the model error itself is counted.
func TestRunnerRecordsTheModelErrorStop(t *testing.T) {
	st := storetest.Open(t)
	id := startedAttempt(t, st, resolution.Auto)
	sb := &fakeSandbox{commits: true}
	gh := &fakeGitHub{issue: resolution.IssueDetail{Title: "T", Body: "B", Open: true}}
	broken := jess.Once(true, func(context.Context, []ac.Message, []ac.ToolSpec) (*ac.LLMResponse, error) {
		return nil, errors.New("openrouter: 402 insufficient credits")
	})
	models := &recordModels{}
	r, m, _ := newRunner(t, st, sb, gh, broken)
	r.Models.Metrics = models
	r.Run(t.Context(), id)

	if steps := m.allSteps(); !contains(steps, "run!") {
		t.Errorf("steps = %v, want the run step to have failed", steps)
	}
	if got := m.allStops(); len(got) != 1 || got[0] != "model_error" {
		t.Errorf("stops = %v, want [model_error]", got)
	}
	if got := models.all(); len(got) != 1 || got[0] != "test/auto/error" {
		t.Errorf("model calls = %v, want [test/auto/error]", got)
	}
}

func TestBudgetToolRecordsEveryToolCall(t *testing.T) {
	m := &countMetrics{}
	diff := func(context.Context) (int, error) { return 0, nil }
	ok := &budgetTool{Tool: &echoTool{}, limit: 100, diffLines: diff, stop: &stopFlag{}, abort: func() {}, logf: func(string, ...any) {}, metrics: m}
	if _, err := ok.Execute(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	bad := &budgetTool{Tool: failingTool{}, limit: 100, diffLines: diff, stop: &stopFlag{}, abort: func() {}, logf: func(string, ...any) {}, metrics: m}
	if _, err := bad.Execute(t.Context(), nil); err == nil {
		t.Fatal("want the tool error back")
	}
	if got := m.allTools(); strings.Join(got, ",") != "touch/ok,boom/error" {
		t.Errorf("tool calls = %v, want [touch/ok boom/error]", got)
	}
}

// Every adapter Models builds meters its calls under the model id it was
// built for, which is the label the dashboard groups by.
func TestModelsMeterReportsTheModelID(t *testing.T) {
	models := &recordModels{}
	m := Models{Key: "k", Title: "autophage", Metrics: models}
	m.meter("openai/gpt-5").Observe(llm.Usage{PromptTokens: 11, CompletionTokens: 3, Cost: 0.25, Latency: time.Second})

	if got := models.all(); len(got) != 1 || got[0] != "openai/gpt-5/ok" {
		t.Fatalf("model calls = %v", got)
	}
	if u := models.usage[0]; u.PromptTokens != 11 || u.Cost != 0.25 {
		t.Errorf("usage = %+v", u)
	}
	if _, err := m.Tier("openai/gpt-5"); err != nil {
		t.Errorf("Tier: %v", err)
	}
}

// The triage tier's failures never reach the Meter either: the adapter
// reports usage, and a call that failed before the provider answered has
// none. Classify is where they are counted instead.
func TestTriagerCountsAModelError(t *testing.T) {
	models := &recordModels{}
	broken := jess.Once(true, func(context.Context, []ac.Message, []ac.ToolSpec) (*ac.LLMResponse, error) {
		return nil, errors.New("openrouter: 429 rate limited")
	})
	tr := &Triager{Model: broken, ModelID: "test/triage", Metrics: models}
	if _, err := tr.Classify(t.Context(), "Typo", "teh"); err == nil {
		t.Fatal("want the model error back")
	}
	if got := models.all(); len(got) != 1 || got[0] != "test/triage/error" {
		t.Errorf("model calls = %v, want [test/triage/error]", got)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
