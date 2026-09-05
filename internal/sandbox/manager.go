package sandbox

import (
	"context"
	"fmt"
)

var _ Sandbox = (*Manager)(nil)

// Prepare acquires the repository's workspace lock, sweeps away any
// container still holding that workspace, puts the case branch in place on
// host disk and warms dependencies in the networked prep container. On its
// own failure it releases the lock it acquired, since nothing further down
// the pipeline (Start, Teardown) will run to release it otherwise.
//
// The sweep runs under the lock and always before the first host git
// command. Under the lock, because any container this repository legitimately
// has running belongs to an attempt that holds the lock, and sweeping outside
// it would tear down a sibling attempt mid-run. Before git, because the lock
// alone cannot prove no container is on /work: it is empty after a daemon
// restart and a Teardown whose removal failed unlocks anyway, and podman
// containers belong to conmon rather than to this process, so one left
// looping "cp evil .git/config" would be racing host git with the
// installation token in scope. Fails closed on any listing or removal error.
func (m *Manager) Prepare(ctx context.Context, repository, cloneURL, branch, defaultBranch, token string) (Workspace, error) {
	held, err := m.lock(ctx, repository)
	if err != nil {
		return Workspace{}, fmt.Errorf("acquire workspace lock for %s: %w", repository, err)
	}
	if err := m.sweepContainers(ctx, repository); err != nil {
		m.unlock(repository, held)
		return Workspace{}, err
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
