package github

import (
	"context"
	"strings"
	"testing"

	"github.com/guygrigsby/autophage/internal/store"
	"github.com/guygrigsby/autophage/internal/storetest"
	"github.com/guygrigsby/autophage/internal/upkeep"
)

const (
	headA = "1111111111111111111111111111111111111111"
	headB = "2222222222222222222222222222222222222222"
)

// stubRollup answers CheckRollup from a script, so the translator can be
// driven through every rollup shape without a GitHub.
type stubRollup struct {
	rollups map[string]Rollup
	calls   int
}

func (s *stubRollup) CheckRollup(_ context.Context, _ string, headSha string) (Rollup, error) {
	s.calls++
	r, ok := s.rollups[headSha]
	if !ok {
		return Rollup{}, nil
	}
	return r, nil
}

// deliverAndProcess stores one delivery and processes it before the next
// arrives, which is what production does: deliveries are processed oldest
// first and these fixtures all share a received_at.
func deliverAndProcess(t *testing.T, st *store.Store, tr *Translator, id, event, name string) {
	t.Helper()
	deliver(t, st, id, event, name)
	if err := tr.ProcessPending(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func upkeepTranslator(st *store.Store, r Roller) *Translator {
	return &Translator{Store: st, Clock: fixedClock{t0}, ApprovedLabel: "approved", BotLogin: "autophage[bot]", DependabotLogin: "dependabot[bot]", Rollup: r, RoundCap: 3}
}

// enrolled installs the app and watches the repository, which is what a bump
// needs before it can exist.
func enrolledAndWatched(t *testing.T, st *store.Store, tr *Translator) {
	t.Helper()
	deliver(t, st, "d-inst", "installation", "installation_created.json")
	if err := tr.ProcessPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	w, err := upkeep.NewWatch("guy/repo", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WatchRepository(t.Context(), w); err != nil {
		t.Fatal(err)
	}
}

func TestPullRequestOpenedCreatesABump(t *testing.T) {
	st := storetest.Open(t)
	tr := upkeepTranslator(st, &stubRollup{})
	enrolledAndWatched(t, st, tr)

	deliver(t, st, "d-pr", "pull_request", "pull_request_opened.json")
	if err := tr.ProcessPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r, d := processing(t, st, "d-pr"); r != "translated" {
		t.Fatalf("result = %s (%s), want translated", r, d)
	}
	b, err := st.GetBump(t.Context(), "guy/repo", 11)
	if err != nil {
		t.Fatal(err)
	}
	if b.State() != upkeep.AwaitingChecks || b.HeadSha() != headA {
		t.Errorf("bump = %s@%s", b.State(), b.HeadSha())
	}
	if b.Branch() != "dependabot/go_modules/golang.org/x/net-0.38.0" || b.BaseBranch() != "main" {
		t.Errorf("branch = %s onto %s", b.Branch(), b.BaseBranch())
	}
}

// Trust here is identity, not association: anyone else's pull request is not
// Upkeep's business at all.
func TestPullRequestFromAHumanIsIgnored(t *testing.T) {
	st := storetest.Open(t)
	tr := upkeepTranslator(st, &stubRollup{})
	enrolledAndWatched(t, st, tr)

	deliver(t, st, "d-human", "pull_request", "pull_request_opened_human.json")
	if err := tr.ProcessPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r, _ := processing(t, st, "d-human"); r != "ignored" {
		t.Errorf("result = %s, want ignored", r)
	}
	if _, err := st.GetBump(t.Context(), "guy/repo", 12); err == nil {
		t.Error("a human's pull request became a bump")
	}
}

// Enrollment is not enough. An unwatched repository's bumps are ignored, not
// rejected: nothing is wrong, autophage is simply not doing that repository.
func TestPullRequestOnAnUnwatchedRepositoryIsIgnored(t *testing.T) {
	st := storetest.Open(t)
	tr := upkeepTranslator(st, &stubRollup{})
	deliver(t, st, "d-inst", "installation", "installation_created.json")
	if err := tr.ProcessPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	deliver(t, st, "d-pr", "pull_request", "pull_request_opened.json")
	if err := tr.ProcessPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r, _ := processing(t, st, "d-pr"); r != "ignored" {
		t.Errorf("result = %s, want ignored", r)
	}
}

func TestSynchronizeAdvancesTheHead(t *testing.T) {
	st := storetest.Open(t)
	tr := upkeepTranslator(st, &stubRollup{})
	enrolledAndWatched(t, st, tr)
	deliverAndProcess(t, st, tr, "d-pr", "pull_request", "pull_request_opened.json")
	deliverAndProcess(t, st, tr, "d-sync", "pull_request", "pull_request_synchronize.json")
	if r, d := processing(t, st, "d-sync"); r != "translated" {
		t.Fatalf("result = %s (%s), want translated", r, d)
	}
	b, err := st.GetBump(t.Context(), "guy/repo", 11)
	if err != nil {
		t.Fatal(err)
	}
	if b.HeadSha() != headB {
		t.Errorf("head = %s, want %s", b.HeadSha(), headB)
	}
}

func TestClosedRecordsMergedOrDiscarded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture string
		want    upkeep.ClosureKind
	}{
		{"merged", "pull_request_closed_merged.json", upkeep.Merged},
		{"discarded", "pull_request_closed_discarded.json", upkeep.Discarded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := storetest.Open(t)
			tr := upkeepTranslator(st, &stubRollup{})
			enrolledAndWatched(t, st, tr)
			deliverAndProcess(t, st, tr, "d-pr", "pull_request", "pull_request_opened.json")
			deliverAndProcess(t, st, tr, "d-close", "pull_request", tc.fixture)
			if r, d := processing(t, st, "d-close"); r != "translated" {
				t.Fatalf("result = %s (%s), want translated", r, d)
			}
			b, err := st.GetBump(t.Context(), "guy/repo", 11)
			if err != nil {
				t.Fatal(err)
			}
			cl := b.Closure()
			if cl == nil || cl.Kind != tc.want {
				t.Errorf("closure = %+v, want %s", cl, tc.want)
			}
			if b.State() != upkeep.Closed {
				t.Errorf("state = %s, want closed", b.State())
			}
		})
	}
}

