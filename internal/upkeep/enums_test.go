package upkeep

import (
	"errors"
	"testing"

	"github.com/guygrigsby/autophage/internal/resolution"
)

// Every vocabulary is a closed set that is also a seeded table in the store.
// Parsing must accept exactly the listed values and reject everything else.
func TestVocabulariesAreClosed(t *testing.T) {
	check := func(t *testing.T, name string, values []string, parse func(string) error) {
		t.Helper()
		for _, v := range values {
			if err := parse(v); err != nil {
				t.Errorf("%s: parse(%q) = %v, want nil", name, v, err)
			}
		}
		for _, v := range []string{"", "nope", "Success"} {
			if err := parse(v); !errors.Is(err, resolution.ErrInvalid) {
				t.Errorf("%s: parse(%q) = %v, want ErrInvalid", name, v, err)
			}
		}
	}

	t.Run("bump state", func(t *testing.T) {
		check(t, "bump state",
			[]string{"awaiting_checks", "queued", "repairing", "green", "abandoned", "closed"},
			func(v string) error { _, err := ParseBumpState(v); return err })
	})
	t.Run("check conclusion", func(t *testing.T) {
		check(t, "check conclusion", []string{"success", "failure"},
			func(v string) error { _, err := ParseCheckConclusion(v); return err })
	})
	t.Run("repair outcome kind", func(t *testing.T) {
		check(t, "repair outcome kind",
			[]string{"pushed", "no_change", "budget_exhausted", "failed", "aborted"},
			func(v string) error { _, err := ParseRepairOutcomeKind(v); return err })
	})
	t.Run("repair abort reason", func(t *testing.T) {
		check(t, "repair abort reason",
			[]string{"operator_stop", "daemon_restart", "pull_request_closed"},
			func(v string) error { _, err := ParseRepairAbortReason(v); return err })
	})
	t.Run("abandon reason", func(t *testing.T) {
		check(t, "abandon reason",
			[]string{"rounds_exhausted", "budget_exhausted", "repair_failed", "no_change", "checks_never_concluded", "operator_stop", "restart_failed"},
			func(v string) error { _, err := ParseAbandonReason(v); return err })
	})
	t.Run("closure kind", func(t *testing.T) {
		check(t, "closure kind", []string{"merged", "discarded"},
			func(v string) error { _, err := ParseClosureKind(v); return err })
	})
}

// issue_closed is Resolution's and must not be accepted here: a bump has no
// issue. The reverse (pull_request_closed in resolution) is that package's
// own business.
func TestRepairAbortReasonRejectsIssueClosed(t *testing.T) {
	if _, err := ParseRepairAbortReason("issue_closed"); !errors.Is(err, resolution.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

// Terminal is a single-value claim the state machine leans on hard: green
// and abandoned must stay non-terminal or a merge lands as a rejection.
func TestOnlyClosedIsTerminal(t *testing.T) {
	for _, s := range AllBumpStates() {
		want := s == Closed
		if got := s.Terminal(); got != want {
			t.Errorf("%s.Terminal() = %v, want %v", s, got, want)
		}
	}
}

// Every transition cause the aggregate can record must be in the vocabulary,
// or the store's FK rejects the row at persist time rather than here.
func TestTransitionCausesParse(t *testing.T) {
	for _, c := range AllBumpTransitionCauses() {
		if _, err := ParseBumpTransitionCause(string(c)); err != nil {
			t.Errorf("parse(%q) = %v, want nil", c, err)
		}
	}
	// Tripwire on the seeded vocabulary. The initial state is
	// not logged as a transition, so there is no 'opened' cause, exactly as
	// Resolution does not log a case's receipt.
	if len(AllBumpTransitionCauses()) != 17 {
		t.Errorf("causes = %d, want 17", len(AllBumpTransitionCauses()))
	}
}
