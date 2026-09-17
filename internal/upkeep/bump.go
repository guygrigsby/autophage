package upkeep

import (
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
)

// Bump is the aggregate root: one dependabot pull request on a watched
// repository that autophage is shepherding to green. It owns its facts,
// refuses every transition its table does not list and tracks what changed
// since it was loaded so the store can persist exactly that in one
// transaction.
type Bump struct {
	id          string
	repository  string
	number      int
	branch      string
	baseBranch  string
	headSha     string
	state       BumpState
	openedAt    time.Time
	verdicts    []CheckVerdict
	attempts    []RepairAttempt
	transitions []BumpTransition
	abandonment *BumpAbandonment
	closure     *BumpClosure
	changes     Changes
}

// Changes is what happened to a Bump since it was loaded. Runs and Outcomes
// are keyed by round because attempt ids belong to the store.
type Changes struct {
	Verdicts    []CheckVerdict
	Attempts    []RepairAttempt
	Runs        map[int]resolution.Run
	Outcomes    map[int]RepairOutcome
	Abandonment *BumpAbandonment
	DropAbandon bool // an operator retry deletes the row rather than appending
	Closure     *BumpClosure
	Transitions []BumpTransition
	HeadSha     string
	State       BumpState
}

// NewBump receives a dependabot pull request. That the author is the
// dependabot login and that the repository has a Watch are checked by the
// caller before this is reached: both are facts about the world, not about
// the aggregate, and a watch deleted later must not invalidate the bump.
func NewBump(repository string, number int, branch, baseBranch, headSha string, openedAt time.Time) (*Bump, error) {
	if repository == "" {
		return nil, resolution.Invalid("repository is empty")
	}
	if number < 1 {
		return nil, resolution.Invalid("pull request number %d", number)
	}
	if branch == "" {
		return nil, resolution.Invalid("branch is empty")
	}
	if baseBranch == "" {
		return nil, resolution.Invalid("base branch is empty")
	}
	if !shaRe.MatchString(headSha) {
		return nil, resolution.Invalid("head sha %q", headSha)
	}
	b := &Bump{
		repository: repository, number: number, branch: branch, baseBranch: baseBranch,
		headSha: headSha, state: AwaitingChecks, openedAt: openedAt,
	}
	b.changes.State = AwaitingChecks
	b.changes.HeadSha = headSha
	return b, nil
}

func (b *Bump) ID() string          { return b.id }
func (b *Bump) Repository() string  { return b.repository }
func (b *Bump) Number() int         { return b.number }
func (b *Bump) Branch() string      { return b.branch }
func (b *Bump) BaseBranch() string  { return b.baseBranch }
func (b *Bump) HeadSha() string     { return b.headSha }
func (b *Bump) State() BumpState    { return b.state }
func (b *Bump) OpenedAt() time.Time { return b.openedAt }

// Rounds is how many repair rounds this bump has spent. The cap counts it.
func (b *Bump) Rounds() int { return len(b.attempts) }

// AssignID sets the store-generated id once, right after CreateBump.
func (b *Bump) AssignID(id string) error {
	if id == "" {
		return resolution.Invalid("id is empty")
	}
	if b.id != "" {
		return resolution.Refused("id already assigned")
	}
	b.id = id
	return nil
}

// Changes reports what changed since load; ClearChanges resets after persist.
func (b *Bump) Changes() Changes { return b.changes }
func (b *Bump) ClearChanges()    { b.changes = Changes{State: b.state, HeadSha: b.headSha} }

// move records a transition. Every state change goes through here.
func (b *Bump) move(to BumpState, cause BumpTransitionCause, at time.Time) {
	tr := BumpTransition{From: b.state, To: to, Cause: cause, OccurredAt: at}
	b.state = to
	b.transitions = append(b.transitions, tr)
	b.changes.Transitions = append(b.changes.Transitions, tr)
	b.changes.State = to
}

// Verdicts is a copy of every conclusive rollup recorded, oldest first.
func (b *Bump) Verdicts() []CheckVerdict { return append([]CheckVerdict(nil), b.verdicts...) }

// Attempts is a copy of every repair round, oldest first. The copies are
// shallow by design: Run and Outcome are pointers the caller must not write
// through, which is why OpenRepair deep-copies the one round that matters.
func (b *Bump) Attempts() []RepairAttempt { return append([]RepairAttempt(nil), b.attempts...) }

