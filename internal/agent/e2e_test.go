//go:build e2e

package agent

import (
	"context"
	"testing"
	"time"
)

// TestE2E_TriageOnAperture sizes two issues on the real triage tier through
// the gateway on the tailnet, which authenticates the host: no key to set.
// go test -tags e2e ./internal/agent/ -run TestE2E_Triage -v
func TestE2E_TriageOnAperture(t *testing.T) {
	m, err := (Models{}).Tier("zai/glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	tr := &Triager{Model: m, ModelID: "zai/glm-5.3"}
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
