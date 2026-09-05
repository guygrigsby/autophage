package resolution

import (
	"regexp"
	"time"
)

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Triage is the size verdict on a trusted case. Rationale is posted verbatim
// and never executed.
type Triage struct {
	Size      Size
	Rationale string
	Model     string
	TriagedAt time.Time
}

// Approval is one approved label add or one operator run. DeliveryID is set
// exactly when Source is label.
type Approval struct {
	ID         string
	Approver   Requester
	Source     ApprovalSource
	ApprovedAt time.Time
	DeliveryID string
}

// Closure records the issue being closed on GitHub.
type Closure struct {
	DeliveryID string
	ClosedAt   time.Time
}

// Transition is one recorded state change.
type Transition struct {
	From       CaseState
	To         CaseState
	Cause      TransitionCause
	OccurredAt time.Time
}

// Attempt is one budgeted try. ID is the store's; empty until persisted.
type Attempt struct {
	ID        string
	Ordinal   int
	Kind      AttemptKind
	Budget    Budget
	Brief     string
	StartedAt time.Time
	Run       *Run
	Outcome   *Outcome
}

// Open reports whether the attempt has no outcome yet.
func (a Attempt) Open() bool { return a.Outcome == nil }

// Elapsed is how long the attempt has been running as of now.
func (a Attempt) Elapsed(now time.Time) time.Duration { return now.Sub(a.StartedAt) }

// Run exists once the agent began executing.
type Run struct {
	RunID   string
	Model   string
	BaseSha string
	BeganAt time.Time
}

// Outcome is a sum: Kind says which variant fields are meaningful.
// PullRequestOpened: PRNumber, HeadSha. BudgetExhausted: Limit.
// Failed: Class, Message. Aborted: Reason. Summary is always set; for a
// failure it is the message.
type Outcome struct {
	Kind     OutcomeKind
	EndedAt  time.Time
	Usage    Usage
	Summary  string
	PRNumber int
	HeadSha  string
	Limit    Limit
	Class    FailureClass
	Message  string
	Reason   AbortReason
}

func OutcomePullRequest(prNumber int, headSha, summary string, u Usage, at time.Time) (Outcome, error) {
	if prNumber < 1 {
		return Outcome{}, Invalid("pull request number %d", prNumber)
	}
	if !shaRe.MatchString(headSha) {
		return Outcome{}, Invalid("head sha %q", headSha)
	}
	if summary == "" {
		return Outcome{}, Invalid("outcome summary is empty")
	}
	return Outcome{Kind: PullRequestOpened, EndedAt: at, Usage: u, Summary: summary, PRNumber: prNumber, HeadSha: headSha}, nil
}

func OutcomeExhausted(limit Limit, summary string, u Usage, at time.Time) (Outcome, error) {
	if _, err := ParseLimit(string(limit)); err != nil {
		return Outcome{}, err
	}
	if summary == "" {
		return Outcome{}, Invalid("outcome summary is empty")
	}
	return Outcome{Kind: BudgetExhausted, EndedAt: at, Usage: u, Summary: summary, Limit: limit}, nil
}

func OutcomeFailed(class FailureClass, message string, u Usage, at time.Time) (Outcome, error) {
	if _, err := ParseFailureClass(string(class)); err != nil {
		return Outcome{}, err
	}
	if message == "" {
		return Outcome{}, Invalid("failure message is empty")
	}
	return Outcome{Kind: FailedOutcome, EndedAt: at, Usage: u, Summary: message, Class: class, Message: message}, nil
}

func OutcomeAborted(reason AbortReason, summary string, u Usage, at time.Time) (Outcome, error) {
	if _, err := ParseAbortReason(string(reason)); err != nil {
		return Outcome{}, err
	}
	if summary == "" {
		return Outcome{}, Invalid("outcome summary is empty")
	}
	return Outcome{Kind: Aborted, EndedAt: at, Usage: u, Summary: summary, Reason: reason}, nil
}

// Cause is the transition cause an outcome produces, with requeued saying
// whether a daemon-restart abort re-queues (first time) or fails (second).
func (o Outcome) cause(requeued bool) TransitionCause {
	switch o.Kind {
	case PullRequestOpened:
		return CauseOutcomePullRequestOpened
	case BudgetExhausted:
		return CauseOutcomeBudgetExhausted
	case FailedOutcome:
		return CauseOutcomeFailed
	}
	switch o.Reason {
	case AbortOperatorStop:
		return CauseOutcomeAbortedOperatorStop
	case AbortDaemonRestart:
		if requeued {
			return CauseOutcomeAbortedRestartRequeue
		}
		return CauseOutcomeAbortedRestartFailed
	}
	return CauseClosed
}
