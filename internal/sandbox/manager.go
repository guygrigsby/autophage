package sandbox

import (
	"context"
	"fmt"
)

var _ Sandbox = (*Manager)(nil)

// Prepare sweeps away any container still holding this repository's
// workspace, acquires the repository's workspace lock, puts the case branch
// in place on host disk and warms dependencies in the networked prep
// container. On its own failure it releases the lock it acquired, since
// nothing further down the pipeline (Start, Teardown) will run to release
// it otherwise.
//
// The sweep comes before the lock, and always before the first host git
// command. The in-process lock is empty after a daemon restart and a
// Teardown whose removal failed still unlocks, so it cannot by itself prove
// no container is on /work: podman containers belong to conmon, not to this
// process, and one left looping "cp evil .git/config" would be racing host
// git with the installation token in scope. Fails closed on any listing or
// removal error.
func (m *Manager) Prepare(ctx context.Context, repository, cloneURL, branch, defaultBranch, token string) (Workspace, error) {
	if err := m.sweepContainers(ctx, repository); err != nil {
		return Workspace{}, err
	}
	held, err := m.lock(ctx, repository)
	if err != nil {
		return Workspace{}, fmt.Errorf("acquire workspace lock for %s: %w", repository, err)
	}
	ws, err := m.checkout(ctx, repository, cloneURL, branch, defaultBranch, token)
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
