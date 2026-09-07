package app

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
)

// Enrollment finishes what a webhook enrollment could not: it reads the
// repository's real default branch and ensures the approval label exists.
// Both run once per enrollment, driven by the same scan, so a repository
// already set up costs no GitHub calls on later sweeps.
type Enrollment struct {
	Store  *store.Store
	GitHub resolution.GitHub
	Label  string
	Clock  resolution.Clock
}

// Run walks every enrolled repository with no label setup recorded. A
// failure on one is logged and retried on the next sweep; the label call is
// idempotent on GitHub's side and the branch refresh is a no-op when it
// already matches.
func (e *Enrollment) Run(ctx context.Context) error {
	repos, err := e.Store.RepositoriesNeedingLabel(ctx)
	if err != nil {
		return err
	}
	for _, r := range repos {
		// Branch first: it and EnsureLabel fail together when GitHub is
		// unreachable, and recording the label setup is what takes the
		// repository off this list, so doing it last means a branch that
		// could not be read is tried again rather than left as the
		// enrollment's placeholder for ever.
		if err := e.refreshBranch(ctx, r); err != nil {
			log.Printf("default branch for %s: %v (will retry)", r.FullName, err)
			continue
		}
		if err := e.GitHub.EnsureLabel(ctx, r.FullName, e.Label); err != nil {
			if errors.Is(err, resolution.ErrRepositoryReadOnly) {
				// Archived: nothing autophage does there can land, so the
				// repository leaves the sweep the way an uninstall would.
				if rerr := e.Store.RemoveRepository(ctx, r.FullName, e.now()); rerr != nil {
					log.Printf("remove read only %s: %v (will retry)", r.FullName, rerr)
					continue
				}
				log.Printf("enrollment %s: read only, removed", r.FullName)
				continue
			}
			log.Printf("label %s on %s: %v (will retry)", e.Label, r.FullName, err)
			continue
		}
		if err := e.Store.RecordLabelSetup(ctx, r.FullName, e.Label); err != nil {
			log.Printf("record label setup %s: %v (will retry)", r.FullName, err)
			continue
		}
	}
	return nil
}

// refreshBranch rewrites default_branch when GitHub disagrees with what
// enrollment guessed. EnrollRepository is the one way in, so re-enrolling
// with the corrected branch is the write.
func (e *Enrollment) refreshBranch(ctx context.Context, r resolution.Repository) error {
	branch, err := e.GitHub.DefaultBranch(ctx, r)
	if err != nil {
		return err
	}
	if branch == "" || branch == r.DefaultBranch {
		return nil
	}
	fresh, err := resolution.NewRepository(r.FullName, r.InstallationID, branch, r.EnrolledAt)
	if err != nil {
		return err
	}
	log.Printf("enrollment %s: default branch %s, was %s", r.FullName, branch, r.DefaultBranch)
	return e.Store.EnrollRepository(ctx, fresh)
}

// now tolerates an unset Clock so a test that wires only the store and the
// label keeps working.
func (e *Enrollment) now() time.Time {
	if e.Clock == nil {
		return time.Now().UTC()
	}
	return e.Clock.Now()
}