// Transitions is a copy of the state change log.
func (b *Bump) Transitions() []BumpTransition {
	return append([]BumpTransition(nil), b.transitions...)
}

// Abandonment is a copy of why autophage stopped, or nil while it has not.
func (b *Bump) Abandonment() *BumpAbandonment {
	if b.abandonment == nil {
		return nil
	}
	a := *b.abandonment
	return &a
}

// Closure is a copy of the pull request closing, or nil while it is open.
func (b *Bump) Closure() *BumpClosure {
	if b.closure == nil {
		return nil
	}
	cl := *b.closure
	return &cl
}

// CurrentVerdict is the verdict for the head sha the bump is on now, or nil
// when the checks for that head have not concluded.
func (b *Bump) CurrentVerdict() *CheckVerdict {
	for i := range b.verdicts {
		if b.verdicts[i].HeadSha == b.headSha {
			v := b.verdicts[i]
			return &v
		}
	}
	return nil
}

// OpenRepair is a deep copy of the round without an outcome, or nil. Its Run
// and Outcome are copied too, so mutating any of them does not reach the
// aggregate; RecordRun and RecordOutcome are the only way in.
func (b *Bump) OpenRepair() *RepairAttempt {
	a := b.openRepair()
	if a == nil {
		return nil
	}
	cp := *a
	if a.Run != nil {
		r := *a.Run
		cp.Run = &r
	}
	if a.Outcome != nil {
		o := *a.Outcome
		cp.Outcome = &o
	}
	return &cp
}

// openRepair is the real, mutable round without an outcome, for commands
// that need to change it.
func (b *Bump) openRepair() *RepairAttempt {
	for i := range b.attempts {
		if b.attempts[i].Open() {
			return &b.attempts[i]
		}
	}
	return nil
}

// retryGrants is how many further rounds operators have granted, derived
// from the transition log rather than stored: an operator retry is exactly
// the transitions with that cause.
func (b *Bump) retryGrants() int {
	n := 0
	for _, tr := range b.transitions {
		if tr.Cause == CauseOperatorRetry {
			n++
		}
	}
	return n
}

// cap is how many rounds this bump may spend: the configured cap plus one
// per operator retry.
func (b *Bump) cap(maxRounds int) int { return maxRounds + b.retryGrants() }

// RecordVerdict records a conclusive CI rollup and moves the bump by its
// conclusion. A verdict for anything but the current head is refused, which
// is the loop guard: a late rollup for a head a push or a force-push already
// replaced can never move the bump.
func (b *Bump) RecordVerdict(v CheckVerdict, maxRounds int) error {
	if _, err := ParseCheckConclusion(string(v.Conclusion)); err != nil {
		return err
	}
	if !shaRe.MatchString(v.HeadSha) {
		return resolution.Invalid("verdict head sha %q", v.HeadSha)
	}
	if (v.Conclusion == CheckSuccess) != (v.FailingContexts == "") {
		return resolution.Invalid("failing contexts must be set exactly for a failure")
	}
	if b.state != AwaitingChecks {
		return resolution.Refused("verdict in state %s", b.state)
	}
	if v.HeadSha != b.headSha {
		return resolution.Refused("verdict for %s is not the current head %s", v.HeadSha, b.headSha)
	}
	for _, seen := range b.verdicts {
		if seen.HeadSha == v.HeadSha {
			return resolution.Refused("head %s already has a verdict", v.HeadSha)
		}
	}
	b.verdicts = append(b.verdicts, v)
	b.changes.Verdicts = append(b.changes.Verdicts, v)
	switch {
	case v.Conclusion == CheckSuccess:
		b.move(Green, CauseVerdictSuccess, v.ConcludedAt)
	case b.Rounds() < b.cap(maxRounds):
		b.move(Queued, CauseVerdictFailure, v.ConcludedAt)
	default:
		b.setAbandonment(AbandonRoundsExhausted, v.FailingContexts, v.ConcludedAt)
		b.move(Abandoned, CauseVerdictFailureCapReached, v.ConcludedAt)
	}
	return nil
}

