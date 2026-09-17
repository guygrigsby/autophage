package upkeep

import "github.com/guygrigsby/autophage/internal/resolution"

// Every enumeration here is a closed vocabulary that is also a seeded table
// in the store. Values are the table rows verbatim.

type BumpState string

const (
	AwaitingChecks BumpState = "awaiting_checks"
	Queued         BumpState = "queued"
	Repairing      BumpState = "repairing"
	Green          BumpState = "green"
	Abandoned      BumpState = "abandoned"
	Closed         BumpState = "closed"
)

// AllBumpStates lists every state, for the store's seed and for tests.
func AllBumpStates() []BumpState {
	return []BumpState{AwaitingChecks, Queued, Repairing, Green, Abandoned, Closed}
}

// Terminal reports whether no transition leaves the state. Only Closed is.
// Green is deliberately not: someone merging the pull request is the most
// ordinary end to a green bump and it arrives as a delivery afterwards, so a
// terminal Green would record the normal ending as a rejection. Abandoned is
// not terminal for the same reason, and because an operator may retry it.
func (s BumpState) Terminal() bool { return s == Closed }

func ParseBumpState(v string) (BumpState, error) {
	return parseEnum("bump state", v, AllBumpStates())
}

// CheckConclusion is the rollup verdict, not GitHub's per-run vocabulary.
// There is no pending value: an unconcluded rollup records no verdict at
// all, and that absence has a deadline.
type CheckConclusion string

const (
	CheckSuccess CheckConclusion = "success"
	CheckFailure CheckConclusion = "failure"
)

func AllCheckConclusions() []CheckConclusion {
	return []CheckConclusion{CheckSuccess, CheckFailure}
}

func ParseCheckConclusion(v string) (CheckConclusion, error) {
	return parseEnum("check conclusion", v, AllCheckConclusions())
}

type RepairOutcomeKind string

const (
	Pushed          RepairOutcomeKind = "pushed"
	NoChange        RepairOutcomeKind = "no_change"
	BudgetExhausted RepairOutcomeKind = "budget_exhausted"
	RepairFailed    RepairOutcomeKind = "failed"
	RepairAborted   RepairOutcomeKind = "aborted"
)

func AllRepairOutcomeKinds() []RepairOutcomeKind {
	return []RepairOutcomeKind{Pushed, NoChange, BudgetExhausted, RepairFailed, RepairAborted}
}

func ParseRepairOutcomeKind(v string) (RepairOutcomeKind, error) {
	return parseEnum("repair outcome kind", v, AllRepairOutcomeKinds())
}

// RepairAbortReason is Upkeep's own, not Resolution's AbortReason:
// issue_closed has no meaning for a bump and pull_request_closed has none
// for a case, so one shared vocabulary would carry rows invalid on one side.
type RepairAbortReason string

const (
	AbortOperatorStop      RepairAbortReason = "operator_stop"
	AbortDaemonRestart     RepairAbortReason = "daemon_restart"
	AbortPullRequestClosed RepairAbortReason = "pull_request_closed"
)

func AllRepairAbortReasons() []RepairAbortReason {
	return []RepairAbortReason{AbortOperatorStop, AbortDaemonRestart, AbortPullRequestClosed}
}

func ParseRepairAbortReason(v string) (RepairAbortReason, error) {
	return parseEnum("repair abort reason", v, AllRepairAbortReasons())
}

type AbandonReason string

const (
	AbandonRoundsExhausted      AbandonReason = "rounds_exhausted"
	AbandonBudgetExhausted      AbandonReason = "budget_exhausted"
	AbandonRepairFailed         AbandonReason = "repair_failed"
	AbandonNoChange             AbandonReason = "no_change"
	AbandonChecksNeverConcluded AbandonReason = "checks_never_concluded"
	AbandonOperatorStop         AbandonReason = "operator_stop"
	AbandonRestartFailed        AbandonReason = "restart_failed"
)

