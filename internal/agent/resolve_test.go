package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/guygrigsby/autophage/internal/routing"
)

type fakeChooser struct {
	choice routing.Choice
	err    error
}

func (f fakeChooser) Choose(context.Context, routing.Tier) (routing.Choice, error) {
	return f.choice, f.err
}

func TestResolveRoutedPick(t *testing.T) {
	var logs []string
	m := Models{Chooser: fakeChooser{choice: routing.Choice{ModelID: "qwen-27b", Warnings: []string{"unscored"}}},
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	if got := m.Resolve(t.Context(), routing.Triage, "zai/glm-5.3"); got != "qwen-27b" {
		t.Errorf("got %q, want the routed pick", got)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "qwen-27b") || !strings.Contains(logs[0], "unscored") {
		t.Errorf("logs = %q, want the pick and its warnings", logs)
	}
}

func TestResolveFallsBackToStaticWithWarning(t *testing.T) {
	var logs []string
	m := Models{Chooser: fakeChooser{err: errors.New("tailnet down")},
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	if got := m.Resolve(t.Context(), routing.Repair, "qwen-27b"); got != "qwen-27b" {
		t.Errorf("got %q, want the static id", got)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "tailnet down") {
		t.Errorf("logs = %q, want one fallback warning", logs)
	}
}

func TestResolveUnroutedTierIsSilent(t *testing.T) {
	var logs []string
	m := Models{Chooser: fakeChooser{err: routing.ErrNotRouted}, Logf: func(f string, a ...any) { logs = append(logs, f) }}
	if got := m.Resolve(t.Context(), routing.Attempt, "anthropic/claude-sonnet-5"); got != "anthropic/claude-sonnet-5" {
		t.Errorf("got %q", got)
	}
	if len(logs) != 0 {
		t.Errorf("an unrouted tier logged %q", logs)
	}
}

func TestResolveWithoutChooserIsStatic(t *testing.T) {
	if got := (Models{}).Resolve(t.Context(), routing.Triage, "x"); got != "x" {
		t.Errorf("got %q", got)
	}
}

func TestTriagerResolvesPerCall(t *testing.T) {
	var picked []string
	tr := &Triager{Resolve: func(context.Context) (ModelPick, error) {
		picked = append(picked, "a")
		return ModelPick{Model: canned(`{"size":"small","rationale":"typo"}`), ID: "qwen-27b"}, nil
	}}
	got, err := tr.Classify(t.Context(), "t", "b")
	if err != nil || got.Model != "qwen-27b" || len(picked) != 1 {
		t.Fatalf("got %+v %v picked=%v", got, err, picked)
	}
}
