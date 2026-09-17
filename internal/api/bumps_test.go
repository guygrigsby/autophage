package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/auth"
	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
	"github.com/guygrigsby/autophage/internal/storetest"
	"github.com/guygrigsby/autophage/internal/upkeep"
)

const apiHead = "1111111111111111111111111111111111111111"

// bumpServer is newServer with the Upkeep half wired, including a StopRepair
// that always reports it stopped something.
func bumpServer(t *testing.T, st *store.Store) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	h := New(dir, nil, Deps{Store: st, GitHub: &fakeGitHub{}, Clock: fixedClock{t0}, OperatorLogin: "guy",
		Version: "test", StartedAt: t0, Concurrency: 2,
		Stop:       func(string) bool { return true },
		StopRepair: func(string) bool { return true },
		Sweep:      func(context.Context) {}})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tok, err := auth.Mint(dir)
	if err != nil {
		t.Fatal(err)
	}
	return srv, tok
}

func seedBump(t *testing.T, st *store.Store) *upkeep.Bump {
	t.Helper()
	ctx := t.Context()
	repo, _ := resolution.NewRepository("guy/repo", 42, "main", t0)
	if err := st.EnrollRepository(ctx, repo); err != nil {
		t.Fatal(err)
	}
	w, _ := upkeep.NewWatch("guy/repo", t0)
	if err := st.WatchRepository(ctx, w); err != nil {
		t.Fatal(err)
	}
	b, err := upkeep.NewBump("guy/repo", 11, "dependabot/go_modules/y", "main", apiHead, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateBump(ctx, b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWatchIsIdempotentAndUnwatchLeavesTheBumps(t *testing.T) {
	st := storetest.Open(t)
	seedBump(t, st)
	srv, tok := bumpServer(t, st)

	code, body := do(t, srv, tok, "POST", "/api/repositories/guy/repo/watch")
	if code != http.StatusOK {
		t.Fatalf("watch = %d %v", code, body)
	}
	first := body["watched_at"]
	code, body = do(t, srv, tok, "POST", "/api/repositories/guy/repo/watch")
	if code != http.StatusOK || body["watched_at"] != first {
		t.Errorf("a repeated watch moved watched_at: %v then %v", first, body["watched_at"])
	}
	code, _ = do(t, srv, tok, "POST", "/api/repositories/guy/repo/unwatch")
	if code != http.StatusOK {
		t.Fatalf("unwatch = %d", code)
	}
	// Idempotent both ways: deleting a watch that is gone is not an error.
	if code, _ := do(t, srv, tok, "POST", "/api/repositories/guy/repo/unwatch"); code != http.StatusOK {
		t.Errorf("second unwatch = %d", code)
	}
	if code, _ := do(t, srv, tok, "GET", "/api/bumps/guy/repo/11"); code != http.StatusOK {
		t.Errorf("the bump went away with the watch: %d", code)
	}
}

func TestWatchRefusesAnUnenrolledRepository(t *testing.T) {
	st := storetest.Open(t)
	srv, tok := bumpServer(t, st)
	if code, _ := do(t, srv, tok, "POST", "/api/repositories/nobody/nothing/watch"); code != http.StatusNotFound {
		t.Errorf("watch = %d, want 404", code)
	}
}

func TestListAndGetBump(t *testing.T) {
	st := storetest.Open(t)
	seedBump(t, st)
	srv, tok := bumpServer(t, st)

	code, body := do(t, srv, tok, "GET", "/api/bumps")
	if code != http.StatusOK {
		t.Fatalf("list = %d %v", code, body)
	}
	bumps, _ := body["bumps"].([]any)
	if len(bumps) != 1 {
		t.Fatalf("bumps = %v", body["bumps"])
	}
	row, _ := bumps[0].(map[string]any)
	if row["branch"] != "dependabot/go_modules/y" || row["state"] != "awaiting_checks" {
		t.Errorf("row = %v", row)
	}
	if row["latest_conclusion"] != "none" {
		t.Errorf("latest_conclusion = %v, want none before the checks report", row["latest_conclusion"])
	}

	code, body = do(t, srv, tok, "GET", "/api/bumps/guy/repo/11")
	if code != http.StatusOK {
		t.Fatalf("get = %d %v", code, body)
	}
	if body["head_sha"] != apiHead || body["base_branch"] != "main" {
		t.Errorf("bump = %v", body)
	}
	if _, ok := body["abandonment"]; ok {
		t.Error("a live bump carries an abandonment key")
	}
}

func TestListBumpsRejectsAnUnknownState(t *testing.T) {
	st := storetest.Open(t)
	srv, tok := bumpServer(t, st)
	if code, _ := do(t, srv, tok, "GET", "/api/bumps?state=vibes"); code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", code)
	}
}

func TestStatusCarriesTheUpkeepHalfEvenWithNoBumps(t *testing.T) {
	st := storetest.Open(t)
	srv, tok := bumpServer(t, st)
	code, body := do(t, srv, tok, "GET", "/api/status")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	for _, key := range []string{"bumps_by_state", "repairing", "repair_queue_depth", "watched_repositories"} {
		if _, ok := body[key]; !ok {
			t.Errorf("status has no %q: an operator cannot tell off from broken", key)
		}
	}
	counts, _ := body["bumps_by_state"].(map[string]any)
	if len(counts) != len(upkeep.AllBumpStates()) {
		t.Errorf("bumps_by_state = %v, want every state present as zero", counts)
	}
}

// Retry is the only thing that undoes an abandonment, and the round cap has
// to let the bump run again afterwards.
func TestRetryUndoesAnAbandonment(t *testing.T) {
	st := storetest.Open(t)
	seedBump(t, st)
	ctx := t.Context()
	if _, err := st.UpdateBump(ctx, "guy/repo", 11, func(b *upkeep.Bump) error {
		return b.Abandon(upkeep.BumpAbandonment{Reason: upkeep.AbandonChecksNeverConcluded, Detail: "no rollup in 6h", AbandonedAt: t0.Add(6 * time.Hour)})
	}); err != nil {
		t.Fatal(err)
	}
	srv, tok := bumpServer(t, st)
	code, body := do(t, srv, tok, "POST", "/api/bumps/guy/repo/11/retry")
	if code != http.StatusAccepted {
		t.Fatalf("retry = %d %v", code, body)
	}
	if body["state"] != "queued" {
		t.Errorf("state = %v, want queued", body["state"])
	}
	b, err := st.GetBump(ctx, "guy/repo", 11)
	if err != nil {
		t.Fatal(err)
	}
	if b.Abandonment() != nil {
		t.Error("the abandonment row survived the retry")
	}
}

// Retrying a bump whose checks have not concluded is repairing without
// knowing what is broken, so the aggregate refuses and the API says conflict.
func TestRetryOnALiveBumpIsAConflict(t *testing.T) {
	st := storetest.Open(t)
	seedBump(t, st)
	srv, tok := bumpServer(t, st)
	if code, _ := do(t, srv, tok, "POST", "/api/bumps/guy/repo/11/retry"); code != http.StatusConflict {
		t.Errorf("code = %d, want 409", code)
	}
}

func TestStopRepairOnABumpWithNoRoundIsAConflict(t *testing.T) {
	st := storetest.Open(t)
	seedBump(t, st)
	srv, tok := bumpServer(t, st)
	if code, _ := do(t, srv, tok, "POST", "/api/repairs/00000000-0000-0000-0000-000000000000/stop"); code != http.StatusNotFound {
		t.Errorf("code = %d, want 404 for a round that does not exist", code)
	}
}

func TestBumpEndpointsNeedTheBearer(t *testing.T) {
	st := storetest.Open(t)
	seedBump(t, st)
	srv, _ := bumpServer(t, st)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/bumps"},
		{"GET", "/api/bumps/guy/repo/11"},
		{"POST", "/api/repositories/guy/repo/watch"},
		{"POST", "/api/repositories/guy/repo/unwatch"},
		{"POST", "/api/bumps/guy/repo/11/retry"},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s unauthenticated = %d, want 401", tc.method, tc.path, resp.StatusCode)
		}
	}
}
