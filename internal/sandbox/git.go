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

// authEnvVar names the environment variable a per-command --config-env
// passes the GitHub auth header through. Never argv, never a repository's
// own config: see gitArgs and resetGitConfig.
const authEnvVar = "AUTOPHAGE_GIT_AUTH"

// gitArgs prefixes every git invocation with the identity, the hooks and
// fsmonitor overrides, and, when withAuth is set, a --config-env that scopes
// the auth header to https://github.com/ only and reads its value from
// authEnvVar rather than argv. Nothing here is ever written to the
// repository's own config.
func (m *Manager) gitArgs(withAuth bool, args ...string) []string {
	base := []string{
		"-c", "user.name=" + m.BotName,
		"-c", "user.email=" + m.BotEmail,
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
	}
	if withAuth {
		base = append(base, "--config-env=http.https://github.com/.extraheader="+authEnvVar)
	}
	return append(base, args...)
}

// gitEnv builds the environment for one git invocation: no global or system
// config contributes (GIT_CONFIG_GLOBAL/GIT_CONFIG_NOSYSTEM), no terminal
// prompt, and, when token is set, the auth header value in authEnvVar. auth
// is returned alongside so the caller can scrub it from output afterward.
func gitEnv(token string) (env []string, auth string) {
	env = []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	if token != "" {
		auth = base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		env = append(env, authEnvVar+"=AUTHORIZATION: basic "+auth)
	}
	return env, auth
}

// redactAuth scrubs auth (the base64 credential, not just the argv form of
// it) from s and from err's text, in case it ever surfaces in git's own
// output or an error message rather than staying confined to the env var.
func redactAuth(s string, err error, auth string) (string, error) {
	if auth == "" {
		return s, err
	}
	s = strings.ReplaceAll(s, auth, "<redacted>")
	if err != nil {
		err = fmt.Errorf("%s", strings.ReplaceAll(err.Error(), auth, "<redacted>"))
	}
	return s, err
}

// git runs one git command with combined output. Use gitOut instead when
// the output must be parsed.
func (m *Manager) git(ctx context.Context, dir, token string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	env, auth := gitEnv(token)
	s, err := run(ctx, dir, env, "git", m.gitArgs(token != "", args...)...)
	return redactAuth(s, err, auth)
}

// gitOut runs one git command and returns stdout alone, for callers that
// parse the result (a sha).
func (m *Manager) gitOut(ctx context.Context, dir, token string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	env, auth := gitEnv(token)
	s, err := out(ctx, dir, env, "git", m.gitArgs(token != "", args...)...)
	return redactAuth(s, err, auth)
}

// workspacePath validates repository and builds its path under
// WorkspacesDir. repository must be exactly two non-empty segments
// (owner/name) with no ".." component, so it can never walk outside
// WorkspacesDir or collapse two different repositories onto one directory.
func (m *Manager) workspacePath(repository string) (string, error) {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("invalid repository %q: want exactly two non-empty segments owner/name", repository)
	}
	owner, name := parts[0], parts[1]
	if strings.Contains(owner, "..") || strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid repository %q: must not contain \"..\"", repository)
	}
	return filepath.Join(m.WorkspacesDir, owner, name), nil
}

// resetGitConfig discards whatever .git/config the workspace currently
// holds and rewrites it from a fixed, minimal template naming only the
// origin remote. /work is bind-mounted whole into the agent container, so a
// compromised agent can rewrite .git/config on disk: a credential helper,
// an sshCommand, a diff or filter driver, an insteadOf rewrite, or
// remote.origin.url itself, any of which would otherwise run on the host
// with the push token in scope the next time this package runs git. Nothing
// in the workspace's own config is ever trusted; this is called before any
// host git command that runs after a container may have touched the
// workspace. Refuses (rather than following) a .git that is not a real
// directory, since a symlink there could point the rewrite anywhere.
func resetGitConfig(path, cloneURL string) error {
	gitDir := filepath.Join(path, ".git")
	info, err := os.Lstat(gitDir)
	if err != nil {
		return fmt.Errorf("stat %s: %w", gitDir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("refusing workspace: %s is not a real directory", gitDir)
	}
	configPath := filepath.Join(gitDir, "config")
	if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", configPath, err)
	}
	template := fmt.Sprintf(`[core]
	repositoryformatversion = 0
	filemode = true
	bare = false
	logallrefupdates = true
[remote "origin"]
	url = %s
	fetch = +refs/heads/*:refs/remotes/origin/*
`, cloneURL)
	if err := os.WriteFile(configPath, []byte(template), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	return nil
}

// checkout clones on first use, otherwise fetches, then puts the case branch
// in place on top of the current default branch. A rebase conflict is left
// as merge conflict markers for the agent.
func (m *Manager) checkout(ctx context.Context, repository, cloneURL, branch, defaultBranch, token string) (Workspace, error) {
	path, err := m.workspacePath(repository)
	if err != nil {
		return Workspace{}, err
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return Workspace{}, err
		}
		if _, err := m.git(ctx, filepath.Dir(path), token, "clone", "--no-tags", "--branch", defaultBranch, cloneURL, path); err != nil {
			return Workspace{}, fmt.Errorf("clone %s: %w", repository, err)
		}
	} else {
		// A previous attempt's container may have rewritten this config;
		// never trust it before running host git again.
		if err := resetGitConfig(path, cloneURL); err != nil {
			return Workspace{}, fmt.Errorf("reset git config for %s: %w", repository, err)
		}
		if _, err := m.git(ctx, path, token, "-c", "remote.origin.url="+cloneURL, "fetch", "--prune", "origin"); err != nil {
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
	baseOut, err := m.gitOut(ctx, path, "", "rev-parse", "origin/"+defaultBranch)
	if err != nil {
		return Workspace{}, fmt.Errorf("default branch %s: %w", defaultBranch, err)
	}
	base, err := validateSha(baseOut)
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
	return Workspace{Path: path, Repository: repository, CloneURL: cloneURL, Branch: branch, DefaultBranch: defaultBranch, BaseSha: base}, nil
}

// CommitAndPush stages everything, commits when there is anything to commit
// and pushes the branch with force-with-lease. The first push of a fresh
// branch always happens so the branch exists remotely.
func (m *Manager) CommitAndPush(ctx context.Context, ws Workspace, token, message string) (string, bool, error) {
	// The container that just ran may have rewritten .git/config; never
	// trust it before pushing with the token in scope.
	if err := resetGitConfig(ws.Path, ws.CloneURL); err != nil {
		return "", false, fmt.Errorf("reset git config for %s: %w", ws.Repository, err)
	}
	if _, err := m.git(ctx, ws.Path, "", "add", "-A"); err != nil {
		return "", false, err
	}
	if _, err := m.git(ctx, ws.Path, "", "diff", "--cached", "--quiet"); err != nil {
		if _, err := m.git(ctx, ws.Path, "", "commit", "-q", "-m", message); err != nil {
			return "", false, err
		}
	}
	headOut, err := m.gitOut(ctx, ws.Path, "", "rev-parse", "HEAD")
	if err != nil {
		return "", false, err
	}
	head, err := validateSha(headOut)
	if err != nil {
		return "", false, err
	}
	if _, err := m.git(ctx, ws.Path, token, "-c", "remote.origin.url="+ws.CloneURL, "push", "--force-with-lease", "origin", "HEAD:refs/heads/"+ws.Branch); err != nil {
		return head, false, err
	}
	return head, true, nil
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}
