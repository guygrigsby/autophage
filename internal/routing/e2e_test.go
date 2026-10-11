//go:build e2e

package routing

import (
	"context"
	"testing"

	"github.com/guygrigsby/llm/aperture"
)

// TestE2E_ChooseFromLiveRegistry routes every tier against the real gateway
// registry on the tailnet.
// go test -tags e2e ./internal/routing/ -run TestE2E -v
func TestE2E_ChooseFromLiveRegistry(t *testing.T) {
	r, err := New(Config{
		Registry: func(ctx context.Context) ([]aperture.ModelInfo, error) { return aperture.ListModelInfo(ctx, "", "") },
		Modes:    map[Tier]Mode{Triage: Cost, Repair: Cost},
		Local:    []string{"trig"}, LocalContext: map[string]int64{"qwen-27b": 131072, "qwen": 32768},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tier := range []Tier{Triage, Repair} {
		c, err := r.Choose(t.Context(), tier)
		if err != nil {
			t.Fatalf("%s: %v", tier, err)
		}
		t.Logf("%s -> %s %v", tier, c.ModelID, c.Warnings)
	}
}
