package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guygrigsby/jess"
	"github.com/guygrigsby/jess/ledger"
	ac "github.com/voocel/agentcore"

	"github.com/guygrigsby/autophage/internal/resolution"
)

// echoTool records calls and returns ok.
type echoTool struct{ calls atomic.Int32 }

func (e *echoTool) Name() string        { return "touch" }
func (e *echoTool) Description() string { return "touch a file" }
func (e *echoTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}
}
func (e *echoTool) Execute(context.Context, json.RawMessage) (json.RawMessage, error) {
	e.calls.Add(1)
	return json.RawMessage(`{"ok":true}`), nil
}

// scripted plays a sequence: n tool calls, then a final text. A real model
// cannot emit a tool call for a tool it was never offered, so once the
// agent's tool list is empty (the forced-summary turn calls SetTools() with
// none) the script answers with final text right away regardless of n.
func scripted(toolCalls int, final string, sawSteer *atomic.Bool) ac.ChatModel {
	var n atomic.Int32
	return jess.Once(true, func(_ context.Context, msgs []ac.Message, tools []ac.ToolSpec) (*ac.LLMResponse, error) {
		for _, m := range msgs {
			if m.Role == ac.RoleUser && strings.Contains(m.TextContent(), "Budget is nearly used up") && sawSteer != nil {
				sawSteer.Store(true)
			}
		}
		if len(tools) > 0 && int(n.Add(1)) <= toolCalls {
			return &ac.LLMResponse{Message: ac.Message{Role: ac.RoleAssistant,
				Content:    []ac.ContentBlock{ac.ToolCallBlock(ac.ToolCall{ID: "c", Name: "touch", Args: json.RawMessage(`{"path":"x"}`)})},
				StopReason: ac.StopReasonToolUse}}, nil
		}
		return &ac.LLMResponse{Message: ac.Message{Role: ac.RoleAssistant, Content: []ac.ContentBlock{ac.TextBlock(final)}, StopReason: ac.StopReasonStop}}, nil
	})
}

const goodSummary = "What I found: a typo.\nWhat I did: fixed it.\nWhat is left: nothing.\nWhat I would do with more budget: nothing."

func input(t *testing.T, model ac.ChatModel, tool ac.Tool, turns int, wall time.Duration, diff int, diffFn func(context.Context) (int, error)) (RunInput, *string) {
	t.Helper()
	b, err := resolution.NewBudget(turns, wall, diff)
	if err != nil {
		t.Fatal(err)
	}
	var runID string
	if diffFn == nil {
		diffFn = func(context.Context) (int, error) { return 0, nil }
	}
	// DiscardSink is not a DurableSink: jess's audit middleware denies every
	// non-safe tool call outright (it never reaches budgetTool.Execute), which
	// would also stop the diff-line tracker from ever seeing a call. Every
	// test here needs the tool to actually run, so the ledger is a real
	// (temp-file) SQLite sink throughout.
	sqlite, err := ledger.OpenSQLite(t.TempDir() + "/l.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })
	return RunInput{Model: model, Tools: []ac.Tool{tool}, Ledger: sqlite, Budget: b, Brief: "fix it", AgentID: "t", DiffLines: diffFn,
		Clock: resolution.SystemClock{}, Logf: t.Logf, OnRunBegan: func(id string) { runID = id }}, &runID
}

func TestRunCompletesWithSummary(t *testing.T) {
	tool := &echoTool{}
	in, runID := input(t, scripted(2, goodSummary, nil), tool, 10, time.Minute, 100, nil)
	rep := RunAttempt(t.Context(), in)
	if rep.Stop != StopNone || rep.Err != nil {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Summary != goodSummary || tool.calls.Load() != 2 || rep.Usage.Turns < 3 {
		t.Errorf("report = %+v calls = %d", rep, tool.calls.Load())
	}
	if *runID == "" || rep.RunID != *runID {
		t.Errorf("run id not captured: %q %q", *runID, rep.RunID)
	}
}

func TestRunStopsOnTurnsThenForcesSummary(t *testing.T) {
	tool := &echoTool{}
	var steered atomic.Bool
	in, _ := input(t, scripted(100, goodSummary, &steered), tool, 5, time.Minute, 100, nil)
	rep := RunAttempt(t.Context(), in)
	if rep.Stop != StopTurns {
		t.Fatalf("stop = %q, want turns", rep.Stop)
	}
	if !strings.Contains(rep.Summary, "What I found") {
		t.Errorf("no forced summary: %q", rep.Summary)
	}
	if !steered.Load() {
		t.Error("80% turn steer never reached the model")
	}
}

func TestRunStopsOnDiffLines(t *testing.T) {
	tool := &echoTool{}
	lines := 0
	diff := func(context.Context) (int, error) { lines += 60; return lines, nil }
	in, _ := input(t, scripted(100, goodSummary, nil), tool, 50, time.Minute, 100, diff)
	rep := RunAttempt(t.Context(), in)
	if rep.Stop != StopDiffLines || tool.calls.Load() > 3 {
		t.Fatalf("stop = %q calls = %d", rep.Stop, tool.calls.Load())
	}
	if rep.Usage.DiffLines < 100 || !strings.Contains(rep.Summary, "What I found") {
		t.Errorf("report = %+v", rep)
	}
}

func TestRunStopsOnWallClock(t *testing.T) {
	slow := jess.Once(true, func(ctx context.Context, _ []ac.Message, _ []ac.ToolSpec) (*ac.LLMResponse, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
		return &ac.LLMResponse{Message: ac.Message{Role: ac.RoleAssistant, Content: []ac.ContentBlock{ac.TextBlock(goodSummary)}, StopReason: ac.StopReasonStop}}, nil
	})
	in, _ := input(t, slow, &echoTool{}, 10, 300*time.Millisecond, 100, nil)
	start := time.Now()
	rep := RunAttempt(t.Context(), in)
	if rep.Stop != StopWallClock {
		t.Fatalf("stop = %q, want wall_clock (%+v)", rep.Stop, rep)
	}
	if time.Since(start) > 4*time.Second {
		t.Error("wall clock stop did not cut the model call short")
	}
}

func TestRunCancelledByCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	slow := jess.Once(true, func(ctx context.Context, _ []ac.Message, _ []ac.ToolSpec) (*ac.LLMResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	in, _ := input(t, slow, &echoTool{}, 10, time.Minute, 100, nil)
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	rep := RunAttempt(ctx, in)
	if rep.Stop != StopCancelled {
		t.Errorf("stop = %q", rep.Stop)
	}
}

func TestRunModelError(t *testing.T) {
	broken := jess.Once(true, func(context.Context, []ac.Message, []ac.ToolSpec) (*ac.LLMResponse, error) {
		return nil, context.DeadlineExceeded
	})
	in, _ := input(t, broken, &echoTool{}, 10, time.Minute, 100, nil)
	rep := RunAttempt(t.Context(), in)
	if rep.Stop != StopModelErr || rep.Err == nil {
		t.Errorf("report = %+v", rep)
	}
}
