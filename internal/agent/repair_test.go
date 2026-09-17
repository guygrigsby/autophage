package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	ac "github.com/voocel/agentcore"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
	"github.com/guygrigsby/autophage/internal/storetest"
	"github.com/guygrigsby/autophage/internal/upkeep"
)

const bumpHead = "1111111111111111111111111111111111111111"

// startedRound seeds a watched repository, a red bump and one open repair
// round, and returns the round's id.
func startedRound(t *testing.T, st *store.Store) string {
	t.Helper()
	ctx := t.Context()
	repo, err := resolution.NewRepository("guy/repo", 42, "main", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnrollRepository(ctx, repo); err != nil {
		t.Fatal(err)
	}
	w, _ := upkeep.NewWatch("guy/repo", t0)
	if err := st.WatchRepository(ctx, w); err != nil {
		t.Fatal(err)
	}
	b, err := upkeep.NewBump("guy/repo", 11, "dependabot/go_modules/y", "main", bumpHead, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateBump(ctx, b); err != nil {
		t.Fatal(err)
	}
	v := upkeep.CheckVerdict{HeadSha: bumpHead, Conclusion: upkeep.CheckFailure, FailingContexts: "test (1.26)", ConcludedAt: t0}
	if _, err := st.UpdateBump(ctx, "guy/repo", 11, func(b *upkeep.Bump) error { return b.RecordVerdict(v, 3) }); err != nil {
		t.Fatal(err)
	}
	budget, err := resolution.NewBudget(10, time.Minute, 500)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.UpdateBump(ctx, "guy/repo", 11, func(b *upkeep.Bump) error {
		_, err := b.StartRepair(budget, "make the checks pass", t0, 3)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return got.OpenRepair().ID
}

func newRepairer(t *testing.T, st *store.Store, sb *fakeSandbox, gh *fakeGitHub, model ac.ChatModel) (*Repairer, *countMetrics) {
	t.Helper()
	m := &countMetrics{}
	logs := &logRecorder{t: t}
	return &Repairer{
		Store: st, GitHub: gh, Sandbox: sb, Ledger: &memLedger{}, Clock: resolution.SystemClock{},
		Model: "test/repair", RoundCap: 3, Metrics: m, Logf: logs.logf,
		CloneURL:      func(repo string) string { return "file:///" + repo },
		ModelOverride: model,
	}, m
}

func loadRound(t *testing.T, st *store.Store) upkeep.RepairAttempt {
	t.Helper()
	b, err := st.GetBump(t.Context(), "guy/repo", 11)
	if err != nil {
		t.Fatal(err)
	}
	rounds := b.Attempts()
	if len(rounds) == 0 {
		t.Fatal("no rounds")
	}
	return rounds[len(rounds)-1]
}

func TestRepairPushesAndReturnsTheBumpToAwaitingChecks(t *testing.T) {
	st := storetest.Open(t)
	id := startedRound(t, st)
	sb := &fakeSandbox{commits: true}
	gh := &fakeGitHub{}
	p, _ := newRepairer(t, st, sb, gh, scripted(0, "What was failing:\ntest\nWhat I changed:\nthe pin\nWhy I think the checks will pass:\nit builds\nWhat is left:\nnothing", &atomic.Bool{}))
	p.Run(t.Context(), id)

	b, err := st.GetBump(t.Context(), "guy/repo", 11)
	if err != nil {
		t.Fatal(err)
	}
	o := loadRound(t, st).Outcome
	if o == nil || o.Kind != upkeep.Pushed {
		t.Fatalf("outcome = %+v, want pushed", o)
	}
	if o.HeadSha != headSha {
		t.Errorf("head sha = %s, want %s", o.HeadSha, headSha)
	}
	if b.State() != upkeep.AwaitingChecks {
		t.Errorf("state = %s, want awaiting_checks", b.State())
	}
	if b.HeadSha() != headSha {
		t.Errorf("bump head = %s, want the pushed sha", b.HeadSha())
	}
	// A repair works the branch it was given, pinned to the sha CI judged.
	if len(sb.prepared) != 1 || sb.prepared[0] != "guy/repo file:///guy/repo dependabot/go_modules/y "+bumpHead {
		t.Errorf("prepared = %v", sb.prepared)
	}
	index(t, sb.order(), "prepare-head")
	if prs := gh.pullRequests(); len(prs) != 0 {
		t.Errorf("pull requests opened = %v, want none: the pull request already exists", prs)
	}
}

// An agent that changed nothing is not a failure: it ran, it looked, and it
// had nothing to offer. Another round would produce the same nothing.
func TestRepairWithNoCommitsIsNoChange(t *testing.T) {
	st := storetest.Open(t)
	id := startedRound(t, st)
	sb := &fakeSandbox{commits: false}
	p, _ := newRepairer(t, st, sb, &fakeGitHub{}, scripted(0, "What was failing:\ntest\nWhat I changed:\nnothing, the break is upstream\nWhy I think the checks will pass:\nthey will not\nWhat is left:\nwait for upstream", &atomic.Bool{}))
	p.Run(t.Context(), id)

	o := loadRound(t, st).Outcome
	if o == nil || o.Kind != upkeep.NoChange {
		t.Fatalf("outcome = %+v, want no_change", o)
	}
	b, _ := st.GetBump(t.Context(), "guy/repo", 11)
	if b.State() != upkeep.Abandoned {
		t.Errorf("state = %s, want abandoned", b.State())
	}
	if ab := b.Abandonment(); ab == nil || ab.Reason != upkeep.AbandonNoChange {
		t.Errorf("abandonment = %+v", ab)
	}
}

// A push that never reached the remote leaves the work on host disk. Calling
// that a push would point the next verdict at a sha nobody has.
func TestRepairPushFailureIsInfraAndRequeues(t *testing.T) {
	st := storetest.Open(t)
	id := startedRound(t, st)
	sb := &fakeSandbox{commits: true, failPush: errors.New("remote rejected: stale info")}
	p, _ := newRepairer(t, st, sb, &fakeGitHub{}, scripted(0, "done", &atomic.Bool{}))
	p.Run(t.Context(), id)

	o := loadRound(t, st).Outcome
	if o == nil || o.Kind != upkeep.RepairFailed || o.Class != resolution.FailureInfra {
		t.Fatalf("outcome = %+v, want failed{infra}", o)
	}
	// The raw git text is an operator's fact, not something to post.
	if o.Message == "" || o.Message == "remote rejected: stale info" {
		t.Errorf("message = %q: the git error should not be the outcome text", o.Message)
	}
	b, _ := st.GetBump(t.Context(), "guy/repo", 11)
	if b.State() != upkeep.Queued {
		t.Errorf("state = %s, want queued: infra requeues", b.State())
	}
}

// Conflict markers on someone else's branch are worse than on ours: the
// branch is already pushed and the next check run will see them.
func TestRepairThatCommittedConflictMarkersFailsOnTheAgent(t *testing.T) {
	st := storetest.Open(t)
	id := startedRound(t, st)
	sb := &fakeSandbox{commits: true, conflicted: true}
	p, _ := newRepairer(t, st, sb, &fakeGitHub{}, scripted(0, "done", &atomic.Bool{}))
	p.Run(t.Context(), id)

	o := loadRound(t, st).Outcome
	if o == nil || o.Kind != upkeep.RepairFailed || o.Class != resolution.FailureAgent {
		t.Fatalf("outcome = %+v, want failed{agent}", o)
	}
	b, _ := st.GetBump(t.Context(), "guy/repo", 11)
	if b.State() != upkeep.Abandoned {
		t.Errorf("state = %s, want abandoned", b.State())
	}
}

func TestRepairRecordsTheRunForWhy(t *testing.T) {
	st := storetest.Open(t)
	id := startedRound(t, st)
	sb := &fakeSandbox{commits: true}
	p, _ := newRepairer(t, st, sb, &fakeGitHub{}, scripted(0, "done", &atomic.Bool{}))
	p.Run(t.Context(), id)

	a := loadRound(t, st)
	if a.Run == nil || a.Run.RunID == "" {
		t.Fatalf("run = %+v: autophage why has nothing to read", a.Run)
	}
	if a.Run.BaseSha != bumpHead {
		t.Errorf("run base sha = %s, want the head CI judged", a.Run.BaseSha)
	}
	if a.Run.Model != "test/repair" {
		t.Errorf("run model = %s", a.Run.Model)
	}
}

// A shutdown under a running round records no outcome at all, so recovery
// ends it Aborted{DaemonRestart} on the next boot and the bump re-queues.
func TestRepairRecordsNothingWhenTheDaemonGoesDown(t *testing.T) {
	st := storetest.Open(t)
	id := startedRound(t, st)
	sb := &fakeSandbox{commits: true}
	p, _ := newRepairer(t, st, sb, &fakeGitHub{}, blocking())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx, id)
	}()
	waitFor(t, "the toolbox to be dialled", func() bool {
		for _, c := range sb.order() {
			if c == "tools" {
				return true
			}
		}
		return false
	})
	cancel()
	<-done

	if o := loadRound(t, st).Outcome; o != nil {
		t.Errorf("outcome = %+v, want none: a shutdown leaves the round open", o)
	}
}

// An operator stop is not a shutdown: it has an answer, and the round
// records it.
func TestRepairStopRecordsAnAbort(t *testing.T) {
	st := storetest.Open(t)
	id := startedRound(t, st)
	sb := &fakeSandbox{commits: true}
	p, _ := newRepairer(t, st, sb, &fakeGitHub{}, blocking())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(t.Context(), id)
	}()
	waitFor(t, "the toolbox to be dialled", func() bool {
		for _, c := range sb.order() {
			if c == "tools" {
				return true
			}
		}
		return false
	})
	if !p.Stop(id) {
		t.Fatal("Stop reported the round was not running here")
	}
	<-done

	o := loadRound(t, st).Outcome
	if o == nil || o.Kind != upkeep.RepairAborted || o.Reason != upkeep.AbortOperatorStop {
		t.Fatalf("outcome = %+v, want aborted{operator_stop}", o)
	}
	b, _ := st.GetBump(t.Context(), "guy/repo", 11)
	if b.State() != upkeep.Abandoned {
		t.Errorf("state = %s, want abandoned", b.State())
	}
}
