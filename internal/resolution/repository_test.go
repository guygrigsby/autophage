package resolution

import (
	"errors"
	"testing"
	"time"
)

func TestNewRepositoryRefusals(t *testing.T) {
	type testCase struct {
		name           string
		fullName       string
		installationID int64
		defaultBranch  string
		expectInvalid  bool
	}
	tests := []testCase{
		{"no slash", "repo", 42, "main", true},
		{"empty owner", "/repo", 42, "main", true},
		{"empty name", "guy/", 42, "main", true},
		{"extra slash", "guy/repo/extra", 42, "main", true},
		{"installation id zero", "guy/repo", 0, "main", true},
		{"installation id negative", "guy/repo", -1, "main", true},
		{"empty default branch", "guy/repo", 42, "", true},
		{"happy path", "guy/repo", 42, "main", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := NewRepository(tt.fullName, tt.installationID, tt.defaultBranch, t0)
			if tt.expectInvalid {
				if err == nil {
					t.Errorf("NewRepository(%q, %d, %q) succeeded, want error", tt.fullName, tt.installationID, tt.defaultBranch)
				}
				if !errors.Is(err, ErrInvalid) {
					t.Errorf("error = %v, want ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Errorf("NewRepository(%q, %d, %q) failed: %v", tt.fullName, tt.installationID, tt.defaultBranch, err)
			}
			if r.FullName != tt.fullName || r.InstallationID != tt.installationID || r.DefaultBranch != tt.defaultBranch {
				t.Errorf("repository fields: %+v", r)
			}
		})
	}
}

func TestRepositoryOwnerName(t *testing.T) {
	r, _ := NewRepository("guy/repo", 42, "main", t0)
	owner, name := r.OwnerName()
	if owner != "guy" || name != "repo" {
		t.Errorf("OwnerName() = (%q, %q), want (guy, repo)", owner, name)
	}
}

func TestRepositoryEnrolled(t *testing.T) {
	r, _ := NewRepository("guy/repo", 42, "main", t0)
	if !r.Enrolled() {
		t.Error("Enrolled() = false, want true when Removed is nil")
	}
	r.Removed = &Removal{RemovedAt: t0.Add(time.Hour)}
	if r.Enrolled() {
		t.Error("Enrolled() = true, want false when Removed is set")
	}
}
