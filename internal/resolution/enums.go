package resolution

import "strings"

// Every enumeration here is a closed vocabulary that is also a seeded table
// in the store. Values are the table rows verbatim.

type CaseState string

const (
	Received         CaseState = "received"
	Gated            CaseState = "gated"
	Queued           CaseState = "queued"
	Attempting       CaseState = "attempting"
	AwaitingApproval CaseState = "awaiting_approval"
	Failed           CaseState = "failed"
	Done             CaseState = "done"
	Closed           CaseState = "closed"
)

// AllCaseStates lists every state, for the store's seed and for tests.
func AllCaseStates() []CaseState {
	return []CaseState{Received, Gated, Queued, Attempting, AwaitingApproval, Failed, Done, Closed}
}

// Terminal reports whether no transition leaves the state.
func (s CaseState) Terminal() bool { return s == Done || s == Closed }

func ParseCaseState(v string) (CaseState, error) {
	return parseEnum("case state", v, AllCaseStates())
}

type Trust string

const (
	Trusted   Trust = "trusted"
	Untrusted Trust = "untrusted"
)

func ParseTrust(v string) (Trust, error) { return parseEnum("trust", v, []Trust{Trusted, Untrusted}) }

// Association is GitHub's author_association, recorded as evidence.
type Association string

const (
	AssociationOwner                Association = "owner"
	AssociationMember               Association = "member"
	AssociationCollaborator         Association = "collaborator"
	AssociationContributor          Association = "contributor"
	AssociationFirstTimeContributor Association = "first_time_contributor"
	AssociationFirstTimer           Association = "first_timer"
	AssociationMannequin            Association = "mannequin"
	AssociationNone                 Association = "none"
)

func AllAssociations() []Association {
	return []Association{AssociationOwner, AssociationMember, AssociationCollaborator, AssociationContributor, AssociationFirstTimeContributor, AssociationFirstTimer, AssociationMannequin, AssociationNone}
}

// ParseAssociation accepts GitHub's upper-case spelling as well as ours.
func ParseAssociation(v string) (Association, error) {
	return parseEnum("association", strings.ToLower(v), AllAssociations())
}

type Size string

const (
	Small Size = "small"
	Large Size = "large"
)

func ParseSize(v string) (Size, error) { return parseEnum("size", v, []Size{Small, Large}) }

type AttemptKind string

const (
	Auto     AttemptKind = "auto"
	Approved AttemptKind = "approved"
)

func ParseAttemptKind(v string) (AttemptKind, error) {
	return parseEnum("attempt kind", v, []AttemptKind{Auto, Approved})
}

type OutcomeKind string

const (
	PullRequestOpened OutcomeKind = "pull_request_opened"
	BudgetExhausted   OutcomeKind = "budget_exhausted"
	FailedOutcome     OutcomeKind = "failed"
	Aborted           OutcomeKind = "aborted"
)

func ParseOutcomeKind(v string) (OutcomeKind, error) {
	return parseEnum("outcome kind", v, []OutcomeKind{PullRequestOpened, BudgetExhausted, FailedOutcome, Aborted})
}

type Limit string

const (
	LimitTurns     Limit = "turns"
	LimitWallClock Limit = "wall_clock"
	LimitDiffLines Limit = "diff_lines"
)

func ParseLimit(v string) (Limit, error) {
	return parseEnum("limit", v, []Limit{LimitTurns, LimitWallClock, LimitDiffLines})
}

type FailureClass string

const (
	FailureInfra FailureClass = "infra"
	FailureModel FailureClass = "model"
	FailureAgent FailureClass = "agent"
)

func ParseFailureClass(v string) (FailureClass, error) {
	return parseEnum("failure class", v, []FailureClass{FailureInfra, FailureModel, FailureAgent})
}

type AbortReason string

const (
	AbortIssueClosed   AbortReason = "issue_closed"
	AbortOperatorStop  AbortReason = "operator_stop"
	AbortDaemonRestart AbortReason = "daemon_restart"
)

func ParseAbortReason(v string) (AbortReason, error) {
	return parseEnum("abort reason", v, []AbortReason{AbortIssueClosed, AbortOperatorStop, AbortDaemonRestart})
}

type ApprovalSource string

const (
	SourceLabel    ApprovalSource = "label"
	SourceOperator ApprovalSource = "operator"
)

func ParseApprovalSource(v string) (ApprovalSource, error) {
	return parseEnum("approval source", v, []ApprovalSource{SourceLabel, SourceOperator})
}

type TransitionCause string

const (
	CauseTriageSmall                  TransitionCause = "triage_small"
	CauseTriageLarge                  TransitionCause = "triage_large"
	CauseApproval                     TransitionCause = "approval"
	CauseAttemptStarted               TransitionCause = "attempt_started"
	CauseOutcomePullRequestOpened     TransitionCause = "outcome_pull_request_opened"
	CauseOutcomeBudgetExhausted       TransitionCause = "outcome_budget_exhausted"
	CauseOutcomeFailed                TransitionCause = "outcome_failed"
	CauseOutcomeAbortedOperatorStop   TransitionCause = "outcome_aborted_operator_stop"
	CauseOutcomeAbortedRestartRequeue TransitionCause = "outcome_aborted_daemon_restart_requeued"
	CauseOutcomeAbortedRestartFailed  TransitionCause = "outcome_aborted_daemon_restart_failed"
	CauseClosed                       TransitionCause = "closed"
)

func ParseTransitionCause(v string) (TransitionCause, error) {
	return parseEnum("transition cause", v, []TransitionCause{CauseTriageSmall, CauseTriageLarge, CauseApproval, CauseAttemptStarted, CauseOutcomePullRequestOpened, CauseOutcomeBudgetExhausted, CauseOutcomeFailed, CauseOutcomeAbortedOperatorStop, CauseOutcomeAbortedRestartRequeue, CauseOutcomeAbortedRestartFailed, CauseClosed})
}

// parseEnum matches v against the closed set or returns ErrInvalid.
func parseEnum[T ~string](name, v string, all []T) (T, error) {
	for _, x := range all {
		if string(x) == v {
			return x, nil
		}
	}
	var zero T
	return zero, Invalid("unknown %s %q", name, v)
}
