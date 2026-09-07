package app

import (
	"testing"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/storetest"
)

// TestEnrollmentRefreshesDefaultBranch proves the placeholder branch an
// installation delivery leaves behind is corrected. The payloads carry no
// default branch, so enrollment records "main"; a repository whose real
// default is trunk would have every attempt rebased onto a branch that does
// not exist.
func TestEnrollmentRefreshesDefaultBranch(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	r, _ := resolution.NewRepository("guy/repo", 42, "main", t0)
	if err := st.EnrollRepository(ctx, r); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{issues: map[string]resolution.IssueDetail{}, branch: "trunk"}
	e := &Enrollment{Store: st, GitHub: gh, Label: "approved"}
	if err := e.Run(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetRepository(ctx, "guy/repo")
	if err != nil || got.DefaultBranch != "trunk" {
		t.Fatalf("default branch = %q %v, want trunk", got.DefaultBranch, err)
	}
	if got.InstallationID != 42 || !got.Enrolled() {
		t.Errorf("refresh disturbed the rest of the row: %+v", got)
	}
	if labels := gh.ensuredLabels(); len(labels) != 1 || labels[0] != "guy/repo:approved" {
		t.Errorf("labels = %v", labels)
	}
	// A second sweep has nothing left to do: the label setup row takes the
	// repository off the scan.
	if err := e.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if labels := gh.ensuredLabels(); len(labels) != 1 {
		t.Errorf("label ensured again: %v", labels)
	}
}

// TestEnrollmentDropsAReadOnlyRepository proves an archived repository, whose
// label call fails with a permanent 403, is recorded as removed instead of
// retried on every sweep. The first live installation covered every
// repository the operator owns, archived ones included, and one of them
// held the enrollment step for minutes per sweep.
func TestEnrollmentDropsAReadOnlyRepository(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	r, _ := resolution.NewRepository("guy/archived", 42, "main", t0)
	if err := st.EnrollRepository(ctx, r); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{issues: map[string]resolution.IssueDetail{}, labelErr: resolution.ErrRepositoryReadOnly}
	e := &Enrollment{Store: st, GitHub: gh, Label: "approved", Clock: fixedClock{t0}}
	if err := e.Run(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetRepository(ctx, "guy/archived")
	if err != nil {
		t.Fatal(err)
	}
	if got.Enrolled() {
		t.Fatalf("read only repository still enrolled: %+v", got)
	}
	if err := e.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if labels := gh.ensuredLabels(); len(labels) != 1 {
		t.Errorf("label retried on a removed repository: %v", labels)
	}
}
