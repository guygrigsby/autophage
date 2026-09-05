package resolution

import (
	"strings"
	"time"
)

// Repository is an enrolled GitHub repository. InstallationID is the opaque
// GitHub App installation reference used to mint tokens.
type Repository struct {
	FullName       string
	InstallationID int64
	DefaultBranch  string
	EnrolledAt     time.Time
	Removed        *Removal
}

// Removal exists while the repository is removed from the installation.
type Removal struct {
	RemovedAt time.Time
}

func NewRepository(fullName string, installationID int64, defaultBranch string, at time.Time) (Repository, error) {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Repository{}, Invalid("repository full name %q", fullName)
	}
	if installationID <= 0 {
		return Repository{}, Invalid("installation id %d", installationID)
	}
	if defaultBranch == "" {
		return Repository{}, Invalid("default branch is empty")
	}
	return Repository{FullName: fullName, InstallationID: installationID, DefaultBranch: defaultBranch, EnrolledAt: at}, nil
}

// Enrolled reports whether autophage may act on the repository.
func (r Repository) Enrolled() bool { return r.Removed == nil }

// OwnerName splits owner/name.
func (r Repository) OwnerName() (owner, name string) {
	owner, name, _ = strings.Cut(r.FullName, "/")
	return owner, name
}
