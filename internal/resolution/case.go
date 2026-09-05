package resolution

import (
	"fmt"
	"time"
)

// Case is the aggregate root: one issue in one enrolled repository that
// autophage is handling. It owns its facts, refuses every transition its
// table does not list, and tracks what changed since it was loaded so the
// store can persist exactly that in one transaction.
type Case struct {
	id          string
	repository  string
	number      int
	requester   Requester
	state       CaseState
	receivedAt  time.Time
	triage      *Triage
	approvals   []Approval
	attempts    []Attempt
	transitions []Transition
	closure     *Closure
	changes     Changes
}

// Changes is what happened to a Case since it was loaded. Runs and Outcomes
// are keyed by attempt ordinal because attempt ids belong to the store.
type Changes struct {
	Triage      *Triage
	Approvals   []Approval
	Attempts    []Attempt
	Runs        map[int]Run
	Outcomes    map[int]Outcome
	Closure     *Closure
	Transitions []Transition
	State       CaseState
}

// Snapshot is the store's view of a persisted Case, for LoadCase.
type Snapshot struct {
	ID          string
	Repository  string
	Number      int
	Requester   Requester
	State       CaseState
	ReceivedAt  time.Time
	Triage      *Triage
	Approvals   []Approval
	Attempts    []Attempt
	Transitions []Transition
	Closure     *Closure
}

// NewCase receives an issue. A trusted requester starts in Received, an
// untrusted one in Gated. There is no other way to create a Case.
func NewCase(repository string, number int, requester Requester, receivedAt time.Time) (*Case, error) {
	if repository == "" {
		return nil, Invalid("repository is empty")
	}
	if number < 1 {
		return nil, Invalid("issue number %d", number)
	}
	if requester.Login == "" {
		return nil, Invalid("requester is empty")
	}
	state := Gated
	if requester.Trust == Trusted {
		state = Received
	}
	c := &Case{repository: repository, number: number, requester: requester, state: state, receivedAt: receivedAt}
	c.changes.State = state
	return c, nil
}

// LoadCase rebuilds a Case from the store and re-checks the invariants a row
// could have lost: trust against initial state and at most one open attempt.
func LoadCase(s Snapshot) (*Case, error) {
	if s.Repository == "" || s.Number < 1 || s.Requester.Login == "" {
		return nil, Invalid("snapshot incomplete")
	}
	if (s.State == Gated && s.Requester.Trust == Trusted) || (s.State == Received && s.Requester.Trust == Untrusted) {
		return nil, Invalid("state %s contradicts trust %s", s.State, s.Requester.Trust)
	}
	open := 0
	for _, a := range s.Attempts {
		if a.Open() {
			open++
		}
	}
	if open > 1 {
		return nil, Invalid("%d open attempts", open)
	}
	c := &Case{
		id: s.ID, repository: s.Repository, number: s.Number, requester: s.Requester, state: s.State,
		receivedAt: s.ReceivedAt, triage: s.Triage, approvals: append([]Approval(nil), s.Approvals...),
		attempts: append([]Attempt(nil), s.Attempts...), transitions: append([]Transition(nil), s.Transitions...),
		closure: s.Closure,
	}
	c.changes.State = s.State
	return c, nil
}

func (c *Case) ID() string                { return c.id }
func (c *Case) Repository() string        { return c.repository }
func (c *Case) Number() int               { return c.number }
func (c *Case) Requester() Requester      { return c.requester }
func (c *Case) State() CaseState          { return c.state }
func (c *Case) ReceivedAt() time.Time     { return c.receivedAt }
func (c *Case) Triage() *Triage           { return c.triage }
func (c *Case) Approvals() []Approval     { return append([]Approval(nil), c.approvals...) }
func (c *Case) Attempts() []Attempt       { return append([]Attempt(nil), c.attempts...) }
func (c *Case) Transitions() []Transition { return append([]Transition(nil), c.transitions...) }
func (c *Case) Closure() *Closure         { return c.closure }

