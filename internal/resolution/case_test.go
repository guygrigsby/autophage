package resolution

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

func trusted(t *testing.T) Requester {
	t.Helper()
	r, err := NewRequester("guy", AssociationOwner)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func untrusted(t *testing.T) Requester {
	t.Helper()
	r, err := NewRequester("drive-by", AssociationNone)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func budget(t *testing.T) Budget {
	t.Helper()
	b, err := NewBudget(10, time.Hour, 500)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func approval(src ApprovalSource) Approval {
	r, _ := NewRequester("guy", AssociationOwner)
	a := Approval{Approver: r, Source: src, ApprovedAt: t0}
	if src == SourceLabel {
		a.DeliveryID = "d-1"
	}
	return a
}

// driveTo moves a fresh trusted case into the named state through legal
// transitions, so each table row can start from any state.
func driveTo(t *testing.T, state CaseState) *Case {
	t.Helper()
	c, err := NewCase("guy/repo", 7, trusted(t), t0)
	if err != nil {
		t.Fatal(err)
	}
	step := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("driveTo %s: %v", state, err)
		}
	}
	switch state {
	case Received:
	case Gated:
		c, err = NewCase("guy/repo", 7, untrusted(t), t0)
		step(err)
	case Queued:
		step(c.RecordTriage(Triage{Size: Small, Rationale: "typo", Model: "m", TriagedAt: t0}))
	case AwaitingApproval:
		step(c.RecordTriage(Triage{Size: Large, Rationale: "rewrite", Model: "m", TriagedAt: t0}))
	case Attempting:
		step(c.RecordTriage(Triage{Size: Small, Rationale: "typo", Model: "m", TriagedAt: t0}))
		_, err = c.StartAttempt(Auto, budget(t), "brief", t0)
		step(err)
	case Failed:
		step(c.RecordTriage(Triage{Size: Small, Rationale: "typo", Model: "m", TriagedAt: t0}))
		_, err = c.StartAttempt(Auto, budget(t), "brief", t0)
		step(err)
		o, _ := OutcomeFailed(FailureInfra, "podman died", Usage{}, t0)
		step(c.RecordOutcome("", o))
	case Done:
		step(c.RecordTriage(Triage{Size: Small, Rationale: "typo", Model: "m", TriagedAt: t0}))
		_, err = c.StartAttempt(Auto, budget(t), "brief", t0)
		step(err)
		o, _ := OutcomePullRequest(12, "0123456789abcdef0123456789abcdef01234567", "fixed", Usage{Turns: 3}, t0)
		step(c.RecordOutcome("", o))
	case Closed:
		// Closed carries an open attempt, the only shape in which an
		// outcome is recordable there: the issue was closed while an
		// attempt was running and the runner has yet to end it.
		step(c.RecordTriage(Triage{Size: Small, Rationale: "typo", Model: "m", TriagedAt: t0}))
		_, err = c.StartAttempt(Auto, budget(t), "brief", t0)
		step(err)
		step(c.Close(Closure{DeliveryID: "d-9", ClosedAt: t0}))
	}
	if c.State() != state {
		t.Fatalf("driveTo %s landed in %s", state, c.State())
	}
	c.ClearChanges()
	return c
}

func TestNewCaseGatesByTrust(t *testing.T) {
	c, err := NewCase("guy/repo", 1, trusted(t), t0)
	if err != nil || c.State() != Received {
		t.Fatalf("trusted: %s %v", c.State(), err)
	}
	g, err := NewCase("guy/repo", 2, untrusted(t), t0)
	if err != nil || g.State() != Gated {
		t.Fatalf("untrusted: %s %v", g.State(), err)
	}
	if c.Branch() != "autophage/1" {
		t.Errorf("branch = %q", c.Branch())
	}
	if _, err := NewCase("", 1, trusted(t), t0); err == nil {
		t.Error("empty repository accepted")
	}
	if _, err := NewCase("guy/repo", 0, trusted(t), t0); err == nil {
		t.Error("zero number accepted")
	}
}

// TestTransitionTable is the domain model's state table: every listed row
// passes and lands where it says; every state not listed for a command is
// refused. The map key is the command name; the value lists (from, to).
func TestTransitionTable(t *testing.T) {
	type row struct{ from, to CaseState }
	table := map[string][]row{
		"triage_small":      {{Received, Queued}},
		"triage_large":      {{Received, AwaitingApproval}},
		"approval":          {{Gated, Queued}, {AwaitingApproval, Queued}, {Failed, Queued}, {Received, Received}, {Queued, Queued}, {Attempting, Attempting}},
		"start_attempt":     {{Queued, Attempting}},
		"outcome_pr":        {{Attempting, Done}, {Closed, Closed}},
		"outcome_exhausted": {{Attempting, AwaitingApproval}, {Closed, Closed}},
		"outcome_failed":    {{Attempting, Failed}, {Closed, Closed}},
		"outcome_op_stop":   {{Attempting, AwaitingApproval}, {Closed, Closed}},
		"outcome_restart":   {{Attempting, Queued}, {Closed, Closed}},
		// The one outcome Closed alone admits: an abort for the closure
		// itself is meaningless while the case is still open.
		"outcome_issue_closed": {{Closed, Closed}},
		"close":                {{Received, Closed}, {Gated, Closed}, {Queued, Closed}, {AwaitingApproval, Closed}, {Failed, Closed}, {Attempting, Closed}},
	}
	apply := func(c *Case, cmd string) error {
		switch cmd {
		case "triage_small":
			return c.RecordTriage(Triage{Size: Small, Rationale: "r", Model: "m", TriagedAt: t0})
		case "triage_large":
			return c.RecordTriage(Triage{Size: Large, Rationale: "r", Model: "m", TriagedAt: t0})
		case "approval":
			return c.RecordApproval(approval(SourceLabel))
		case "start_attempt":
			_, err := c.StartAttempt(Auto, budget(t), "brief", t0)
			return err
		case "outcome_pr":
			o, _ := OutcomePullRequest(1, "0123456789abcdef0123456789abcdef01234567", "s", Usage{}, t0)
			return c.RecordOutcome("", o)
		case "outcome_exhausted":
			o, _ := OutcomeExhausted(LimitTurns, "s", Usage{Turns: 10}, t0)
			return c.RecordOutcome("", o)
		case "outcome_failed":
			o, _ := OutcomeFailed(FailureAgent, "gave up", Usage{}, t0)
			return c.RecordOutcome("", o)
		case "outcome_op_stop":
			o, _ := OutcomeAborted(AbortOperatorStop, "s", Usage{}, t0)
			return c.RecordOutcome("", o)
		case "outcome_restart":
			o, _ := OutcomeAborted(AbortDaemonRestart, "s", Usage{}, t0)
			return c.RecordOutcome("", o)
		case "outcome_issue_closed":
			o, _ := OutcomeAborted(AbortIssueClosed, "s", Usage{}, t0)
			return c.RecordOutcome("", o)
		case "close":
			return c.Close(Closure{DeliveryID: "d", ClosedAt: t0})
		}
		t.Fatalf("unknown command %s", cmd)
		return nil
	}
	for cmd, rows := range table {
		listed := map[CaseState]CaseState{}
		for _, r := range rows {
			listed[r.from] = r.to
		}
		for _, from := range AllCaseStates() {
			c := driveTo(t, from)
			// A refusal must leave the aggregate exactly as it was: same
			// state, same open attempt, nothing queued for the store.
			wasState, wasOpen, wasChanges := c.State(), c.OpenAttempt(), c.Changes()
			err := apply(c, cmd)
			to, ok := listed[from]
			switch {
			case ok && err != nil:
				t.Errorf("%s from %s: refused: %v", cmd, from, err)
			case ok && c.State() != to:
				t.Errorf("%s from %s: landed in %s, want %s", cmd, from, c.State(), to)
			case !ok && err == nil:
				t.Errorf("%s from %s: accepted, want refusal", cmd, from)
			case !ok && !errors.Is(err, ErrRefused):
				t.Errorf("%s from %s: wrong error %v", cmd, from, err)
			case !ok:
				if c.State() != wasState {
					t.Errorf("%s from %s: refusal moved the state to %s", cmd, from, c.State())
				}
				if !reflect.DeepEqual(c.OpenAttempt(), wasOpen) {
					t.Errorf("%s from %s: refusal changed the open attempt to %+v", cmd, from, c.OpenAttempt())
				}
				if !reflect.DeepEqual(c.Changes(), wasChanges) {
					t.Errorf("%s from %s: refusal recorded changes %+v", cmd, from, c.Changes())
				}
			}
			if ok && from != to {
				tr := c.Changes().Transitions
				if len(tr) != 1 || tr[0].From != from || tr[0].To != to {
					t.Errorf("%s from %s: transitions recorded = %+v", cmd, from, tr)
				}
			}
			if ok && from == to && len(c.Changes().Transitions) != 0 {
				t.Errorf("%s from %s: no-op transition recorded", cmd, from)
			}
		}
	}
}

func TestDaemonRestartRequeuesOnce(t *testing.T) {
	c := driveTo(t, Attempting)
	o, _ := OutcomeAborted(AbortDaemonRestart, "restart", Usage{}, t0)
	if err := c.RecordOutcome("", o); err != nil || c.State() != Queued {
		t.Fatalf("first restart: %s %v", c.State(), err)
	}
	if _, err := c.StartAttempt(Auto, budget(t), "brief", t0); err != nil {
		t.Fatal(err)
	}
	if err := c.RecordOutcome("", o); err != nil || c.State() != Failed {
		t.Fatalf("second restart: %s %v", c.State(), err)
	}
	tr := c.Changes().Transitions
	if tr[len(tr)-1].Cause != CauseOutcomeAbortedRestartFailed {
		t.Errorf("cause = %s", tr[len(tr)-1].Cause)
	}
}

func TestCloseWhileAttemptingKeepsOutcomeRecordable(t *testing.T) {
	c := driveTo(t, Attempting)
	if err := c.Close(Closure{DeliveryID: "d", ClosedAt: t0}); err != nil {
		t.Fatal(err)
	}
	o, _ := OutcomeAborted(AbortIssueClosed, "closed", Usage{}, t0)
	if err := c.RecordOutcome("", o); err != nil {
		t.Fatalf("outcome after close refused: %v", err)
	}
	if c.State() != Closed || c.OpenAttempt() != nil {
		t.Errorf("state %s open %v", c.State(), c.OpenAttempt())
	}
	if len(c.Changes().Transitions) != 1 {
		t.Errorf("transitions = %+v", c.Changes().Transitions)
	}
}

func TestTriageOnlyOnceAndOnlyTrusted(t *testing.T) {
	c := driveTo(t, Queued)
	if err := c.RecordTriage(Triage{Size: Small, Rationale: "again", Model: "m", TriagedAt: t0}); !errors.Is(err, ErrRefused) {
		t.Errorf("second triage: %v", err)
	}
	g := driveTo(t, Gated)
	if err := g.RecordTriage(Triage{Size: Small, Rationale: "r", Model: "m", TriagedAt: t0}); !errors.Is(err, ErrRefused) {
		t.Errorf("gated triage: %v", err)
	}
	if err := driveTo(t, Received).RecordTriage(Triage{Size: Small, Model: "m", TriagedAt: t0}); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty rationale: %v", err)
	}
}