// AdvanceHead records the branch tip moving, whoever moved it. From
// awaiting_checks the state is already right and no transition is logged,
// which is also what the store's from_state <> to_state check requires. An
// abandoned bump is not revived: a rebase of a version autophage already
// failed to repair is not new information.
func (b *Bump) AdvanceHead(sha string, at time.Time) error {
	if !shaRe.MatchString(sha) {
		return resolution.Invalid("head sha %q", sha)
	}
	if b.state == Closed || b.state == Abandoned {
		return resolution.Refused("advance head in state %s", b.state)
	}
	if sha == b.headSha {
		return resolution.Refused("head is already %s", sha)
	}
	b.headSha = sha
	b.changes.HeadSha = sha
	if b.state != AwaitingChecks {
		b.move(AwaitingChecks, CauseHeadAdvanced, at)
	}
	return nil
}

// StartRepair opens the next round on a queued bump, from the head the
// failing verdict was about.
func (b *Bump) StartRepair(budget resolution.Budget, brief string, at time.Time, maxRounds int) (RepairAttempt, error) {
	if budget.MaxTurns() == 0 {
		return RepairAttempt{}, resolution.Invalid("budget is zero")
	}
	if brief == "" {
		return RepairAttempt{}, resolution.Invalid("brief is empty")
	}
	if b.state != Queued {
		return RepairAttempt{}, resolution.Refused("start repair in state %s", b.state)
	}
	if b.openRepair() != nil {
		return RepairAttempt{}, resolution.Refused("a round is already open")
	}
	if b.Rounds() >= b.cap(maxRounds) {
		return RepairAttempt{}, resolution.Refused("round cap %d reached", b.cap(maxRounds))
	}
	a := RepairAttempt{Round: b.Rounds() + 1, BaseSha: b.headSha, Budget: budget, Brief: brief, StartedAt: at}
	b.attempts = append(b.attempts, a)
	b.changes.Attempts = append(b.changes.Attempts, a)
	b.move(Repairing, CauseRepairStarted, at)
	return a, nil
}

// RecordRun marks the open round as having reached the agent. attemptID,
// when non-empty, must name the open round.
func (b *Bump) RecordRun(attemptID string, r resolution.Run) error {
	if r.RunID == "" || r.Model == "" || !shaRe.MatchString(r.BaseSha) {
		return resolution.Invalid("run needs run id, model and a 40-hex base sha")
	}
	a, err := b.target(attemptID)
	if err != nil {
		return err
	}
	if a.Run != nil {
		return resolution.Refused("run already recorded for round %d", a.Round)
	}
	a.Run = &r
	if b.changes.Runs == nil {
		b.changes.Runs = map[int]resolution.Run{}
	}
	b.changes.Runs[a.Round] = r
	return nil
}

// RecordOutcome ends the open round and moves the bump by outcome kind. The
// bump only moves when it is still repairing: a force-push or the pull
// request closing under a running round leaves the bump where that event put
// it, and the round still records how it ended.
func (b *Bump) RecordOutcome(attemptID string, o RepairOutcome, maxRounds int) error {
	if err := validateOutcome(o); err != nil {
		return err
	}
	a, err := b.target(attemptID)
	if err != nil {
		return err
	}
	if o.Kind == Pushed && o.HeadSha == a.BaseSha {
		return resolution.Invalid("a pushed outcome must advance the head; %s is the base", o.HeadSha)
	}
	// Decide the whole transition before touching anything, so an outcome the
	// bump will not take leaves the aggregate exactly as it found it rather
	// than shipping an outcome to the store on the next persist.
	moves := b.state == Repairing
	var to BumpState
	var cause BumpTransitionCause
	var reason AbandonReason
	if o.Kind == RepairAborted && o.Reason == AbortPullRequestClosed && b.state != Closed {
		return resolution.Refused("abort reason %s in state %s", o.Reason, b.state)
	}
	if moves {
		to, cause, reason = b.outcomeMove(a, o, maxRounds)
	}
	a.Outcome = &o
	if b.changes.Outcomes == nil {
		b.changes.Outcomes = map[int]RepairOutcome{}
	}
	b.changes.Outcomes[a.Round] = o
	if o.Kind == Pushed {
		b.headSha = o.HeadSha
		b.changes.HeadSha = o.HeadSha
	}
	if moves {
		if reason != "" {
			b.setAbandonment(reason, o.Summary, o.EndedAt)
		}
		b.move(to, cause, o.EndedAt)
	}
	return nil
}

