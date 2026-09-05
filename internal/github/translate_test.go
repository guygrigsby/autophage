package github

import (
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
	"github.com/guygrigsby/autophage/internal/storetest"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

var t0 = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

func deliver(t *testing.T, st *store.Store, id, event, name string) store.Delivery {
	t.Helper()
	body := fixture(t, name)
	d := store.Delivery{ID: id, Event: event, Action: actionOf(body), SenderLogin: senderOf(body), Payload: body, ReceivedAt: t0}
	if _, err := st.StoreDelivery(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	return d
}

func processing(t *testing.T, st *store.Store, id string) (result, detail string) {
	t.Helper()
	err := st.Pool().QueryRow(t.Context(), `select result, detail from webhook_delivery_processings where delivery_id = $1`, id).Scan(&result, &detail)
	if err != nil {
		t.Fatalf("processing row for %s: %v", id, err)
	}
	return result, detail
}

func newTranslator(st *store.Store) *Translator {
	return &Translator{Store: st, Clock: fixedClock{t0}, ApprovedLabel: "approved", BotLogin: "autophage[bot]"}
}

func TestTranslateInstallationThenIssueLifecycle(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	tr := newTranslator(st)

	deliver(t, st, "d-inst", "installation", "installation_created.json")
	if err := tr.ProcessPending(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := processing(t, st, "d-inst"); r != "translated" {
		t.Errorf("installation result = %s", r)
	}
	repo, err := st.GetRepository(ctx, "guy/repo")
	if err != nil || repo.InstallationID != 42 || !repo.Enrolled() {
		t.Fatalf("repo = %+v %v", repo, err)
	}

	deliver(t, st, "d-open", "issues", "issues_opened.json")
	_ = tr.ProcessPending(ctx)
	c, err := st.GetCase(ctx, "guy/repo", 7)
	if err != nil || c.State() != resolution.Received || c.Requester().Login != "guy" || c.Requester().Trust != resolution.Trusted {
		t.Fatalf("case = %+v %v", c, err)
	}
	if r, d := processing(t, st, "d-open"); r != "translated" || d == "" {
		t.Errorf("open = %s %q", r, d)
	}

	deliver(t, st, "d-open-again", "issues", "issues_opened.json")
	_ = tr.ProcessPending(ctx)
	if r, d := processing(t, st, "d-open-again"); r != "ignored" || d != "case exists" {
		t.Errorf("reopen = %s %q", r, d)
	}

	if _, err := st.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error {
		return c.RecordTriage(resolution.Triage{Size: resolution.Large, Rationale: "big", Model: "m", TriagedAt: t0})
	}); err != nil {
		t.Fatal(err)
	}
	deliver(t, st, "d-label", "issues", "issues_labeled.json")
	_ = tr.ProcessPending(ctx)
	c, _ = st.GetCase(ctx, "guy/repo", 7)
	if c.State() != resolution.Queued || len(c.Approvals()) != 1 || c.Approvals()[0].Source != resolution.SourceLabel || c.Approvals()[0].DeliveryID != "d-label" {
		t.Errorf("after label: %s %+v", c.State(), c.Approvals())
	}

	deliver(t, st, "d-close", "issues", "issues_closed.json")
	_ = tr.ProcessPending(ctx)
	c, _ = st.GetCase(ctx, "guy/repo", 7)
	if c.State() != resolution.Closed {
		t.Errorf("after close: %s", c.State())
	}
	deliver(t, st, "d-close-again", "issues", "issues_closed.json")
	_ = tr.ProcessPending(ctx)
	if r, _ := processing(t, st, "d-close-again"); r != "rejected" {
		t.Errorf("second close = %s", r)
	}

	deliver(t, st, "d-rm", "installation_repositories", "installation_repositories_removed.json")
	_ = tr.ProcessPending(ctx)
	repo, _ = st.GetRepository(ctx, "guy/repo")
	if repo.Enrolled() {
		t.Error("repo still enrolled after removal")
	}
}

func TestTranslateIgnoresUnenrolledOwnAndUnsubscribed(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	tr := newTranslator(st)
	deliver(t, st, "d-open", "issues", "issues_opened.json")
	_ = tr.ProcessPending(ctx)
	if r, d := processing(t, st, "d-open"); r != "ignored" || d != "repository not enrolled" {
		t.Errorf("unenrolled = %s %q", r, d)
	}
	body := fixture(t, "issues_opened.json")
	own := store.Delivery{ID: "d-own", Event: "issues", Action: "opened", SenderLogin: "autophage[bot]", Payload: body, ReceivedAt: t0}
	_, _ = st.StoreDelivery(ctx, own)
	deliver(t, st, "d-ping", "ping", "ping.json")
	_ = tr.ProcessPending(ctx)
	if r, d := processing(t, st, "d-own"); r != "ignored" || d != "own event" {
		t.Errorf("own = %s %q", r, d)
	}
	if r, _ := processing(t, st, "d-ping"); r != "ignored" {
		t.Errorf("ping = %s", r)
	}
}

// TestLabelOnExistingCaseApproves proves the label path is not derailed by
// the case already existing. The create it attempts first comes back as a
// conflict, which means the case is there, so the approval must still land
// rather than the delivery being recorded as ignored.
func TestLabelOnExistingCaseApproves(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	tr := newTranslator(st)
	deliver(t, st, "d-inst", "installation", "installation_created.json")
	if err := tr.ProcessPending(ctx); err != nil {
		t.Fatal(err)
	}
	req, _ := resolution.NewRequester("guy", resolution.AssociationOwner)
	c, _ := resolution.NewCase("guy/repo", 7, req, t0)
	if err := st.CreateCase(ctx, c); err != nil {
		t.Fatal(err)
	}
	deliver(t, st, "d-label", "issues", "issues_labeled.json")
	if err := tr.ProcessPending(ctx); err != nil {
		t.Fatal(err)
	}
	if r, d := processing(t, st, "d-label"); r != "translated" {
		t.Errorf("label on an existing case = %s %q, want translated", r, d)
	}
	got, err := st.GetCase(ctx, "guy/repo", 7)
	if err != nil || len(got.Approvals()) != 1 || got.Approvals()[0].DeliveryID != "d-label" {
		t.Errorf("approvals = %+v %v", got.Approvals(), err)
	}
}

func TestLabelBeforeOpenCreatesThenApproves(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	tr := newTranslator(st)
	deliver(t, st, "d-inst", "installation", "installation_created.json")
	deliver(t, st, "d-label", "issues", "issues_labeled.json")
	if err := tr.ProcessPending(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := st.GetCase(ctx, "guy/repo", 7)
	if err != nil || c.State() != resolution.Received || len(c.Approvals()) != 1 {
		t.Errorf("case = %+v %v", c, err)
	}
}
