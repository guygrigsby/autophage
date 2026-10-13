package routing

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/guygrigsby/llm/aperture"

	"github.com/guygrigsby/agent-select/route"
)

func registry(models ...aperture.ModelInfo) Registry {
	return func(context.Context) ([]aperture.ModelInfo, error) { return models, nil }
}

var chat = []string{"/v1/chat/completions"}

var (
	qwen    = aperture.ModelInfo{ID: "qwen-27b", Provider: "trig", Endpoints: chat}
	qwenB   = aperture.ModelInfo{ID: "qwen-coder", Provider: "trig", Endpoints: chat}
	flash   = aperture.ModelInfo{ID: "zai/glm-5.3-flash", Provider: "aperture", Endpoints: chat, ContextTokens: 1000000, MaxOutputTokens: 131000, InputPrice: "0.00000015", OutputPrice: "0.00000050"}
	sonnet  = aperture.ModelInfo{ID: "anthropic/claude-sonnet-5", Provider: "aperture", Endpoints: chat, ContextTokens: 1000000, MaxOutputTokens: 128000, InputPrice: "0.00000200", OutputPrice: "0.00001000"}
	byoAuth = aperture.ModelInfo{ID: "claude-sonnet-5", Provider: "anthropic-sub", Endpoints: []string{"/v1/messages"}, RequiresClientAuth: true, ContextTokens: 1000000, MaxOutputTokens: 128000, InputPrice: "0.0000001", OutputPrice: "0.0000001"}
)

