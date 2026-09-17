package upkeep

import (
	"errors"
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
)

const maxRounds = 3

var (
	sha2 = "2222222222222222222222222222222222222222"
	sha3 = "3333333333333333333333333333333333333333"
)

func at(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

func testBudget(t *testing.T) resolution.Budget {
	t.Helper()
	b, err := resolution.NewBudget(20, 30*time.Minute, 400)
	if err != nil {
		t.Fatalf("NewBudget: %v", err)
	}
	return b
}

func failure(sha string, n int) CheckVerdict {
	return CheckVerdict{HeadSha: sha, Conclusion: CheckFailure, FailingContexts: "test (1.26)", DetailsURL: "https://x", ConcludedAt: at(n)}
}

func success(sha string, n int) CheckVerdict {
	return CheckVerdict{HeadSha: sha, Conclusion: CheckSuccess, ConcludedAt: at(n)}
}

// red drives a fresh bump to queued by recording a failing verdict.
func red(t *testing.T) *Bump {
	t.Helper()
	b := newTestBump(t)
	if err := b.RecordVerdict(failure(sha1, 1), maxRounds); err != nil {
		t.Fatalf("RecordVerdict: %v", err)
	}
	return b
}

// repairing drives a fresh bump to an open round.
func repairing(t *testing.T) *Bump {
	t.Helper()
	b := red(t)
	if _, err := b.StartRepair(testBudget(t), "fix it", at(2), maxRounds); err != nil {
		t.Fatalf("StartRepair: %v", err)
	}
	return b
}

func wantState(t *testing.T, b *Bump, want BumpState) {
	t.Helper()
	if got := b.State(); got != want {
		t.Errorf("state = %s, want %s", got, want)
	}
}

func lastCause(t *testing.T, b *Bump) BumpTransitionCause {
	t.Helper()
	tr := b.Transitions()
	if len(tr) == 0 {
		t.Fatal("no transitions recorded")
	}
	return tr[len(tr)-1].Cause
}

func TestVerdictSuccessGoesGreen(t *testing.T) {
	b := newTestBump(t)
	if err := b.RecordVerdict(success(sha1, 1), maxRounds); err != nil {
		t.Fatalf("RecordVerdict: %v", err)
	}
	wantState(t, b, Green)
	if got := lastCause(t, b); got != CauseVerdictSuccess {
		t.Errorf("cause = %s, want %s", got, CauseVerdictSuccess)
	}
}

func TestVerdictFailureQueues(t *testing.T) {
	b := red(t)
	wantState(t, b, Queued)
	if got := lastCause(t, b); got != CauseVerdictFailure {
		t.Errorf("cause = %s, want %s", got, CauseVerdictFailure)
	}
}

// The loop guard. A verdict for a head a repair or a force-push already
// replaced must not move the bump.
func TestVerdictForStaleHeadIsRefused(t *testing.T) {
	b := newTestBump(t)
	if err := b.RecordVerdict(failure(sha2, 1), maxRounds); !errors.Is(err, resolution.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	wantState(t, b, AwaitingChecks)
	if len(b.Changes().Verdicts) != 0 {
		t.Error("a refused verdict reached the store changes")
	}
}

func TestVerdictTwiceForOneHeadIsRefused(t *testing.T) {
	b := newTestBump(t)
	if err := b.RecordVerdict(failure(sha1, 1), maxRounds); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := b.RecordVerdict(failure(sha1, 2), maxRounds); !errors.Is(err, resolution.ErrRefused) {
		t.Errorf("second: err = %v, want ErrRefused", err)
	}
}

// CI finishing on a head a round is already working is ordinary, not an
// error the ledger should call a rejection, but the aggregate still refuses.
func TestVerdictWhileRepairingIsRefused(t *testing.T) {
	b := repairing(t)
	if err := b.RecordVerdict(failure(sha1, 3), maxRounds); !errors.Is(err, resolution.ErrRefused) {
		t.Errorf("err = %v, want ErrRefused", err)
	}
}

func TestVerdictFailingContextsMustMatchConclusion(t *testing.T) {
	b := newTestBump(t)
	v := success(sha1, 1)
	v.FailingContexts = "test (1.26)"
	if err := b.RecordVerdict(v, maxRounds); !errors.Is(err, resolution.ErrInvalid) {
		t.Errorf("success with failing contexts: err = %v, want ErrInvalid", err)
	}
	v = failure(sha1, 1)
	v.FailingContexts = ""
	if err := b.RecordVerdict(v, maxRounds); !errors.Is(err, resolution.ErrInvalid) {
		t.Errorf("failure without failing contexts: err = %v, want ErrInvalid", err)
	}
}

// The second loop guard: the cap bounds the cycle even when every verdict is
// legitimately for the current head.
func TestVerdictFailureAtCapAbandons(t *testing.T) {
	b := newTestBump(t)
	head := sha1
	for r := range maxRounds {
		if err := b.RecordVerdict(failure(head, 2*r+1), maxRounds); err != nil {
			t.Fatalf("round %d verdict: %v", r+1, err)
		}
		if _, err := b.StartRepair(testBudget(t), "fix it", at(2*r+2), maxRounds); err != nil {
			t.Fatalf("round %d start: %v", r+1, err)
		}
		head = pushSha(r)
		if err := b.RecordOutcome("", RepairOutcome{Kind: Pushed, HeadSha: head, Summary: "pushed", EndedAt: at(2*r + 3)}, maxRounds); err != nil {
			t.Fatalf("round %d outcome: %v", r+1, err)
		}
	}
	if got := b.Rounds(); got != maxRounds {
		t.Fatalf("rounds = %d, want %d", got, maxRounds)
	}
	if err := b.RecordVerdict(failure(head, 99), maxRounds); err != nil {
		t.Fatalf("final verdict: %v", err)
	}
	wantState(t, b, Abandoned)
	if got := lastCause(t, b); got != CauseVerdictFailureCapReached {
		t.Errorf("cause = %s, want %s", got, CauseVerdictFailureCapReached)
	}
	ab := b.Abandonment()
	if ab == nil || ab.Reason != AbandonRoundsExhausted {
		t.Errorf("abandonment = %+v, want rounds_exhausted", ab)
	}
}

func pushSha(round int) string {
	return string([]byte{byte('a' + round)}) + "000000000000000000000000000000000000000"
}

func TestStartRepairOnlyFromQueued(t *testing.T) {
	b := newTestBump(t)
	if _, err := b.StartRepair(testBudget(t), "fix it", at(2), maxRounds); !errors.Is(err, resolution.ErrRefused) {
		t.Errorf("from awaiting_checks: err = %v, want ErrRefused", err)
	}
}

func TestStartRepairRecordsBaseShaAndRound(t *testing.T) {
	b := repairing(t)
	wantState(t, b, Repairing)
	a := b.OpenRepair()
	if a == nil {
		t.Fatal("no open round")
	}
	if a.Round != 1 {
		t.Errorf("round = %d, want 1", a.Round)
	}
	if a.BaseSha != sha1 {
		t.Errorf("base sha = %s, want %s", a.BaseSha, sha1)
	}
	if got := lastCause(t, b); got != CauseRepairStarted {
		t.Errorf("cause = %s, want %s", got, CauseRepairStarted)
	}
}

func TestPushedOutcomeAdvancesHeadAndWaits(t *testing.T) {
	b := repairing(t)
	err := b.RecordOutcome("", RepairOutcome{Kind: Pushed, HeadSha: sha2, Summary: "bumped the pin", EndedAt: at(3)}, maxRounds)
	if err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	wantState(t, b, AwaitingChecks)
	if got := b.HeadSha(); got != sha2 {
		t.Errorf("head sha = %s, want %s", got, sha2)
	}
	if got := lastCause(t, b); got != CauseRepairPushed {
		t.Errorf("cause = %s, want %s", got, CauseRepairPushed)
	}
}

// A push whose head equals the base would leave the bump waiting on a sha
// that already has a verdict, so nothing could ever move it again.
func TestPushedOutcomeMustAdvanceTheSha(t *testing.T) {
	b := repairing(t)
	err := b.RecordOutcome("", RepairOutcome{Kind: Pushed, HeadSha: sha1, Summary: "pushed", EndedAt: at(3)}, maxRounds)
	if !errors.Is(err, resolution.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestOutcomeTransitions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		out    RepairOutcome
		state  BumpState
		cause  BumpTransitionCause
		reason AbandonReason // empty means no abandonment expected
	}{
		{"no change abandons", RepairOutcome{Kind: NoChange, Summary: "nothing to change"}, Abandoned, CauseRepairNoChange, AbandonNoChange},
		{"budget exhausted abandons", RepairOutcome{Kind: BudgetExhausted, Limit: resolution.LimitTurns, Summary: "out of turns"}, Abandoned, CauseRepairBudgetExhausted, AbandonBudgetExhausted},
		{"infra failure requeues", RepairOutcome{Kind: RepairFailed, Class: resolution.FailureInfra, Message: "podman busy", Summary: "podman busy"}, Queued, CauseRepairFailedInfraRequeued, ""},
		{"model failure abandons", RepairOutcome{Kind: RepairFailed, Class: resolution.FailureModel, Message: "provider 500", Summary: "provider 500"}, Abandoned, CauseRepairFailedAbandoned, AbandonRepairFailed},
		{"agent failure abandons", RepairOutcome{Kind: RepairFailed, Class: resolution.FailureAgent, Message: "conflict markers", Summary: "conflict markers"}, Abandoned, CauseRepairFailedAbandoned, AbandonRepairFailed},
		{"operator stop abandons", RepairOutcome{Kind: RepairAborted, Reason: AbortOperatorStop, Summary: "stopped"}, Abandoned, CauseRepairAbortedOperatorStop, AbandonOperatorStop},
		{"first restart requeues", RepairOutcome{Kind: RepairAborted, Reason: AbortDaemonRestart, Summary: "restarted"}, Queued, CauseRepairAbortedRestartRequeue, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := repairing(t)
			tc.out.EndedAt = at(3)
			if err := b.RecordOutcome("", tc.out, maxRounds); err != nil {
				t.Fatalf("RecordOutcome: %v", err)
			}
			wantState(t, b, tc.state)
			if got := lastCause(t, b); got != tc.cause {
				t.Errorf("cause = %s, want %s", got, tc.cause)
			}
			ab := b.Abandonment()
			if tc.reason == "" {
				if ab != nil {
					t.Errorf("abandonment = %+v, want none", ab)
				}
				return
			}
			if ab == nil || ab.Reason != tc.reason {
				t.Errorf("abandonment = %+v, want %s", ab, tc.reason)
			}
			if ab != nil && ab.Detail == "" {
				t.Error("abandonment detail is empty")
			}
		})
	}
}

func TestSecondRestartAbandons(t *testing.T) {
	b := repairing(t)
	if err := b.RecordOutcome("", RepairOutcome{Kind: RepairAborted, Reason: AbortDaemonRestart, Summary: "restarted", EndedAt: at(3)}, maxRounds); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := b.StartRepair(testBudget(t), "fix it", at(4), maxRounds); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if err := b.RecordOutcome("", RepairOutcome{Kind: RepairAborted, Reason: AbortDaemonRestart, Summary: "restarted", EndedAt: at(5)}, maxRounds); err != nil {
		t.Fatalf("second: %v", err)
	}
	wantState(t, b, Abandoned)
	if got := lastCause(t, b); got != CauseRepairAbortedRestartAbandon {
		t.Errorf("cause = %s, want %s", got, CauseRepairAbortedRestartAbandon)
	}
	if ab := b.Abandonment(); ab == nil || ab.Reason != AbandonRestartFailed {
		t.Errorf("abandonment = %+v, want restart_failed", ab)
	}
}

// A requeue that the cap will never let run would leave the bump queued for
// ever, so the cap is checked on the requeue too.
func TestInfraFailureAtCapAbandonsInsteadOfRequeuing(t *testing.T) {
	b := repairing(t)
	if err := b.RecordOutcome("", RepairOutcome{Kind: RepairFailed, Class: resolution.FailureInfra, Message: "podman busy", Summary: "podman busy", EndedAt: at(3)}, 1); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	wantState(t, b, Abandoned)
	if ab := b.Abandonment(); ab == nil || ab.Reason != AbandonRoundsExhausted {
		t.Errorf("abandonment = %+v, want rounds_exhausted", ab)
	}
}

func TestStartRepairRefusedAtCap(t *testing.T) {
	b := red(t)
	if _, err := b.StartRepair(testBudget(t), "fix it", at(2), 0); !errors.Is(err, resolution.ErrRefused) {
		t.Errorf("err = %v, want ErrRefused", err)
	}
}

func TestAdvanceHeadFromAwaitingChecksLogsNoTransition(t *testing.T) {
	b := newTestBump(t)
	before := len(b.Transitions())
	if err := b.AdvanceHead(sha2, at(1)); err != nil {
		t.Fatalf("AdvanceHead: %v", err)
	}
	wantState(t, b, AwaitingChecks)
	if got := b.HeadSha(); got != sha2 {
		t.Errorf("head sha = %s, want %s", got, sha2)
	}
	if got := len(b.Transitions()); got != before {
		t.Errorf("transitions = %d, want %d: the state was already correct", got, before)
	}
}

func TestAdvanceHeadFromGreenReopensTheWait(t *testing.T) {
	b := newTestBump(t)
	if err := b.RecordVerdict(success(sha1, 1), maxRounds); err != nil {
		t.Fatalf("RecordVerdict: %v", err)
	}
	if err := b.AdvanceHead(sha2, at(2)); err != nil {
		t.Fatalf("AdvanceHead: %v", err)
	}
	wantState(t, b, AwaitingChecks)
	if got := lastCause(t, b); got != CauseHeadAdvanced {
		t.Errorf("cause = %s, want %s", got, CauseHeadAdvanced)
	}
}

// Abandonment is sticky: a rebase of a version autophage already failed to
// repair is not new information.
func TestAdvanceHeadDoesNotReviveAnAbandonedBump(t *testing.T) {
	b := repairing(t)
	if err := b.RecordOutcome("", RepairOutcome{Kind: NoChange, Summary: "nothing to change", EndedAt: at(3)}, maxRounds); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	if err := b.AdvanceHead(sha3, at(4)); !errors.Is(err, resolution.ErrRefused) {
		t.Errorf("err = %v, want ErrRefused", err)
	}
	wantState(t, b, Abandoned)
}

func TestRetryIsTheOnlyThingThatUndoesAbandonment(t *testing.T) {
	b := repairing(t)
	if err := b.RecordOutcome("", RepairOutcome{Kind: NoChange, Summary: "nothing to change", EndedAt: at(3)}, maxRounds); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	if err := b.Retry(at(4)); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	wantState(t, b, Queued)
	if got := lastCause(t, b); got != CauseOperatorRetry {
		t.Errorf("cause = %s, want %s", got, CauseOperatorRetry)
	}
	if b.Abandonment() != nil {
		t.Error("abandonment survived the retry")
	}
	if !b.Changes().DropAbandon {
		t.Error("changes do not tell the store to delete the abandonment row")
	}
}

// Retry grants one further round, derived from the transition log rather
// than stored, so a bump at its cap can actually run again.
func TestRetryGrantsOneFurtherRound(t *testing.T) {
	b := repairing(t)
	if err := b.RecordOutcome("", RepairOutcome{Kind: NoChange, Summary: "nothing", EndedAt: at(3)}, 1); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	if err := b.Retry(at(4)); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if _, err := b.StartRepair(testBudget(t), "fix it", at(5), 1); err != nil {
		t.Errorf("StartRepair after retry: %v, want nil", err)
	}
}

// Retrying a bump whose checks have not concluded would repair without
// knowing what is broken.
func TestRetryRefusedWhileChecksRun(t *testing.T) {
	b := newTestBump(t)
	if err := b.Retry(at(1)); !errors.Is(err, resolution.ErrRefused) {
		t.Errorf("err = %v, want ErrRefused", err)
	}
}

func TestRetryRefusedWhileQueuedOrRepairing(t *testing.T) {
	if err := red(t).Retry(at(5)); !errors.Is(err, resolution.ErrRefused) {
		t.Errorf("queued: err = %v, want ErrRefused", err)
	}
	if err := repairing(t).Retry(at(5)); !errors.Is(err, resolution.ErrRefused) {
		t.Errorf("repairing: err = %v, want ErrRefused", err)
	}
}

func TestCloseFromAnyOpenState(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    func(*testing.T) *Bump
	}{
		{"awaiting_checks", newTestBump},
		{"queued", red},
		{"repairing", repairing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.b(t)
			if err := b.Close(BumpClosure{Kind: Merged, DeliveryID: "d1", ClosedAt: at(9)}); err != nil {
				t.Fatalf("Close: %v", err)
			}
			wantState(t, b, Closed)
			if got := lastCause(t, b); got != CauseBumpClosed {
				t.Errorf("cause = %s, want %s", got, CauseBumpClosed)
			}
		})
	}
}

