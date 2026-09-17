package upkeep

import (
	"regexp"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
)

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// CheckVerdict is the conclusive CI rollup for one head sha. It exists only
// when GitHub had no run still queued or in progress: a pending rollup
// records nothing, which is why there is no pending conclusion.
type CheckVerdict struct {
	ID              string
	HeadSha         string
	Conclusion      CheckConclusion
	FailingContexts string // newline-separated run names, verbatim; empty iff success
	DetailsURL      string // empty when GitHub gave none; FailingContexts is the evidence
	ConcludedAt     time.Time
}

// RepairAttempt is one budgeted round at making a red bump's checks pass, on
// dependabot's own branch. ID is the store's; empty until persisted.
type RepairAttempt struct {
	ID        string
	Round     int
	BaseSha   string
	Budget    resolution.Budget
	Brief     string
	StartedAt time.Time
	Run       *resolution.Run
	Outcome   *RepairOutcome
}

// Open reports whether the round has no outcome yet.
func (a RepairAttempt) Open() bool { return a.Outcome == nil }

// Elapsed is how long the round has been running as of now.
func (a RepairAttempt) Elapsed(now time.Time) time.Duration { return now.Sub(a.StartedAt) }

// RepairOutcome is a sum: Kind says which variant fields are meaningful.
// Pushed: HeadSha. BudgetExhausted: Limit. Failed: Class, Message, and the
// class decides whether the bump requeues or abandons. Aborted: Reason.
// NoChange has no variant fields; Summary carries the evidence. Summary is
// always set; for a failure it is the message.
type RepairOutcome struct {
	Kind    RepairOutcomeKind
	EndedAt time.Time
	Usage   resolution.Usage
	Summary string
	HeadSha string
	Limit   resolution.Limit
	Class   resolution.FailureClass
	Message string
	Reason  RepairAbortReason
}

// BumpAbandonment records that autophage has stopped trying. It is not
// derivable from the last outcome: checks_never_concluded abandons a bump on
// which no round ever ran.
type BumpAbandonment struct {
	Reason      AbandonReason
	Detail      string
	AbandonedAt time.Time
}

// BumpClosure records the pull request closing on GitHub.
type BumpClosure struct {
	Kind       ClosureKind
	DeliveryID string
	ClosedAt   time.Time
}

// BumpTransition is one recorded state change.
type BumpTransition struct {
	From       BumpState
	To         BumpState
	Cause      BumpTransitionCause
	OccurredAt time.Time
}

// Watch is a repository Upkeep acts on. Enrollment authorises issues; this
// authorises bumps. Deleting it stops new bumps only.
type Watch struct {
	Repository string
	WatchedAt  time.Time
}

// NewWatch is the only way to build one.
func NewWatch(repository string, at time.Time) (Watch, error) {
	if repository == "" {
		return Watch{}, resolution.Invalid("repository is empty")
	}
	return Watch{Repository: repository, WatchedAt: at}, nil
}