func newRouter(t *testing.T, reg Registry) *Router {
	t.Helper()
	r, err := New(Config{
		Registry:     reg,
		Modes:        map[Tier]Mode{Triage: Cost, Repair: Cost, Attempt: Quality},
		Local:        []string{"trig"},
		LocalContext: map[string]int64{"qwen-27b": 131072, "qwen-coder": 131072},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCostTierPrefersFreeLocalModel(t *testing.T) {
	r := newRouter(t, registry(sonnet, flash, qwen, byoAuth))
	c, err := r.Choose(t.Context(), Triage)
	if err != nil {
		t.Fatal(err)
	}
	if c.ModelID != "qwen-27b" {
		t.Errorf("chose %q, want the free local qwen-27b", c.ModelID)
	}
}

func TestBothLocalModelsAreCandidates(t *testing.T) {
	// qwen-27b is down for this registry read: the other local model wins
	// the free slot.
	r := newRouter(t, registry(sonnet, flash, qwenB))
	c, err := r.Choose(t.Context(), Repair)
	if err != nil {
		t.Fatal(err)
	}
	if c.ModelID != "qwen-coder" {
		t.Errorf("chose %q, want qwen-coder", c.ModelID)
	}
}

func TestIneligibleModelsNeverChosen(t *testing.T) {
	// Only a bring-your-own-auth model that serves /v1/messages: nothing
	// this host can call.
	r := newRouter(t, registry(byoAuth))
	if _, err := r.Choose(t.Context(), Triage); err == nil {
		t.Fatal("chose a model the host cannot call")
	}
}

func TestLocalModelWithoutKnownContextIsRejected(t *testing.T) {
	r, err := New(Config{Registry: registry(qwen, flash), Modes: map[Tier]Mode{Triage: Cost}, Local: []string{"trig"}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.Choose(t.Context(), Triage)
	if err != nil {
		t.Fatal(err)
	}
	if c.ModelID != "zai/glm-5.3-flash" {
		t.Errorf("chose %q: an unbounded local model must not take a request it may not fit", c.ModelID)
	}
}

func TestRegistryFailureIsAnError(t *testing.T) {
	boom := errors.New("tailnet down")
	r := newRouter(t, func(context.Context) ([]aperture.ModelInfo, error) { return nil, boom })
	if _, err := r.Choose(t.Context(), Triage); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the registry error", err)
	}
}

func TestUnroutedTierIsAnError(t *testing.T) {
	r := newRouter(t, registry(sonnet))
	_, err := r.Choose(t.Context(), Approved)
	if !errors.Is(err, ErrNotRouted) {
		t.Fatalf("err = %v, want ErrNotRouted", err)
	}
}

func TestPerMillion(t *testing.T) {
	for in, want := range map[string]float64{"0.00000200": 2, "0.00000015": 0.15, "0": 0} {
		got := perMillion(in)
		if got == nil || math.Abs(*got-want) > 1e-9 {
			t.Errorf("perMillion(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"", "x", "-1", "1e400"} {
		if perMillion(bad) != nil {
			t.Errorf("perMillion(%q) accepted", bad)
		}
	}
}

func TestQualityTierFollowsScores(t *testing.T) {
	r, err := New(Config{Registry: registry(flash, sonnet, qwen), Modes: map[Tier]Mode{Attempt: Quality}, Local: []string{"trig"},
		LocalContext: map[string]int64{"qwen-27b": 131072},
		Scorer:       route.Table{"anthropic/claude-sonnet-5": {"code": 0.9}, "zai/glm-5.3-flash": {"code": 0.6}, "qwen-27b": {"code": 0.5}}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.Choose(t.Context(), Attempt)
	if err != nil || c.ModelID != "anthropic/claude-sonnet-5" || c.Score != "table" {
		t.Errorf("choice = %+v, %v; want the best-scored model", c, err)
	}
}

func TestWarningsSurface(t *testing.T) {
	r := newRouter(t, registry(sonnet, flash))
	c, err := r.Choose(t.Context(), Triage)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.Warnings, "unscored") {
		t.Errorf("an unscored choice must say so: %+v", c)
	}
}

func TestSmallLocalWindowExcludedByRequestSize(t *testing.T) {
	small := aperture.ModelInfo{ID: "qwen", Provider: "trig", Endpoints: chat}
	r, err := New(Config{Registry: registry(small, qwen, flash), Modes: map[Tier]Mode{Triage: Cost, Repair: Cost}, Local: []string{"trig"},
		LocalContext: map[string]int64{"qwen": 32768, "qwen-27b": 131072}})
	if err != nil {
		t.Fatal(err)
	}
	// Triage (8.4k tokens) fits both free models; repair (68k) fits only the 27b.
	if c, err := r.Choose(t.Context(), Repair); err != nil || c.ModelID != "qwen-27b" {
		t.Errorf("repair = %+v, %v; want qwen-27b", c, err)
	}
	if c, err := r.Choose(t.Context(), Triage); err != nil || (c.ModelID != "qwen" && c.ModelID != "qwen-27b") {
		t.Errorf("triage = %+v, %v; want a free local model", c, err)
	}
}

func TestLocalOnlyNeverPicksRemote(t *testing.T) {
	r, err := New(Config{Registry: registry(flash, sonnet, qwen), Modes: map[Tier]Mode{Repair: Quality}, LocalOnly: map[Tier]bool{Repair: true},
		Local: []string{"trig"}, LocalContext: map[string]int64{"qwen-27b": 131072},
		Scorer: route.Table{"anthropic/claude-sonnet-5": {"code": 0.9}, "qwen-27b": {"code": 0.1}}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.Choose(t.Context(), Repair)
	if err != nil || c.ModelID != "qwen-27b" {
		t.Fatalf("choice = %+v, %v; want the local model despite a better remote score", c, err)
	}
}

func TestLocalOnlyWithNoLocalModelIsAnError(t *testing.T) {
	r, err := New(Config{Registry: registry(flash, sonnet), Modes: map[Tier]Mode{Triage: Cost}, LocalOnly: map[Tier]bool{Triage: true}, Local: []string{"trig"}})
	if err != nil {
		t.Fatal(err)
	}
	if c, err := r.Choose(t.Context(), Triage); err == nil {
		t.Fatalf("chose %+v with no local model", c)
	}
}

func outcomes(ts ...Tally) Outcomes {
	return func(context.Context) ([]Tally, error) { return ts, nil }
}

// Recorded CI verdicts outrank the operator's table once a model has
// MinRuns of them for the task; below that the table still decides.
func TestHistoryOutranksTable(t *testing.T) {
	table := route.Table{"anthropic/claude-sonnet-5": {"code": 0.9}, "qwen-27b": {"code": 0.5}}
	for _, tc := range []struct {
		name      string
		runs      int
		wantModel string
		wantScore string
	}{
		{"enough runs", 5, "qwen-27b", "history n=5"},
		{"too few runs", 4, "anthropic/claude-sonnet-5", "table"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := New(Config{Registry: registry(sonnet, qwen), Modes: map[Tier]Mode{Repair: Quality}, Local: []string{"trig"},
				LocalContext: map[string]int64{"qwen-27b": 131072}, Scorer: table, MinRuns: 5,
				Outcomes: outcomes(Tally{Tier: Repair, Model: "qwen-27b", Passes: tc.runs}, Tally{Tier: Repair, Model: "anthropic/claude-sonnet-5", Fails: tc.runs})})
			if err != nil {
				t.Fatal(err)
			}
			c, err := r.Choose(t.Context(), Repair)
			if err != nil || c.ModelID != tc.wantModel || c.Score != tc.wantScore {
				t.Errorf("choice = %+v, %v; want %s on %q", c, err, tc.wantModel, tc.wantScore)
			}
		})
	}
}

// A failing history read degrades to the table and says so; it never stops
// routing.
func TestHistoryReadFailureFallsBackToTable(t *testing.T) {
	r, err := New(Config{Registry: registry(sonnet, qwen), Modes: map[Tier]Mode{Repair: Quality}, Local: []string{"trig"},
		LocalContext: map[string]int64{"qwen-27b": 131072}, Scorer: route.Table{"anthropic/claude-sonnet-5": {"code": 0.9}},
		Outcomes: func(context.Context) ([]Tally, error) { return nil, errors.New("db down") }, MinRuns: 1})
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.Choose(t.Context(), Repair)
	if err != nil || c.ModelID != "anthropic/claude-sonnet-5" || !slices.Contains(c.Warnings, "history_unavailable") {
		t.Errorf("choice = %+v, %v; want the table pick with a history_unavailable warning", c, err)
	}
}