func AllAbandonReasons() []AbandonReason {
	return []AbandonReason{AbandonRoundsExhausted, AbandonBudgetExhausted, AbandonRepairFailed, AbandonNoChange, AbandonChecksNeverConcluded, AbandonOperatorStop, AbandonRestartFailed}
}

func ParseAbandonReason(v string) (AbandonReason, error) {
	return parseEnum("abandon reason", v, AllAbandonReasons())
}

type ClosureKind string

const (
	Merged    ClosureKind = "merged"
	Discarded ClosureKind = "discarded"
)

func AllClosureKinds() []ClosureKind { return []ClosureKind{Merged, Discarded} }

func ParseClosureKind(v string) (ClosureKind, error) {
	return parseEnum("closure kind", v, AllClosureKinds())
}

// BumpTransitionCause has one value per legal transition, so the transition
// log reads without joining anything. The initial state is not logged; it is
// awaiting_checks at openedAt, as Resolution does for a case's receipt.
type BumpTransitionCause string

const (
	CauseVerdictSuccess            BumpTransitionCause = "verdict_success"
	CauseVerdictFailure            BumpTransitionCause = "verdict_failure"
	CauseVerdictFailureCapReached  BumpTransitionCause = "verdict_failure_cap_reached"
	CauseChecksNeverConcluded      BumpTransitionCause = "checks_never_concluded"
	CauseRepairStarted             BumpTransitionCause = "repair_started"
	CauseRepairPushed              BumpTransitionCause = "repair_pushed"
	CauseRepairNoChange            BumpTransitionCause = "repair_no_change"
	CauseRepairBudgetExhausted     BumpTransitionCause = "repair_budget_exhausted"
	CauseRepairFailedInfraRequeued BumpTransitionCause = "repair_failed_infra_requeued"
	// CauseRepairRequeueCapped is a requeue the cap would never let run, so
	// the bump abandons instead of sitting queued for ever.
	CauseRepairRequeueCapped         BumpTransitionCause = "repair_requeue_capped"
	CauseRepairFailedAbandoned       BumpTransitionCause = "repair_failed_abandoned"
	CauseRepairAbortedOperatorStop   BumpTransitionCause = "repair_aborted_operator_stop"
	CauseRepairAbortedRestartRequeue BumpTransitionCause = "repair_aborted_restart_requeued"
	CauseRepairAbortedRestartAbandon BumpTransitionCause = "repair_aborted_restart_abandoned"
	CauseHeadAdvanced                BumpTransitionCause = "head_advanced"
	CauseOperatorRetry               BumpTransitionCause = "operator_retry"
	CauseBumpClosed                  BumpTransitionCause = "closed"
)

func AllBumpTransitionCauses() []BumpTransitionCause {
	return []BumpTransitionCause{
		CauseVerdictSuccess, CauseVerdictFailure, CauseVerdictFailureCapReached,
		CauseChecksNeverConcluded, CauseRepairStarted, CauseRepairPushed,
		CauseRepairNoChange, CauseRepairBudgetExhausted, CauseRepairFailedInfraRequeued,
		CauseRepairRequeueCapped,
		CauseRepairFailedAbandoned, CauseRepairAbortedOperatorStop,
		CauseRepairAbortedRestartRequeue, CauseRepairAbortedRestartAbandon,
		CauseHeadAdvanced, CauseOperatorRetry, CauseBumpClosed,
	}
}

func ParseBumpTransitionCause(v string) (BumpTransitionCause, error) {
	return parseEnum("bump transition cause", v, AllBumpTransitionCauses())
}

// parseEnum matches v against the closed set or returns resolution.ErrInvalid,
// so a caller mapping domain errors handles both contexts the same way.
func parseEnum[T ~string](name, v string, all []T) (T, error) {
	for _, x := range all {
		if string(x) == v {
			return x, nil
		}
	}
	var zero T
	return zero, resolution.Invalid("unknown %s %q", name, v)
}
