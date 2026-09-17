package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
	"github.com/guygrigsby/autophage/internal/storetest"
	"github.com/guygrigsby/autophage/internal/upkeep"
)

const (
	bumpSha1 = "1111111111111111111111111111111111111111"
	bumpSha2 = "2222222222222222222222222222222222222222"
	bumpCap  = 3
)

func bumpBudget(t *testing.T) resolution.Budget {
	t.Helper()
	b, err := resolution.NewBudget(20, 30*time.Minute, 400)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// watched enrolls a repository and watches it, which is what a bump needs.
func watched(t *testing.T, s *store.Store, name string) {
	t.Helper()
	seedRepo(t, s, name)
	w, err := upkeep.NewWatch(name, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WatchRepository(t.Context(), w); err != nil {
		t.Fatal(err)
	}
}

func createBump(t *testing.T, s *store.Store, repo string, number int) *upkeep.Bump {
	t.Helper()
	b, err := upkeep.NewBump(repo, number, "dependabot/go_modules/y-1.2.3", "main", bumpSha1, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateBump(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCreateBumpAssignsAnID(t *testing.T) {
	s := storetest.Open(t)
	watched(t, s, "guy/repo")
	b := createBump(t, s, "guy/repo", 7)
	if b.ID() == "" {
		t.Error("no id assigned")
	}
	if ch := b.Changes(); len(ch.Transitions) != 0 {
		t.Error("changes survived the create")
	}
}

func TestCreateBumpTwiceConflicts(t *testing.T) {
	s := storetest.Open(t)
	watched(t, s, "guy/repo")
	createBump(t, s, "guy/repo", 7)
	b2, err := upkeep.NewBump("guy/repo", 7, "dependabot/other", "main", bumpSha2, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateBump(t.Context(), b2); !errors.Is(err, store.ErrConflict) {
		t.Errorf("err = %v, want ErrConflict", err)
	}
}

// Enrollment authorises issues; a watch authorises bumps. Without one the
// store refuses, so the rule holds even if a caller forgets to check.
func TestCreateBumpRefusesAnUnwatchedRepository(t *testing.T) {
	s := storetest.Open(t)
	seedRepo(t, s, "guy/repo")
	b, err := upkeep.NewBump("guy/repo", 7, "dependabot/x", "main", bumpSha1, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateBump(t.Context(), b); err == nil {
		t.Error("the store created a bump on an unwatched repository")
	}
}

// Unwatching must not delete the bumps already open, or an operator turning
// a repository off loses the history of what autophage did to it.
func TestUnwatchKeepsExistingBumps(t *testing.T) {
	s := storetest.Open(t)
	watched(t, s, "guy/repo")
	createBump(t, s, "guy/repo", 7)
	if err := s.UnwatchRepository(t.Context(), "guy/repo"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBump(t.Context(), "guy/repo", 7); err != nil {
		t.Errorf("GetBump after unwatch: %v", err)
	}
	ws, err := s.WatchedRepositories(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 0 {
		t.Errorf("watches = %v, want none", ws)
	}
}

func TestUpdateBumpPersistsEveryFact(t *testing.T) {
	s := storetest.Open(t)
	watched(t, s, "guy/repo")
	createBump(t, s, "guy/repo", 7)
	ctx := t.Context()

	v := upkeep.CheckVerdict{HeadSha: bumpSha1, Conclusion: upkeep.CheckFailure, FailingContexts: "test (1.26)", DetailsURL: "https://x", ConcludedAt: t0.Add(time.Minute)}
	if _, err := s.UpdateBump(ctx, "guy/repo", 7, func(b *upkeep.Bump) error { return b.RecordVerdict(v, bumpCap) }); err != nil {
		t.Fatalf("verdict: %v", err)
	}
	if _, err := s.UpdateBump(ctx, "guy/repo", 7, func(b *upkeep.Bump) error {
		_, err := b.StartRepair(bumpBudget(t), "fix the bump", t0.Add(2*time.Minute), bumpCap)
		return err
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	run := resolution.Run{RunID: "run-1", Model: "m", BaseSha: bumpSha1, BeganAt: t0.Add(3 * time.Minute)}
	if _, err := s.UpdateBump(ctx, "guy/repo", 7, func(b *upkeep.Bump) error { return b.RecordRun("", run) }); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := upkeep.RepairOutcome{Kind: upkeep.Pushed, HeadSha: bumpSha2, Summary: "raised the pin", EndedAt: t0.Add(4 * time.Minute), Usage: resolution.Usage{Turns: 6, InputTokens: 900, OutputTokens: 120, WallClock: time.Minute, DiffLines: 3}}
	if _, err := s.UpdateBump(ctx, "guy/repo", 7, func(b *upkeep.Bump) error { return b.RecordOutcome("", out, bumpCap) }); err != nil {
		t.Fatalf("outcome: %v", err)
	}

	got, err := s.GetBump(ctx, "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if got.State() != upkeep.AwaitingChecks {
		t.Errorf("state = %s, want awaiting_checks", got.State())
	}
	if got.HeadSha() != bumpSha2 {
		t.Errorf("head sha = %s, want %s", got.HeadSha(), bumpSha2)
	}
	if n := len(got.Verdicts()); n != 1 {
		t.Fatalf("verdicts = %d, want 1", n)
	}
	if fc := got.Verdicts()[0].FailingContexts; fc != "test (1.26)" {
		t.Errorf("failing contexts = %q", fc)
	}
	rounds := got.Attempts()
	if len(rounds) != 1 {
		t.Fatalf("rounds = %d, want 1", len(rounds))
	}
	a := rounds[0]
	if a.Round != 1 || a.BaseSha != bumpSha1 || a.Brief != "fix the bump" {
		t.Errorf("round = %+v", a)
	}
	if a.Budget.MaxTurns() != 20 || a.Budget.MaxWallClock() != 30*time.Minute || a.Budget.MaxDiffLines() != 400 {
		t.Errorf("budget = %+v", a.Budget)
	}
	if a.Run == nil || a.Run.RunID != "run-1" {
		t.Errorf("run = %+v", a.Run)
	}
	if a.Outcome == nil || a.Outcome.Kind != upkeep.Pushed || a.Outcome.HeadSha != bumpSha2 {
		t.Fatalf("outcome = %+v", a.Outcome)
	}
	if a.Outcome.Usage.Turns != 6 || a.Outcome.Usage.WallClock != time.Minute {
		t.Errorf("usage = %+v", a.Outcome.Usage)
	}
	if n := len(got.Transitions()); n != 3 {
		t.Errorf("transitions = %d, want 3", n)
	}
}

func TestUpdateBumpPersistsEachOutcomeVariant(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  upkeep.RepairOutcome
	}{
		{"no change", upkeep.RepairOutcome{Kind: upkeep.NoChange, Summary: "nothing to change"}},
		{"exhausted", upkeep.RepairOutcome{Kind: upkeep.BudgetExhausted, Limit: resolution.LimitTurns, Summary: "out of turns"}},
		{"failed", upkeep.RepairOutcome{Kind: upkeep.RepairFailed, Class: resolution.FailureAgent, Message: "conflict markers", Summary: "conflict markers"}},
		{"aborted", upkeep.RepairOutcome{Kind: upkeep.RepairAborted, Reason: upkeep.AbortOperatorStop, Summary: "stopped"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := storetest.Open(t)
			watched(t, s, "guy/repo")
			createBump(t, s, "guy/repo", 7)
			ctx := t.Context()
			v := upkeep.CheckVerdict{HeadSha: bumpSha1, Conclusion: upkeep.CheckFailure, FailingContexts: "test", ConcludedAt: t0.Add(time.Minute)}
			if _, err := s.UpdateBump(ctx, "guy/repo", 7, func(b *upkeep.Bump) error { return b.RecordVerdict(v, bumpCap) }); err != nil {
				t.Fatal(err)
			}
			if _, err := s.UpdateBump(ctx, "guy/repo", 7, func(b *upkeep.Bump) error {
				_, err := b.StartRepair(bumpBudget(t), "fix it", t0.Add(2*time.Minute), bumpCap)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			tc.out.EndedAt = t0.Add(3 * time.Minute)
			if _, err := s.UpdateBump(ctx, "guy/repo", 7, func(b *upkeep.Bump) error { return b.RecordOutcome("", tc.out, bumpCap) }); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetBump(ctx, "guy/repo", 7)
			if err != nil {
				t.Fatal(err)
			}
			o := got.Attempts()[0].Outcome
			if o == nil || o.Kind != tc.out.Kind {
				t.Fatalf("outcome = %+v, want kind %s", o, tc.out.Kind)
			}
			if o.Limit != tc.out.Limit || o.Class != tc.out.Class || o.Message != tc.out.Message || o.Reason != tc.out.Reason {
				t.Errorf("variant fields = %+v, want %+v", o, tc.out)
			}
		})
	}
}

func TestAbandonmentAndClosureRoundTrip(t *testing.T) {
	s := storetest.Open(t)
	watched(t, s, "guy/repo")
	createBump(t, s, "guy/repo", 7)
	ctx := t.Context()
	if err := s.StoreDeliveryForTest(ctx, "d-close"); err != nil {
		t.Fatal(err)
	}
	ab := upkeep.BumpAbandonment{Reason: upkeep.AbandonChecksNeverConcluded, Detail: "no conclusive rollup in 6h", AbandonedAt: t0.Add(6 * time.Hour)}
	if _, err := s.UpdateBump(ctx, "guy/repo", 7, func(b *upkeep.Bump) error { return b.Abandon(ab) }); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	got, err := s.GetBump(ctx, "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if a := got.Abandonment(); a == nil || a.Reason != upkeep.AbandonChecksNeverConcluded || a.Detail == "" {
		t.Fatalf("abandonment = %+v", a)
	}
	// The operator retry is the second place in the system that deletes a
	// fact row rather than appending one; the row must actually go.
	if _, err := s.UpdateBump(ctx, "guy/repo", 7, func(b *upkeep.Bump) error { return b.Retry(t0.Add(7 * time.Hour)) }); err != nil {
		t.Fatalf("retry: %v", err)
	}
	got, err = s.GetBump(ctx, "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if a := got.Abandonment(); a != nil {
		t.Errorf("abandonment = %+v, want none after retry", a)
	}
	if got.State() != upkeep.Queued {
		t.Errorf("state = %s, want queued", got.State())
	}
	cl := upkeep.BumpClosure{Kind: upkeep.Merged, DeliveryID: "d-close", ClosedAt: t0.Add(8 * time.Hour)}
	if _, err := s.UpdateBump(ctx, "guy/repo", 7, func(b *upkeep.Bump) error { return b.Close(cl) }); err != nil {
		t.Fatalf("close: %v", err)
	}
	got, err = s.GetBump(ctx, "guy/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if c := got.Closure(); c == nil || c.Kind != upkeep.Merged || c.DeliveryID != "d-close" {
		t.Errorf("closure = %+v", c)
	}
}

// The loop guard has to hold at the table, not only in memory: two verdicts
// for one head would leave the bump's state undecidable.
func TestOneVerdictPerHeadIsStructural(t *testing.T) {
	s := storetest.Open(t)
	watched(t, s, "guy/repo")
	b := createBump(t, s, "guy/repo", 7)
	ctx := t.Context()
	v := upkeep.CheckVerdict{HeadSha: bumpSha1, Conclusion: upkeep.CheckSuccess, ConcludedAt: t0.Add(time.Minute)}
	if _, err := s.UpdateBump(ctx, "guy/repo", 7, func(b *upkeep.Bump) error { return b.RecordVerdict(v, bumpCap) }); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertVerdictForTest(ctx, s, b.ID(), v); err == nil {
		t.Error("the table accepted a second verdict for one head")
	}
}

func TestQueuedBumpsAreFIFOByTheirQueuedTransition(t *testing.T) {
	s := storetest.Open(t)
	watched(t, s, "guy/repo")
	ctx := t.Context()
	for i, n := range []int{11, 12, 13} {
		createBump(t, s, "guy/repo", n)
		v := upkeep.CheckVerdict{HeadSha: bumpSha1, Conclusion: upkeep.CheckFailure, FailingContexts: "test", ConcludedAt: t0.Add(time.Duration(3-i) * time.Minute)}
		if _, err := s.UpdateBump(ctx, "guy/repo", n, func(b *upkeep.Bump) error { return b.RecordVerdict(v, bumpCap) }); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := s.QueuedBumps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{13, 12, 11} // queued at t0+1, t0+2, t0+3
	if len(keys) != len(want) {
		t.Fatalf("queued = %v, want %d", keys, len(want))
	}
	for i, k := range keys {
		if k.Number != want[i] {
			t.Errorf("queued[%d] = %d, want %d", i, k.Number, want[i])
		}
	}
}

// The sweeper needs when the current wait started, which is the latest
// transition into awaiting_checks, not when the bump opened.
func TestAwaitingChecksSinceUsesTheLatestWait(t *testing.T) {
	s := storetest.Open(t)
	watched(t, s, "guy/repo")
	ctx := t.Context()
	createBump(t, s, "guy/repo", 7)
	stale, err := s.BumpsAwaitingChecksSince(ctx, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Fatalf("stale = %v, want the freshly opened bump", stale)
	}
	// A push restarts the wait, so the same cutoff must no longer match.
	v := upkeep.CheckVerdict{HeadSha: bumpSha1, Conclusion: upkeep.CheckFailure, FailingContexts: "test", ConcludedAt: t0.Add(time.Minute)}
	if _, err := s.UpdateBump(ctx, "guy/repo", 7, func(b *upkeep.Bump) error { return b.RecordVerdict(v, bumpCap) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateBump(ctx, "guy/repo", 7, func(b *upkeep.Bump) error {
		_, err := b.StartRepair(bumpBudget(t), "fix it", t0.Add(2*time.Minute), bumpCap)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	out := upkeep.RepairOutcome{Kind: upkeep.Pushed, HeadSha: bumpSha2, Summary: "pushed", EndedAt: t0.Add(3 * time.Hour)}
	if _, err := s.UpdateBump(ctx, "guy/repo", 7, func(b *upkeep.Bump) error { return b.RecordOutcome("", out, bumpCap) }); err != nil {
		t.Fatal(err)
	}
	stale, err = s.BumpsAwaitingChecksSince(ctx, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Errorf("stale = %v, want none: the push restarted the wait", stale)
	}
}

func TestGetBumpNotFound(t *testing.T) {
	s := storetest.Open(t)
	if _, err := s.GetBump(t.Context(), "guy/repo", 7); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestOpenRepairsRecoverAfterRestart(t *testing.T) {
	s := storetest.Open(t)
	watched(t, s, "guy/repo")
	ctx := t.Context()
	createBump(t, s, "guy/repo", 7)
	v := upkeep.CheckVerdict{HeadSha: bumpSha1, Conclusion: upkeep.CheckFailure, FailingContexts: "test", ConcludedAt: t0.Add(time.Minute)}
	if _, err := s.UpdateBump(ctx, "guy/repo", 7, func(b *upkeep.Bump) error { return b.RecordVerdict(v, bumpCap) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateBump(ctx, "guy/repo", 7, func(b *upkeep.Bump) error {
		_, err := b.StartRepair(bumpBudget(t), "fix it", t0.Add(2*time.Minute), bumpCap)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	open, err := s.OpenRepairs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("open rounds = %d, want 1", len(open))
	}
	if open[0].Repository != "guy/repo" || open[0].Number != 7 || open[0].ID == "" {
		t.Errorf("open round = %+v", open[0])
	}
}
