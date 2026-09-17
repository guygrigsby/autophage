package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// PrepareHead is Prepare for a branch autophage did not create: it pins the
// workspace to one exact head sha instead of replaying the branch onto the
// default branch. A dependabot bump is judged by CI at a particular sha, so
// rebasing before the repair would start the round from a tree no verdict
// describes, and the repair would be fixing something other than what broke.
//
// Everything else matches Prepare: the same workspace lock, the same
// container sweep under it and before any host git, the same prep-container
// warm, and the same release-on-own-failure.
func (m *Manager) PrepareHead(ctx context.Context, repository, cloneURL, branch, headSha, token string) (Workspace, error) {
	// Checked before anything else so a malformed sha can never reach git as
	// an argument, where a leading dash is an option rather than a revision.
	if _, err := validateSha(headSha); err != nil {
		return Workspace{}, fmt.Errorf("head sha for %s: %w", repository, err)
	}
	held, err := m.lock(ctx, repository)
	if err != nil {
		return Workspace{}, fmt.Errorf("acquire workspace lock for %s: %w", repository, err)
	}
	if err := m.sweepContainers(ctx, repository); err != nil {
		m.unlock(repository, held)
		return Workspace{}, err
	}
	ws, err := m.checkoutHead(ctx, repository, cloneURL, branch, headSha, token)
	if err != nil {
		m.unlock(repository, held)
		return Workspace{}, err
	}
	ws.lease = held
	if err := m.warm(ctx, ws); err != nil {
		m.unlock(repository, held)
		return Workspace{}, err
	}
	return ws, nil
}

// checkoutHead clones on first use, otherwise fetches, then puts branch at
// exactly headSha with no rebase and no merge. The sha must be reachable
// from the branch on the remote: a repair works the head CI judged, and a
// sha the fetch did not bring is either a typo or a head that has already
// been replaced, neither of which should silently become a checkout of
// something else.
//
// BaseSha and RemoteHead are both headSha. BaseSha is what ConflictMarkers
// and DiffLines measure this round's own work against, and RemoteHead is the
// lease CommitAndPush pushes under, so a dependabot force-push landing
// mid-round makes the push fail rather than clobber it.
func (m *Manager) checkoutHead(ctx context.Context, repository, cloneURL, branch, headSha, token string) (Workspace, error) {
	path, err := m.workspacePath(repository)
	if err != nil {
		return Workspace{}, err
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return Workspace{}, err
		}
		if _, err := m.gitRemote(ctx, filepath.Dir(path), token, cloneURL, cloneTimeout,
			"clone", "--no-tags", "--branch", branch, cloneURL, path); err != nil {
			return Workspace{}, fmt.Errorf("clone %s: %w", repository, err)
		}
	} else {
		// A previous round's container may have rewritten this config; never
		// trust it before running host git again.
		if err := resetGitConfig(path, cloneURL); err != nil {
			return Workspace{}, fmt.Errorf("reset git config for %s: %w", repository, err)
		}
	}
	// Fetched unconditionally, including right after a clone: the clone
	// brought the branch at whatever the remote said a moment ago, and the
	// round is only allowed to work the sha it was told about.
	if _, err := m.gitRemote(ctx, path, token, cloneURL, gitTimeout,
		"fetch", "--prune", cloneURL, "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return Workspace{}, fmt.Errorf("fetch %s: %w", repository, err)
	}
	_, _ = m.git(ctx, path, "rebase", "--abort")
	_, _ = m.git(ctx, path, "merge", "--abort")
	if _, err := m.git(ctx, path, "reset", "--hard"); err != nil {
		return Workspace{}, err
	}
	if _, err := m.git(ctx, path, "clean", "-fdx"); err != nil {
		return Workspace{}, err
	}
	// The head must be what the branch actually points at. Checking the ref
	// rather than merely that the object exists is what stops a round working
	// a sha that a force-push has already replaced.
	branchRef := "refs/remotes/origin/" + branch
	remoteOut, err := m.gitOut(ctx, path, "rev-parse", "--verify", "--quiet", branchRef)
	if err != nil {
		return Workspace{}, fmt.Errorf("branch %s is not on the remote: %w", branch, err)
	}
	remoteHead, err := validateSha(remoteOut)
	if err != nil {
		return Workspace{}, fmt.Errorf("remote head for %s: %w", branch, err)
	}
	if remoteHead != headSha {
		return Workspace{}, fmt.Errorf("branch %s is at %s, not the requested head %s", branch, remoteHead, headSha)
	}
	if _, err := m.git(ctx, path, "checkout", "-q", "-B", branch, headSha); err != nil {
		return Workspace{}, err
	}
	return Workspace{
		Path:       path,
		Repository: repository,
		CloneURL:   cloneURL,
		Branch:     branch,
		// DefaultBranch is deliberately empty: nothing in a repair reads it,
		// and carrying it would invite a rebase back in.
		BaseSha:    headSha,
		RemoteHead: headSha,
	}, nil
}
