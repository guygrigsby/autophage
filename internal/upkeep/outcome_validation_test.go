package upkeep

import (
	"errors"
	"testing"

	"github.com/guygrigsby/autophage/internal/resolution"
)

// Each variant's own fields are required, or the store writes a variant row
// that says nothing and the operator reads an outcome with no evidence.
func TestOutcomeVariantFieldsAreRequired(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  RepairOutcome
	}{
		{"pushed without a sha", RepairOutcome{Kind: Pushed, Summary: "pushed"}},
		{"pushed with a short sha", RepairOutcome{Kind: Pushed, HeadSha: "abc", Summary: "pushed"}},
		{"exhausted without a limit", RepairOutcome{Kind: BudgetExhausted, Summary: "out"}},
		{"exhausted with an unknown limit", RepairOutcome{Kind: BudgetExhausted, Limit: "vibes", Summary: "out"}},
		{"failed without a class", RepairOutcome{Kind: RepairFailed, Message: "boom", Summary: "boom"}},
		{"failed without a message", RepairOutcome{Kind: RepairFailed, Class: resolution.FailureInfra, Summary: "boom"}},
		{"aborted without a reason", RepairOutcome{Kind: RepairAborted, Summary: "stopped"}},
		{"aborted with Resolution's issue_closed", RepairOutcome{Kind: RepairAborted, Reason: "issue_closed", Summary: "stopped"}},
		{"unknown kind", RepairOutcome{Kind: "vibes", Summary: "x"}},
		{"no summary", RepairOutcome{Kind: NoChange}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := repairing(t)
			tc.out.EndedAt = at(3)
			if err := b.RecordOutcome("", tc.out, maxRounds); !errors.Is(err, resolution.ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
			if b.OpenRepair() == nil {
				t.Error("a refused outcome closed the round anyway")
			}
		})
	}
}

func TestCurrentVerdictFollowsTheHead(t *testing.T) {
	b := newTestBump(t)
	if b.CurrentVerdict() != nil {
		t.Error("a bump with no verdict has a current one")
	}
	if err := b.RecordVerdict(failure(sha1, 1), maxRounds); err != nil {
		t.Fatalf("RecordVerdict: %v", err)
	}
	if v := b.CurrentVerdict(); v == nil || v.Conclusion != CheckFailure {
		t.Fatalf("current verdict = %+v, want the failure", v)
	}
	if _, err := b.StartRepair(testBudget(t), "fix it", at(2), maxRounds); err != nil {
		t.Fatalf("StartRepair: %v", err)
	}
	if err := b.RecordOutcome("", RepairOutcome{Kind: Pushed, HeadSha: sha2, Summary: "pushed", EndedAt: at(3)}, maxRounds); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	if b.CurrentVerdict() != nil {
		t.Error("the old head's verdict is still current after a push")
	}
}

func TestAssignIDOnlyOnce(t *testing.T) {
	b := newTestBump(t)
	if err := b.AssignID(""); !errors.Is(err, resolution.ErrInvalid) {
		t.Errorf("empty: err = %v, want ErrInvalid", err)
	}
	if err := b.AssignID("b1"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := b.AssignID("b2"); !errors.Is(err, resolution.ErrRefused) {
		t.Errorf("second: err = %v, want ErrRefused", err)
	}
	if got := b.ID(); got != "b1" {
		t.Errorf("id = %s, want b1", got)
	}
}

func TestNewWatchNeedsARepository(t *testing.T) {
	if _, err := NewWatch("", t0); !errors.Is(err, resolution.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
	w, err := NewWatch("o/r", t0)
	if err != nil {
		t.Fatalf("NewWatch: %v", err)
	}
	if w.Repository != "o/r" || !w.WatchedAt.Equal(t0) {
		t.Errorf("watch = %+v", w)
	}
}