// outcomeMove is the transition table for a round ending on a repairing
// bump: where it goes, what the log says, and what abandonment it records.
func (b *Bump) outcomeMove(a *RepairAttempt, o RepairOutcome, maxRounds int) (BumpState, BumpTransitionCause, AbandonReason) {
	// A requeue the cap would never let run would leave the bump queued for
	// ever, so it abandons instead.
	requeue := func(cause BumpTransitionCause) (BumpState, BumpTransitionCause, AbandonReason) {
		if b.Rounds() < b.cap(maxRounds) {
			return Queued, cause, ""
		}
		return Abandoned, CauseRepairRequeueCapped, AbandonRoundsExhausted
	}
	switch o.Kind {
	case Pushed:
		return AwaitingChecks, CauseRepairPushed, ""
	case NoChange:
		return Abandoned, CauseRepairNoChange, AbandonNoChange
	case BudgetExhausted:
		return Abandoned, CauseRepairBudgetExhausted, AbandonBudgetExhausted
	case RepairFailed:
		if o.Class == resolution.FailureInfra {
			return requeue(CauseRepairFailedInfraRequeued)
		}
		return Abandoned, CauseRepairFailedAbandoned, AbandonRepairFailed
	}
	switch o.Reason {
	case AbortOperatorStop:
		return Abandoned, CauseRepairAbortedOperatorStop, AbandonOperatorStop
	case AbortDaemonRestart:
		if b.restartedBefore(a.Round) {
			return Abandoned, CauseRepairAbortedRestartAbandon, AbandonRestartFailed
		}
		return requeue(CauseRepairAbortedRestartRequeue)
	}
	// pull_request_closed, which RecordOutcome only admits when the bump is
	// already closed, and a closed bump does not move.
	return Closed, CauseBumpClosed, ""
}

// validateOutcome checks the common fields and the variant ones the kind
// says are meaningful.
func validateOutcome(o RepairOutcome) error {
	if _, err := ParseRepairOutcomeKind(string(o.Kind)); err != nil {
		return err
	}
	if o.Summary == "" {
		return resolution.Invalid("outcome summary is empty")
	}
	switch o.Kind {
	case Pushed:
		if !shaRe.MatchString(o.HeadSha) {
			return resolution.Invalid("pushed head sha %q", o.HeadSha)
		}
	case BudgetExhausted:
		if _, err := resolution.ParseLimit(string(o.Limit)); err != nil {
			return err
		}
	case RepairFailed:
		if _, err := resolution.ParseFailureClass(string(o.Class)); err != nil {
			return err
		}
		if o.Message == "" {
			return resolution.Invalid("failure message is empty")
		}
	case RepairAborted:
		if _, err := ParseRepairAbortReason(string(o.Reason)); err != nil {
			return err
		}
	}
	return nil
}

// Abandon records a reason no round produced. Only checks_never_concluded
// arrives this way; every other reason is recorded by the verdict or the
// outcome that caused it, and accepting one here would let a caller write a
// reason the facts do not support.
func (b *Bump) Abandon(a BumpAbandonment) error {
	if _, err := ParseAbandonReason(string(a.Reason)); err != nil {
		return err
	}
	if a.Detail == "" {
		return resolution.Invalid("abandonment detail is empty")
	}
	if a.Reason != AbandonChecksNeverConcluded {
		return resolution.Refused("reason %s is recorded by the outcome that caused it", a.Reason)
	}
	if b.state == Closed || b.state == Abandoned {
		return resolution.Refused("abandon in state %s", b.state)
	}
	b.setAbandonment(a.Reason, a.Detail, a.AbandonedAt)
	b.move(Abandoned, CauseChecksNeverConcluded, a.AbandonedAt)
	return nil
}

// Retry is the one thing that undoes an abandonment, and only an operator
// asks for it. It grants one further round, which cap reads back out of the
// transition log.
//
// Where it lands depends on what the current head's checks actually said. A
// bump abandoned because they never concluded has no verdict to repair from,
// so it goes back to waiting rather than to the queue: queued with no
// verdict is a bump the scheduler asks GitHub about on every sweep for ever
// and can never start, because the brief has no failure to describe. A bump
// with a failing verdict goes to the queue for another round. A green bump
// is refused outright: there is nothing to fix, and a round started on one
// would push to somebody else's branch over a break that does not exist.
func (b *Bump) Retry(at time.Time) error {
	if b.state != Abandoned && b.state != Green {
		return resolution.Refused("retry in state %s", b.state)
	}
	to := AwaitingChecks
	if v := b.CurrentVerdict(); v != nil {
		if v.Conclusion == CheckSuccess {
			return resolution.Refused("the checks on %s passed; there is nothing to repair", b.headSha)
		}
		to = Queued
	}
	if b.abandonment != nil {
		b.abandonment = nil
		b.changes.Abandonment = nil
		b.changes.DropAbandon = true
	}
	b.move(to, CauseOperatorRetry, at)
	return nil
}

