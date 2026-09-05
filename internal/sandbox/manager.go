package sandbox

import "context"

var _ Sandbox = (*Manager)(nil)

// Prepare puts the case branch in place on host disk and warms dependencies
// in the networked prep container.
func (m *Manager) Prepare(ctx context.Context, repository, cloneURL, branch, defaultBranch, token string) (Workspace, error) {
	ws, err := m.checkout(ctx, repository, cloneURL, branch, defaultBranch, token)
	if err != nil {
		return Workspace{}, err
	}
	if err := m.warm(ctx, ws); err != nil {
		return Workspace{}, err
	}
	return ws, nil
}