// One completed suite does not mean the checks are done. Until the rollup is
// conclusive nothing is recorded, and the bump keeps waiting.
func TestPendingRollupRecordsNoVerdict(t *testing.T) {
	st := storetest.Open(t)
	rs := &stubRollup{rollups: map[string]Rollup{headA: {Conclusive: false}}}
	tr := upkeepTranslator(st, rs)
	enrolledAndWatched(t, st, tr)
	deliverAndProcess(t, st, tr, "d-pr", "pull_request", "pull_request_opened.json")
	deliverAndProcess(t, st, tr, "d-cs", "check_suite", "check_suite_completed.json")
	if r, _ := processing(t, st, "d-cs"); r != "ignored" {
		t.Errorf("result = %s, want ignored", r)
	}
	b, err := st.GetBump(t.Context(), "guy/repo", 11)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Verdicts()) != 0 {
		t.Errorf("verdicts = %d, want none while the rollup is pending", len(b.Verdicts()))
	}
	if b.State() != upkeep.AwaitingChecks {
		t.Errorf("state = %s, want awaiting_checks", b.State())
	}
}

func TestConclusiveFailureQueuesTheBump(t *testing.T) {
	st := storetest.Open(t)
	rs := &stubRollup{rollups: map[string]Rollup{headA: {Conclusive: true, Conclusion: upkeep.CheckFailure, FailingContexts: []string{"test (1.26)", "lint"}, DetailsURL: "https://x"}}}
	tr := upkeepTranslator(st, rs)
	enrolledAndWatched(t, st, tr)
	deliverAndProcess(t, st, tr, "d-pr", "pull_request", "pull_request_opened.json")
	deliverAndProcess(t, st, tr, "d-cs", "check_suite", "check_suite_completed.json")
	if r, d := processing(t, st, "d-cs"); r != "translated" {
		t.Fatalf("result = %s (%s), want translated", r, d)
	}
	b, err := st.GetBump(t.Context(), "guy/repo", 11)
	if err != nil {
		t.Fatal(err)
	}
	if b.State() != upkeep.Queued {
		t.Errorf("state = %s, want queued", b.State())
	}
	v := b.CurrentVerdict()
	if v == nil || v.FailingContexts != "test (1.26)\nlint" {
		t.Errorf("verdict = %+v", v)
	}
}

func TestWorkflowRunSuccessGoesGreen(t *testing.T) {
	st := storetest.Open(t)
	rs := &stubRollup{rollups: map[string]Rollup{headA: {Conclusive: true, Conclusion: upkeep.CheckSuccess}}}
	tr := upkeepTranslator(st, rs)
	enrolledAndWatched(t, st, tr)
	deliverAndProcess(t, st, tr, "d-pr", "pull_request", "pull_request_opened.json")
	deliverAndProcess(t, st, tr, "d-wr", "workflow_run", "workflow_run_completed.json")
	if r, d := processing(t, st, "d-wr"); r != "translated" {
		t.Fatalf("result = %s (%s), want translated", r, d)
	}
	b, err := st.GetBump(t.Context(), "guy/repo", 11)
	if err != nil {
		t.Fatal(err)
	}
	if b.State() != upkeep.Green {
		t.Errorf("state = %s, want green", b.State())
	}
}

