package routing

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/guygrigsby/llm/aperture"
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
		Modes:        map[Tier]Mode{Triage: Cost, Repair: Cost, Attempt: Performance},
		Local:        []string{"trig"},
		LocalContext: 131072,
		Clock:        func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) },
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
	r, err := New(Config{Registry: registry(qwen, flash), Modes: map[Tier]Mode{Triage: Cost}, Local: []string{"trig"},
		Clock: func() time.Time { return time.Unix(0, 0).UTC() }})
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

func TestGenerationTracksRegistryMetadata(t *testing.T) {
	a := toRuntime(map[string]bool{"trig": true}, 0, []aperture.ModelInfo{sonnet})
	repriced := sonnet
	repriced.OutputPrice = "0.00002000"
	b := toRuntime(map[string]bool{"trig": true}, 0, []aperture.ModelInfo{repriced})
	if generation(a) == generation(b) {
		t.Error("a price change left the runtime generation unchanged")
	}
	if len(generation(a)) != 64 {
		t.Errorf("generation = %q, want 64 hex characters", generation(a))
	}
}

func TestPricePerMillion(t *testing.T) {
	for in, want := range map[string]string{
		"0.00000200": "2",
		"0.00000015": "0.15",
		"0.00001000": "10",
		"0":          "0",
	} {
		got, ok := perMillion(in)
		if !ok || got != want {
			t.Errorf("perMillion(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "x", "-1", "1e400"} {
		if _, ok := perMillion(bad); ok {
			t.Errorf("perMillion(%q) accepted", bad)
		}
	}
}

func TestWarningsSurface(t *testing.T) {
	r := newRouter(t, registry(sonnet, flash))
	c, err := r.Choose(t.Context(), Triage)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.Warnings, "estimate_unscored") {
		t.Errorf("an unscored choice must say so: %+v", c)
	}
}
