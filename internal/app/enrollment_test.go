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
