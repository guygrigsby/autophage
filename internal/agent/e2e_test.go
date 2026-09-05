//go:build e2e

package agent

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestE2E_TriageOnOpenRouter sizes two issues on the real triage tier. Costs
// a fraction of a cent. OPENROUTER_API_KEY=... go test -tags e2e ./internal/agent/ -run TestE2E_Triage -v
func TestE2E_TriageOnOpenRouter(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}
	m, err := Models{Key: key, Title: "autophage e2e"}.Tier("moonshotai/kimi-k2.7-code")
	if err != nil {
		t.Fatal(err)
	}
	tr := &Triager{Model: m, ModelID: "moonshotai/kimi-k2.7-code"}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	small, err := tr.Classify(ctx, "Typo in README", "The word 'teh' appears in README.md line 3; it should be 'the'.")
	if err != nil || small.Size != "small" {
		t.Errorf("typo sized %+v %v", small, err)
	}
	large, err := tr.Classify(ctx, "Replace the ORM", "Migrate every model and query from GORM to sqlc and change the repository interfaces accordingly.")
	if err != nil || large.Size != "large" {
		t.Errorf("rewrite sized %+v %v", large, err)
	}
}
