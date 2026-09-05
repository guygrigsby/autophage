package sandbox

import (
	"context"
	"fmt"
)

var _ Sandbox = (*Manager)(nil)

// Prepare acquires the repository's workspace lock, puts the case branch in
// place on host disk and warms dependencies in the networked prep
// container. On its own failure it releases the lock it acquired, since
// nothing further down the pipeline (Start, Teardown) will run to release
// it otherwise.
func (m *Manager) Prepare(ctx context.Context, repository, cloneURL, branch, defaultBranch, token string) (Workspace, error) {
	if err := m.lock(ctx, repository); err != nil {
		return Workspace{}, fmt.Errorf("acquire workspace lock for %s: %w", repository, err)
	}
	ws, err := m.checkout(ctx, repository, cloneURL, branch, defaultBranch, token)
	if err != nil {
		m.unlock(repository)
		return Workspace{}, err
	}
	if err := m.warm(ctx, ws); err != nil {
		m.unlock(repository)
		return Workspace{}, err
	}
	return ws, nil
}