func TestCloseTwiceIsRefused(t *testing.T) {
	b := newTestBump(t)
	cl := BumpClosure{Kind: Discarded, DeliveryID: "d1", ClosedAt: at(9)}
	if err := b.Close(cl); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := b.Close(cl); !errors.Is(err, resolution.ErrRefused) {
		t.Errorf("err = %v, want ErrRefused", err)
	}
}

// A round running when the pull request closes still records its outcome;
// the bump is already closed and does not move again.
func TestAbortedOnCloseRecordsOutcomeWithoutMoving(t *testing.T) {
	b := repairing(t)
	if err := b.Close(BumpClosure{Kind: Merged, DeliveryID: "d1", ClosedAt: at(4)}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	before := len(b.Transitions())
	err := b.RecordOutcome("", RepairOutcome{Kind: RepairAborted, Reason: AbortPullRequestClosed, Summary: "pull request closed", EndedAt: at(5)}, maxRounds)
	if err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	wantState(t, b, Closed)
	if got := len(b.Transitions()); got != before {
		t.Errorf("transitions = %d, want %d", got, before)
	}
	if b.OpenRepair() != nil {
		t.Error("round is still open")
	}
}

// pull_request_closed outside Closed would be the runner lying about why it
// stopped.
func TestAbortPullRequestClosedOutsideClosedIsRefused(t *testing.T) {
	b := repairing(t)
	err := b.RecordOutcome("", RepairOutcome{Kind: RepairAborted, Reason: AbortPullRequestClosed, Summary: "pull request closed", EndedAt: at(5)}, maxRounds)
	if !errors.Is(err, resolution.ErrRefused) {
		t.Errorf("err = %v, want ErrRefused", err)
	}
}

func TestAbandonRecordsEvidence(t *testing.T) {
	b := newTestBump(t)
	err := b.Abandon(BumpAbandonment{Reason: AbandonChecksNeverConcluded, Detail: "no conclusive rollup in 6h", AbandonedAt: at(360)})
	if err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	wantState(t, b, Abandoned)
	if got := lastCause(t, b); got != CauseChecksNeverConcluded {
		t.Errorf("cause = %s, want %s", got, CauseChecksNeverConcluded)
	}
}

func TestAbandonRequiresDetail(t *testing.T) {
	b := newTestBump(t)
	err := b.Abandon(BumpAbandonment{Reason: AbandonChecksNeverConcluded, AbandonedAt: at(360)})
	if !errors.Is(err, resolution.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestOnlyOneOpenRoundAtATime(t *testing.T) {
	b := repairing(t)
	if _, err := b.StartRepair(testBudget(t), "again", at(4), maxRounds); !errors.Is(err, resolution.ErrRefused) {
		t.Errorf("err = %v, want ErrRefused", err)
	}
}

func TestRecordRunOnceOnly(t *testing.T) {
	b := repairing(t)
	run := resolution.Run{RunID: "r1", Model: "m", BaseSha: sha1, BeganAt: at(3)}
	if err := b.RecordRun("", run); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := b.RecordRun("", run); !errors.Is(err, resolution.ErrRefused) {
		t.Errorf("second: err = %v, want ErrRefused", err)
	}
}

func TestRecordRunNeedsAnOpenRound(t *testing.T) {
	b := red(t)
	err := b.RecordRun("", resolution.Run{RunID: "r1", Model: "m", BaseSha: sha1, BeganAt: at(3)})
	if !errors.Is(err, resolution.ErrRefused) {
		t.Errorf("err = %v, want ErrRefused", err)
	}
}

// Accessors hand out copies: mutating what a caller got must not reach the
// aggregate, or every invariant is bypassable.
func TestAccessorsCopy(t *testing.T) {
	b := repairing(t)
	if a := b.OpenRepair(); a != nil {
		a.BaseSha = sha3
	}
	if got := b.OpenRepair().BaseSha; got != sha1 {
		t.Errorf("base sha = %s, want %s: OpenRepair handed out the real attempt", got, sha1)
	}
	vs := b.Verdicts()
	if len(vs) > 0 {
		vs[0].Conclusion = CheckSuccess
	}
	if got := b.Verdicts()[0].Conclusion; got != CheckFailure {
		t.Errorf("conclusion = %s, want failure: Verdicts handed out the real slice", got)
	}
}

// A bump abandoned because its checks never concluded has no verdict at all,
// so there is nothing to repair. Sending it to queued would make the
// scheduler ask GitHub about it on every sweep for ever and start nothing,
// because the brief has no failure to describe. It goes back to waiting.
func TestRetryOfAnUnconcludedBumpRestartsTheWait(t *testing.T) {
	b := newTestBump(t)
	err := b.Abandon(BumpAbandonment{Reason: AbandonChecksNeverConcluded, Detail: "no rollup in 6h", AbandonedAt: at(360)})
	if err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	if err := b.Retry(at(400)); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	wantState(t, b, AwaitingChecks)
	if got := lastCause(t, b); got != CauseOperatorRetry {
		t.Errorf("cause = %s, want %s", got, CauseOperatorRetry)
	}
	if b.Abandonment() != nil {
		t.Error("abandonment survived the retry")
	}
}

// Green has a success verdict and nothing to fix. Retrying it would send an
// agent to push to somebody else's branch over a break that does not exist.
func TestRetryFromGreenIsRefused(t *testing.T) {
	b := newTestBump(t)
	if err := b.RecordVerdict(success(sha1, 1), maxRounds); err != nil {
		t.Fatalf("RecordVerdict: %v", err)
	}
	if err := b.Retry(at(2)); !errors.Is(err, resolution.ErrRefused) {
		t.Errorf("err = %v, want ErrRefused", err)
	}
	wantState(t, b, Green)
}