// Close records the pull request closing on GitHub. Any non-terminal state
// goes to Closed; a running round is aborted by the application afterwards.
func (b *Bump) Close(cl BumpClosure) error {
	if _, err := ParseClosureKind(string(cl.Kind)); err != nil {
		return err
	}
	if cl.DeliveryID == "" {
		return resolution.Invalid("closure delivery id is empty")
	}
	if b.state.Terminal() {
		return resolution.Refused("close in state %s", b.state)
	}
	b.closure = &cl
	b.changes.Closure = &cl
	b.move(Closed, CauseBumpClosed, cl.ClosedAt)
	return nil
}

// setAbandonment records why autophage stopped. Callers have already decided
// the transition.
func (b *Bump) setAbandonment(reason AbandonReason, detail string, at time.Time) {
	if detail == "" {
		detail = string(reason)
	}
	a := BumpAbandonment{Reason: reason, Detail: detail, AbandonedAt: at}
	b.abandonment = &a
	b.changes.Abandonment = &a
	b.changes.DropAbandon = false
}

// target returns the open round, checking attemptID against it when both are
// known.
func (b *Bump) target(attemptID string) (*RepairAttempt, error) {
	a := b.openRepair()
	if a == nil {
		return nil, resolution.Refused("no open round")
	}
	if attemptID != "" && a.ID != "" && a.ID != attemptID {
		return nil, resolution.Refused("round %s is not the open one", attemptID)
	}
	return a, nil
}

// restartedBefore reports whether a round before this one ended with a
// daemon-restart abort, which makes this restart the second.
func (b *Bump) restartedBefore(round int) bool {
	for _, a := range b.attempts {
		if a.Round < round && a.Outcome != nil && a.Outcome.Kind == RepairAborted && a.Outcome.Reason == AbortDaemonRestart {
			return true
		}
	}
	return false
}

// Snapshot is the store's view of a persisted Bump, for LoadBump.
type Snapshot struct {
	ID          string
	Repository  string
	Number      int
	Branch      string
	BaseBranch  string
	HeadSha     string
	State       BumpState
	OpenedAt    time.Time
	Verdicts    []CheckVerdict
	Attempts    []RepairAttempt
	Transitions []BumpTransition
	Abandonment *BumpAbandonment
	Closure     *BumpClosure
}

// LoadBump rebuilds a Bump from the store and re-checks the invariants a row
// could have lost: at most one open round, at most one verdict per head, and
// a state whose owning fact row is actually there.
func LoadBump(s Snapshot) (*Bump, error) {
	if s.Repository == "" || s.Number < 1 || s.Branch == "" || s.BaseBranch == "" {
		return nil, resolution.Invalid("snapshot incomplete")
	}
	if !shaRe.MatchString(s.HeadSha) {
		return nil, resolution.Invalid("head sha %q", s.HeadSha)
	}
	if _, err := ParseBumpState(string(s.State)); err != nil {
		return nil, err
	}
	open := 0
	for _, a := range s.Attempts {
		if a.Open() {
			open++
		}
	}
	if open > 1 {
		return nil, resolution.Invalid("%d open rounds", open)
	}
	seen := map[string]bool{}
	for _, v := range s.Verdicts {
		if seen[v.HeadSha] {
			return nil, resolution.Invalid("head %s has two verdicts", v.HeadSha)
		}
		seen[v.HeadSha] = true
	}
	if s.State == Abandoned && s.Abandonment == nil {
		return nil, resolution.Invalid("state abandoned with no abandonment")
	}
	if s.State == Closed && s.Closure == nil {
		return nil, resolution.Invalid("state closed with no closure")
	}
	b := &Bump{
		id: s.ID, repository: s.Repository, number: s.Number, branch: s.Branch,
		baseBranch: s.BaseBranch, headSha: s.HeadSha, state: s.State, openedAt: s.OpenedAt,
		verdicts:    append([]CheckVerdict(nil), s.Verdicts...),
		attempts:    append([]RepairAttempt(nil), s.Attempts...),
		transitions: append([]BumpTransition(nil), s.Transitions...),
		abandonment: s.Abandonment, closure: s.Closure,
	}
	b.ClearChanges()
	return b, nil
}
