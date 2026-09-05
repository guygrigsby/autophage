package sandbox

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const gitTimeout = 5 * time.Minute

// gitArgs prefixes every git invocation with the identity, the hooks
// override and the auth header for this token. Nothing is written to the
// repository's config.
func (m *Manager) gitArgs(token string, args ...string) []string {
	base := []string{
		"-c", "user.name=" + m.BotName,
		"-c", "user.email=" + m.BotEmail,
		"-c", "core.hooksPath=/dev/null",
	}
	if token != "" {
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		base = append(base, "-c", "http.extraheader=AUTHORIZATION: basic "+auth)
	}
	return append(base, args...)
}

func (m *Manager) git(ctx context.Context, dir, token string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	return run(ctx, dir, []string{"GIT_TERMINAL_PROMPT=0"}, "git", m.gitArgs(token, args...)...)
}

func (m *Manager) workspacePath(repository string) string {
	owner, name, _ := strings.Cut(repository, "/")
	return filepath.Join(m.WorkspacesDir, owner, name)
}

// checkout clones on first use, otherwise fetches, then puts the case branch
// in place on top of the current default branch. A rebase conflict is left
// as merge conflict markers for the agent.
func (m *Manager) checkout(ctx context.Context, repository, cloneURL, branch, defaultBranch, token string) (Workspace, error) {
	path := m.workspacePath(repository)
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return Workspace{}, err
		}
		if _, err := m.git(ctx, filepath.Dir(path), token, "clone", "--no-tags", "--branch", defaultBranch, cloneURL, path); err != nil {
			return Workspace{}, fmt.Errorf("clone %s: %w", repository, err)
		}
	} else {
		if _, err := m.git(ctx, path, token, "fetch", "--prune", "origin"); err != nil {
			return Workspace{}, fmt.Errorf("fetch %s: %w", repository, err)
		}
		_, _ = m.git(ctx, path, "", "rebase", "--abort")
		_, _ = m.git(ctx, path, "", "merge", "--abort")
		if _, err := m.git(ctx, path, "", "reset", "--hard"); err != nil {
			return Workspace{}, err
		}
		if _, err := m.git(ctx, path, "", "clean", "-fdx"); err != nil {
			return Workspace{}, err
		}
	}
	base, err := m.git(ctx, path, "", "rev-parse", "origin/"+defaultBranch)
	if err != nil {
		return Workspace{}, fmt.Errorf("default branch %s: %w", defaultBranch, err)
	}
	start := "origin/" + defaultBranch
	if _, err := m.git(ctx, path, "", "rev-parse", "--verify", "--quiet", "origin/"+branch); err == nil {
		start = "origin/" + branch
	}
	if _, err := m.git(ctx, path, "", "checkout", "-q", "-B", branch, start); err != nil {
		return Workspace{}, err
	}
	if start != "origin/"+defaultBranch {
		if _, err := m.git(ctx, path, "", "rebase", "origin/"+defaultBranch); err != nil {
			m.logf("rebase %s onto %s conflicted; leaving markers for the agent", branch, defaultBranch)
			_, _ = m.git(ctx, path, "", "rebase", "--abort")
			_, _ = m.git(ctx, path, "", "merge", "--no-commit", "--no-ff", "origin/"+defaultBranch)
		}
	}
	return Workspace{Path: path, Repository: repository, Branch: branch, DefaultBranch: defaultBranch, BaseSha: base}, nil
}

// CommitAndPush stages everything, commits when there is anything to
// commit, and pushes the branch with force-with-lease. The first push of a
// fresh branch always happens so the branch exists remotely.
func (m *Manager) CommitAndPush(ctx context.Context, ws Workspace, token, message string) (string, bool, error) {
	if _, err := m.git(ctx, ws.Path, "", "add", "-A"); err != nil {
		return "", false, err
	}
	if _, err := m.git(ctx, ws.Path, "", "diff", "--cached", "--quiet"); err != nil {
		if _, err := m.git(ctx, ws.Path, "", "commit", "-q", "-m", message); err != nil {
			return "", false, err
		}
	}
	head, err := m.git(ctx, ws.Path, "", "rev-parse", "HEAD")
	if err != nil {
		return "", false, err
	}
	if _, err := m.git(ctx, ws.Path, token, "push", "--force-with-lease", "origin", "HEAD:refs/heads/"+ws.Branch); err != nil {
		return head, false, err
	}
	return head, true, nil
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}