func TestAttemptOrdinalsAndOpenAttempt(t *testing.T) {
	c := driveTo(t, Queued)
	a1, err := c.StartAttempt(Auto, budget(t), "brief", t0)
	if err != nil || a1.Ordinal != 1 || c.OpenAttempt() == nil {
		t.Fatalf("first: %+v %v", a1, err)
	}
	if _, err := c.StartAttempt(Auto, budget(t), "brief", t0); !errors.Is(err, ErrRefused) {
		t.Errorf("second open attempt: %v", err)
	}
	if err := c.RecordRun("", Run{RunID: "r1", Model: "m", BaseSha: "0123456789abcdef0123456789abcdef01234567", BeganAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := c.RecordRun("", Run{RunID: "r2", Model: "m", BaseSha: "0123456789abcdef0123456789abcdef01234567", BeganAt: t0}); !errors.Is(err, ErrRefused) {
		t.Errorf("second run: %v", err)
	}
	o, _ := OutcomeExhausted(LimitWallClock, "out of time", Usage{Turns: 4}, t0)
	if err := c.RecordOutcome("", o); err != nil {
		t.Fatal(err)
	}
	if err := c.RecordApproval(approval(SourceOperator)); err != nil {
		t.Fatal(err)
	}
	a2, err := c.StartAttempt(c.NextAttemptKind(), budget(t), "brief 2", t0.Add(time.Hour))
	if err != nil || a2.Ordinal != 2 || a2.Kind != Approved {
		t.Fatalf("second attempt: %+v %v", a2, err)
	}
	ch := c.Changes()
	if len(ch.Attempts) != 2 || len(ch.Runs) != 1 || len(ch.Outcomes) != 1 || len(ch.Approvals) != 1 {
		t.Errorf("changes = %+v", ch)
	}
}

func TestNextAttemptKind(t *testing.T) {
	c := driveTo(t, Queued)
	if c.NextAttemptKind() != Auto {
		t.Error("triaged small should be auto")
	}
	g := driveTo(t, Gated)
	_ = g.RecordApproval(approval(SourceLabel))
	if g.NextAttemptKind() != Approved {
		t.Error("approved gated case should be approved")
	}
}

func TestOutcomeConstructorsValidate(t *testing.T) {
	if _, err := OutcomePullRequest(0, "0123456789abcdef0123456789abcdef01234567", "s", Usage{}, t0); err == nil {
		t.Error("pr 0 accepted")
	}
	if _, err := OutcomePullRequest(1, "short", "s", Usage{}, t0); err == nil {
		t.Error("bad sha accepted")
	}
	if _, err := OutcomeExhausted(LimitTurns, "", Usage{}, t0); err == nil {
		t.Error("empty summary accepted")
	}
	if _, err := OutcomeFailed(FailureInfra, "", Usage{}, t0); err == nil {
		t.Error("empty message accepted")
	}
	o, err := OutcomeFailed(FailureInfra, "boom", Usage{}, t0)
	if err != nil || o.Summary != "boom" {
		t.Errorf("failed summary = %q %v", o.Summary, err)
	}
}

func TestLoadCaseRoundTrips(t *testing.T) {
	c := driveTo(t, Attempting)
	snap := Snapshot{ID: "id-1", Repository: c.Repository(), Number: c.Number(), Requester: c.Requester(), State: c.State(), ReceivedAt: c.ReceivedAt(), Triage: c.Triage(), Attempts: c.Attempts(), Transitions: c.Transitions()}
	l, err := LoadCase(snap)
	if err != nil {
		t.Fatal(err)
	}
	if l.ID() != "id-1" || l.State() != Attempting || l.OpenAttempt() == nil || len(l.Changes().Transitions) != 0 {
		t.Errorf("loaded = %+v", l)
	}
	snap.State = Gated
	if _, err := LoadCase(snap); err == nil {
		t.Error("trusted requester in gated state accepted")
	}
}

func TestOpenAttemptIsACopy(t *testing.T) {
	c := driveTo(t, Attempting)
	a := c.OpenAttempt()
	o, _ := OutcomeFailed(FailureInfra, "leaked", Usage{}, t0)
	a.Outcome = &o
	if c.OpenAttempt() == nil {
		t.Error("mutating the returned attempt closed the real one")
	}
	if len(c.Changes().Outcomes) != 0 {
		t.Errorf("mutating the returned attempt changed Changes(): %+v", c.Changes())
	}
}

func TestAttemptElapsed(t *testing.T) {
	a := Attempt{StartedAt: t0}
	if got := a.Elapsed(t0.Add(time.Hour)); got != time.Hour {
		t.Errorf("elapsed = %s", got)
	}
}