// Branch is the case's branch name in the repository.
func (c *Case) Branch() string { return fmt.Sprintf("autophage/%d", c.number) }

// OpenAttempt is the attempt without an outcome, or nil.
func (c *Case) OpenAttempt() *Attempt {
	for i := range c.attempts {
		if c.attempts[i].Open() {
			return &c.attempts[i]
		}
	}
	return nil
}

// NextAttemptKind is Approved when an approval postdates the latest attempt's
// start, or when there is an approval and no attempt; else Auto.
func (c *Case) NextAttemptKind() AttemptKind {
	if len(c.approvals) == 0 {
		return Auto
	}
	if len(c.attempts) == 0 {
		return Approved
	}
	last := c.attempts[len(c.attempts)-1].StartedAt
	for _, a := range c.approvals {
		if !a.ApprovedAt.Before(last) {
			return Approved
		}
	}
	return Auto
}

// Changes reports what changed since load; ClearChanges resets after persist.
func (c *Case) Changes() Changes { return c.changes }
func (c *Case) ClearChanges()    { c.changes = Changes{State: c.state} }

// RecordTriage sizes a Received case. Small queues it, Large parks it.
func (c *Case) RecordTriage(t Triage) error {
	if _, err := ParseSize(string(t.Size)); err != nil {
		return err
	}
	if t.Rationale == "" || t.Model == "" {
		return Invalid("triage rationale and model are required")
	}
	if c.state != Received {
		return Refused("triage in state %s", c.state)
	}
	if c.triage != nil {
		return Refused("already triaged")
	}
	c.triage = &t
	c.changes.Triage = &t
	if t.Size == Small {
		c.move(Queued, CauseTriageSmall, t.TriagedAt)
	} else {
		c.move(AwaitingApproval, CauseTriageLarge, t.TriagedAt)
	}
	return nil
}

// RecordApproval records the label or operator approval and queues the case
// from Gated, AwaitingApproval or Failed. Elsewhere it is recorded without a
// transition; in a terminal state it is refused.
func (c *Case) RecordApproval(a Approval) error {
	if a.Approver.Login == "" {
		return Invalid("approver is empty")
	}
	if _, err := ParseApprovalSource(string(a.Source)); err != nil {
		return err
	}
	if (a.Source == SourceLabel) != (a.DeliveryID != "") {
		return Invalid("delivery id must be set exactly for label approvals")
	}
	if c.state.Terminal() {
		return Refused("approval in state %s", c.state)
	}
	c.approvals = append(c.approvals, a)
	c.changes.Approvals = append(c.changes.Approvals, a)
	switch c.state {
	case Gated, AwaitingApproval, Failed:
		c.move(Queued, CauseApproval, a.ApprovedAt)
	}
	return nil
}

// StartAttempt opens the next attempt on a Queued case.
func (c *Case) StartAttempt(kind AttemptKind, budget Budget, brief string, at time.Time) (Attempt, error) {
	if _, err := ParseAttemptKind(string(kind)); err != nil {
		return Attempt{}, err
	}
	if budget.MaxTurns() == 0 {
		return Attempt{}, Invalid("budget is zero")
	}
	if brief == "" {
		return Attempt{}, Invalid("brief is empty")
	}
	if c.state != Queued {
		return Attempt{}, Refused("start attempt in state %s", c.state)
	}
	if c.OpenAttempt() != nil {
		return Attempt{}, Refused("an attempt is already open")
	}
	a := Attempt{Ordinal: len(c.attempts) + 1, Kind: kind, Budget: budget, Brief: brief, StartedAt: at}
	c.attempts = append(c.attempts, a)
	c.changes.Attempts = append(c.changes.Attempts, a)
	c.move(Attempting, CauseAttemptStarted, at)
	return a, nil
}

