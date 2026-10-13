package store_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
	"github.com/guygrigsby/autophage/internal/storetest"
	"github.com/guygrigsby/autophage/internal/upkeep"
)

// repairRound drives bump n through a failing verdict on its opening head
// and one round run by model, ending in out.
func repairRound(t *testing.T, s *store.Store, n int, model string, out upkeep.RepairOutcome) {
	t.Helper()
	ctx := t.Context()
	createBump(t, s, "guy/repo", n)
	v := upkeep.CheckVerdict{HeadSha: bumpSha1, Conclusion: upkeep.CheckFailure, FailingContexts: "test", ConcludedAt: t0.Add(time.Minute)}
	if _, err := s.UpdateBump(ctx, "guy/repo", n, func(b *upkeep.Bump) error { return b.RecordVerdict(v, bumpCap) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateBump(ctx, "guy/repo", n, func(b *upkeep.Bump) error {
		_, err := b.StartRepair(bumpBudget(t), "fix it", t0.Add(2*time.Minute), bumpCap)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	run := resolution.Run{RunID: fmt.Sprintf("run-%d", n), Model: model, BaseSha: bumpSha1, BeganAt: t0.Add(3 * time.Minute)}
	if _, err := s.UpdateBump(ctx, "guy/repo", n, func(b *upkeep.Bump) error { return b.RecordRun("", run) }); err != nil {
		t.Fatal(err)
	}
	out.EndedAt = t0.Add(4 * time.Minute)
	if _, err := s.UpdateBump(ctx, "guy/repo", n, func(b *upkeep.Bump) error { return b.RecordOutcome("", out, bumpCap) }); err != nil {
		t.Fatal(err)
	}
}

func judge(t *testing.T, s *store.Store, n int, c upkeep.CheckConclusion) {
	t.Helper()
	v := upkeep.CheckVerdict{HeadSha: bumpSha2, Conclusion: c, ConcludedAt: t0.Add(time.Hour)}
	if c == upkeep.CheckFailure {
		v.FailingContexts = "test"
	}
	if _, err := s.UpdateBump(t.Context(), "guy/repo", n, func(b *upkeep.Bump) error { return b.RecordVerdict(v, bumpCap) }); err != nil {
		t.Fatal(err)
	}
}

// Only CI's verdict on the head a round pushed counts: the verdict that
// opened the round judged dependabot's code, and a round that pushed
// nothing or is still waiting on checks has no verdict at all.
func TestRepairVerdictsByModel(t *testing.T) {
	s := storetest.Open(t)
	watched(t, s, "guy/repo")
	pushed := upkeep.RepairOutcome{Kind: upkeep.Pushed, HeadSha: bumpSha2, Summary: "raised the pin"}

	repairRound(t, s, 1, "qwen-27b", pushed)
	judge(t, s, 1, upkeep.CheckSuccess)
	repairRound(t, s, 2, "qwen-27b", pushed)
	judge(t, s, 2, upkeep.CheckFailure)
	repairRound(t, s, 3, "qwen-27b", pushed)
	judge(t, s, 3, upkeep.CheckSuccess)
	repairRound(t, s, 4, "sonnet", pushed)
	judge(t, s, 4, upkeep.CheckFailure)
	repairRound(t, s, 5, "sonnet", pushed) // awaiting checks: no verdict yet
	repairRound(t, s, 6, "sonnet", upkeep.RepairOutcome{Kind: upkeep.NoChange, Summary: "nothing to change"})

	got, err := s.RepairVerdictsByModel(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []store.ModelVerdicts{{Model: "qwen-27b", Passes: 2, Fails: 1}, {Model: "sonnet", Passes: 0, Fails: 1}}
	if !slices.Equal(got, want) {
		t.Errorf("verdicts = %+v, want %+v", got, want)
	}
}
