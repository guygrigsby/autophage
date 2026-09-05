package agent

import (
	"context"
	"encoding/json"
	"testing"

	ac "github.com/voocel/agentcore"
)

// readOnlyTool is an ac.Tool that also declares itself read-only via
// ac.ReadOnlyer, to prove budgetTool forwards that declaration instead of
// losing it behind the embedded ac.Tool.
type readOnlyTool struct{}

func (readOnlyTool) Name() string                  { return "read" }
func (readOnlyTool) Description() string           { return "reads a file" }
func (readOnlyTool) Schema() map[string]any        { return map[string]any{"type": "object"} }
func (readOnlyTool) ReadOnly(json.RawMessage) bool { return true }
func (readOnlyTool) Execute(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

var _ ac.ReadOnlyer = readOnlyTool{}

func TestBudgetToolForwardsReadOnly(t *testing.T) {
	wrapped := &budgetTool{Tool: readOnlyTool{}, stop: &stopFlag{}, abort: func() {}, logf: func(string, ...any) {}}
	ro, ok := ac.Tool(wrapped).(ac.ReadOnlyer)
	if !ok || !ro.ReadOnly(nil) {
		t.Errorf("budgetTool did not forward ReadOnly: ok=%v", ok)
	}
	// A tool with no such declaration gets the conservative default.
	plain := &budgetTool{Tool: &echoTool{}, stop: &stopFlag{}, abort: func() {}, logf: func(string, ...any) {}}
	if plain.ReadOnly(nil) || plain.ConcurrencySafe(nil) || plain.Safe() {
		t.Errorf("budgetTool defaulted to unsafe/non-read-only/non-concurrency-safe wrongly")
	}
}