// RecordRun marks the open attempt as having reached the agent. attemptID,
// when non-empty, must name the open attempt.
func (c *Case) RecordRun(attemptID string, r Run) error {
	if r.RunID == "" || r.Model == "" || !shaRe.MatchString(r.BaseSha) {
		return Invalid("run needs run id, model and a 40-hex base sha")
	}
	a, err := c.target(attemptID)
	if err != nil {
		return err
	}
	if a.Run != nil {
		return Refused("run already recorded for attempt %d", a.Ordinal)
	}
	a.Run = &r
	if c.changes.Runs == nil {
		c.changes.Runs = map[int]Run{}
	}
	c.changes.Runs[a.Ordinal] = r
	return nil
}

// RecordOutcome ends the open attempt and moves the case by outcome kind. In
// Closed the outcome is recorded and the state stays Closed.
func (c *Case) RecordOutcome(attemptID string, o Outcome) error {
	if _, err := ParseOutcomeKind(string(o.Kind)); err != nil {
		return err
	}
	if o.Summary == "" {
		return Invalid("outcome summary is empty")
	}
	a, err := c.target(attemptID)
	if err != nil {
		return err
	}
	a.Outcome = &o
	if c.changes.Outcomes == nil {
		c.changes.Outcomes = map[int]Outcome{}
	}
	c.changes.Outcomes[a.Ordinal] = o
	if c.state == Closed {
		return nil
	}
	switch o.Kind {
	case PullRequestOpened:
		c.move(Done, o.cause(false), o.EndedAt)
	case BudgetExhausted:
		c.move(AwaitingApproval, o.cause(false), o.EndedAt)
	case FailedOutcome:
		c.move(Failed, o.cause(false), o.EndedAt)
	case Aborted:
		switch o.Reason {
		case AbortOperatorStop:
			c.move(AwaitingApproval, o.cause(false), o.EndedAt)
		case AbortDaemonRestart:
			if c.restartedBefore(a.Ordinal) {
				c.move(Failed, o.cause(false), o.EndedAt)
			} else {
				c.move(Queued, o.cause(true), o.EndedAt)
			}
		default:
			return Refused("abort reason %s outside Closed", o.Reason)
		}
	}
	return nil
}

// Close records the issue closing. Any non-terminal state goes to Closed; a
// running attempt is aborted by the application afterwards.
func (c *Case) Close(cl Closure) error {
	if cl.DeliveryID == "" {
		return Invalid("closure delivery id is empty")
	}
	if c.state.Terminal() {
		return Refused("close in state %s", c.state)
	}
	c.closure = &cl
	c.changes.Closure = &cl
	c.move(Closed, CauseClosed, cl.ClosedAt)
	return nil
}

// target returns the open attempt, checking attemptID against it when both
// are known.
func (c *Case) target(attemptID string) (*Attempt, error) {
	a := c.OpenAttempt()
	if a == nil {
		return nil, Refused("no open attempt")
	}
	if attemptID != "" && a.ID != "" && a.ID != attemptID {
		return nil, Refused("attempt %s is not the open attempt", attemptID)
	}
	return a, nil
}

// restartedBefore reports whether an attempt before ordinal ended with a
// daemon-restart abort, which makes this restart the second.
func (c *Case) restartedBefore(ordinal int) bool {
	for _, a := range c.attempts {
		if a.Ordinal < ordinal && a.Outcome != nil && a.Outcome.Kind == Aborted && a.Outcome.Reason == AbortDaemonRestart {
			return true
		}
	}
	return false
}

// move records a transition. Every state change goes through here.
func (c *Case) move(to CaseState, cause TransitionCause, at time.Time) {
	tr := Transition{From: c.state, To: to, Cause: cause, OccurredAt: at}
	c.state = to
	c.transitions = append(c.transitions, tr)
	c.changes.Transitions = append(c.changes.Transitions, tr)
	c.changes.State = to
}
