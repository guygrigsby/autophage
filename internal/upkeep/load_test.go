package upkeep

import (
	"errors"
	"testing"

	"github.com/guygrigsby/autophage/internal/resolution"
)

func validSnapshot() Snapshot {
	return Snapshot{
		ID: "b1", Repository: "o/r", Number: 7, Branch: "dependabot/x", BaseBranch: "main",
		HeadSha: sha1, State: AwaitingChecks, OpenedAt: t0,
	}
}

func TestLoadBumpRoundTrips(t *testing.T) {
	b, err := LoadBump(validSnapshot())
	if err != nil {
		t.Fatalf("LoadBump: %v", err)
	}
	if b.ID() != "b1" || b.Number() != 7 || b.HeadSha() != sha1 {
		t.Errorf("bump = %s#%d@%s, want b1 o/r#7@%s", b.Repository(), b.Number(), b.HeadSha(), sha1)
	}
	if len(b.Changes().Transitions) != 0 {
		t.Error("a freshly loaded bump has pending changes")
	}
}

// Invariants a row could have lost between writes are re-checked on load,
// because a bump that exists is valid is the whole contract.
func TestLoadBumpRefusesBrokenRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Snapshot)
	}{
		{"no repository", func(s *Snapshot) { s.Repository = "" }},
		{"no number", func(s *Snapshot) { s.Number = 0 }},
		{"no branch", func(s *Snapshot) { s.Branch = "" }},
		{"bad head sha", func(s *Snapshot) { s.HeadSha = "nope" }},
		{"unknown state", func(s *Snapshot) { s.State = "wat" }},
		{"two open rounds", func(s *Snapshot) {
			s.State = Repairing
			s.Attempts = []RepairAttempt{{Round: 1, BaseSha: sha1}, {Round: 2, BaseSha: sha1}}
		}},
		{"two verdicts for one head", func(s *Snapshot) {
			s.Verdicts = []CheckVerdict{{HeadSha: sha1, Conclusion: CheckSuccess}, {HeadSha: sha1, Conclusion: CheckFailure}}
		}},
		{"abandoned with no abandonment row", func(s *Snapshot) { s.State = Abandoned }},
		{"closed with no closure row", func(s *Snapshot) { s.State = Closed }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := validSnapshot()
			tc.mut(&s)
			if _, err := LoadBump(s); !errors.Is(err, resolution.ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

// The round cap reads retry grants back out of the transition log, so a
// loaded bump must count them the same way a live one does.
func TestLoadBumpRecoversRetryGrants(t *testing.T) {
	s := validSnapshot()
	s.State = Queued
	s.Attempts = []RepairAttempt{{Round: 1, BaseSha: sha1, Outcome: &RepairOutcome{Kind: NoChange, Summary: "none"}}}
	s.Transitions = []BumpTransition{{From: Abandoned, To: Queued, Cause: CauseOperatorRetry, OccurredAt: at(4)}}
	b, err := LoadBump(s)
	if err != nil {
		t.Fatalf("LoadBump: %v", err)
	}
	if _, err := b.StartRepair(testBudget(t), "fix it", at(5), 1); err != nil {
		t.Errorf("StartRepair at cap 1 with one grant: %v, want nil", err)
	}
}