// CI finishing on a head a round is already working, and a rollup for a head
// a force-push replaced, are both ordinary. Recording either as a rejection
// would put the normal course of events in the ledger as a failure, which is
// what autophage-2lq caught on the Case side.
func TestAStaleOrUntimelyVerdictIsIgnoredNotRejected(t *testing.T) {
	st := storetest.Open(t)
	rs := &stubRollup{rollups: map[string]Rollup{
		headA: {Conclusive: true, Conclusion: upkeep.CheckFailure, FailingContexts: []string{"test"}},
	}}
	tr := upkeepTranslator(st, rs)
	enrolledAndWatched(t, st, tr)
	deliverAndProcess(t, st, tr, "d-pr", "pull_request", "pull_request_opened.json")
	deliverAndProcess(t, st, tr, "d-sync", "pull_request", "pull_request_synchronize.json")
	// The suite fixture is for headA, which the force-push has replaced.
	deliverAndProcess(t, st, tr, "d-cs", "check_suite", "check_suite_completed.json")
	if r, d := processing(t, st, "d-cs"); r != "ignored" {
		t.Errorf("result = %s (%s), want ignored", r, d)
	}
}

// A check_suite for a pull request autophage never made a bump for carries
// no bump to update. Ignored, not an error.
func TestCheckSuiteForAnUnknownBumpIsIgnored(t *testing.T) {
	st := storetest.Open(t)
	rs := &stubRollup{rollups: map[string]Rollup{headA: {Conclusive: true, Conclusion: upkeep.CheckSuccess}}}
	tr := upkeepTranslator(st, rs)
	enrolledAndWatched(t, st, tr)
	deliver(t, st, "d-cs", "check_suite", "check_suite_completed.json")
	if err := tr.ProcessPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r, _ := processing(t, st, "d-cs"); r != "ignored" {
		t.Errorf("result = %s, want ignored", r)
	}
	if rs.calls != 0 {
		t.Errorf("rollup calls = %d, want 0: no bump means no reason to ask GitHub", rs.calls)
	}
}

// autophage pushing a repair is a synchronize from its own bot login. The
// existing self-event check must not swallow it, because the head genuinely
// moved and the wait has to restart.
func TestOwnEventCheckStillIgnoresOurOwnDeliveries(t *testing.T) {
	st := storetest.Open(t)
	tr := upkeepTranslator(st, &stubRollup{})
	enrolledAndWatched(t, st, tr)
	body := fixture(t, "pull_request_synchronize.json")
	d := store.Delivery{ID: "d-own", Event: "pull_request", Action: "synchronize", SenderLogin: "autophage[bot]", Payload: body, ReceivedAt: t0}
	if _, err := st.StoreDelivery(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if err := tr.ProcessPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r, detail := processing(t, st, "d-own"); r != "ignored" || detail != "own event" {
		t.Errorf("result = %s (%s), want ignored/own event", r, detail)
	}
}

// A fork's pull request carries the fork's branch name, and recording it
// would later point a repair push at a same-named branch in the base
// repository. Refused here as a decision, not left to fail later in git.
func TestPullRequestFromAForkIsIgnored(t *testing.T) {
	st := storetest.Open(t)
	tr := upkeepTranslator(st, &stubRollup{})
	enrolledAndWatched(t, st, tr)
	body := fixture(t, "pull_request_opened.json")
	forked := strings.Replace(string(body),
		`"head": {"ref": "dependabot/go_modules/golang.org/x/net-0.38.0", "sha": "1111111111111111111111111111111111111111"}`,
		`"head": {"ref": "dependabot/go_modules/golang.org/x/net-0.38.0", "sha": "1111111111111111111111111111111111111111", "repo": {"full_name": "attacker/repo"}}`, 1)
	d := store.Delivery{ID: "d-fork", Event: "pull_request", Action: "opened", SenderLogin: "dependabot[bot]", Payload: []byte(forked), ReceivedAt: t0}
	if _, err := st.StoreDelivery(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if err := tr.ProcessPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r, detail := processing(t, st, "d-fork"); r != "ignored" {
		t.Errorf("result = %s (%s), want ignored", r, detail)
	}
	if _, err := st.GetBump(t.Context(), "guy/repo", 11); err == nil {
		t.Error("a fork's pull request became a bump")
	}
}
