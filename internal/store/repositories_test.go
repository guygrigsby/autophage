package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
	"github.com/guygrigsby/autophage/internal/storetest"
)

var t0 = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

func TestEnrollGetRemoveReenroll(t *testing.T) {
	s := storetest.Open(t)
	ctx := t.Context()
	r, _ := resolution.NewRepository("guy/repo", 42, "main", t0)
	if err := s.EnrollRepository(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRepository(ctx, "guy/repo")
	if err != nil || got.InstallationID != 42 || got.DefaultBranch != "main" || !got.Enrolled() {
		t.Fatalf("get = %+v %v", got, err)
	}
	if _, err := s.GetRepository(ctx, "guy/nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing = %v", err)
	}
	if err := s.RemoveRepository(ctx, "guy/repo", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetRepository(ctx, "guy/repo")
	if got.Enrolled() || got.Removed == nil || !got.Removed.RemovedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("after remove = %+v", got)
	}
	r2, _ := resolution.NewRepository("guy/repo", 43, "trunk", t0.Add(2*time.Hour))
	if err := s.EnrollRepository(ctx, r2); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetRepository(ctx, "guy/repo")
	if !got.Enrolled() || got.InstallationID != 43 || got.DefaultBranch != "trunk" {
		t.Errorf("after re-enroll = %+v", got)
	}
}

func TestRepositoriesNeedingLabel(t *testing.T) {
	s := storetest.Open(t)
	ctx := t.Context()
	a, _ := resolution.NewRepository("guy/a", 1, "main", t0)
	b, _ := resolution.NewRepository("guy/b", 1, "main", t0)
	c, _ := resolution.NewRepository("guy/c", 1, "main", t0)
	for _, r := range []resolution.Repository{a, b, c} {
		if err := s.EnrollRepository(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordLabelSetup(ctx, "guy/a", "approved"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRepository(ctx, "guy/c", t0); err != nil {
		t.Fatal(err)
	}
	need, err := s.RepositoriesNeedingLabel(ctx)
	if err != nil || len(need) != 1 || need[0].FullName != "guy/b" {
		t.Errorf("need = %+v %v", need, err)
	}
}
