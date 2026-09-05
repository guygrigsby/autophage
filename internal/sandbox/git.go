package sandbox

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	gitTimeout = 5 * time.Minute
	// cloneTimeout bounds the first clone of a repository. A clone pulls the
	// whole history over the network and can take far longer than any later
	// fetch, so sharing gitTimeout with the rest would abort a first attempt
	// on a large repository that was making perfectly good progress.
	cloneTimeout = 20 * time.Minute
)

// authEnvVar names the environment variable a per-command --config-env
// passes the GitHub auth header through. Never argv, never a repository's
// own config: see gitArgs and resetGitConfig.
const authEnvVar = "AUTOPHAGE_GIT_AUTH"

// hardeningArgs are the -c overrides every host git command carries. The
// workspace's .git/config is attacker-controlled: /work is bind-mounted
// whole into the agent container, and a container the daemon failed to
// remove (podman containers are owned by conmon, not by the daemon, so one
// can outlive a restart) may still be rewriting that file while host git
// runs. Every key git can turn into an exec surface is therefore pinned to a
// harmless value on the command line, which the file cannot outrank: the
// credential helper list (an empty value resets it), the ssh command, the
// askpass program, the git:// proxy command and the upload-pack hook.
// protocol.allow=never then refuses every transport except the ones the
// caller explicitly needs, which is what kills a hostile
// remote.origin.url = "ext::sh -c ...". resetGitConfig is the second layer,
// not the only one: it rewrites the file before these commands run, but it
// cannot win a race against a process that is still writing it.
func hardeningArgs(remote string) []string {
	args := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "credential.helper=",
		"-c", "core.sshCommand=",
		"-c", "core.askPass=",
		"-c", "core.gitProxy=",
		"-c", "uploadpack.packObjectsHook=",
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
	}
	if isLocalRemote(remote) {
		args = append(args, "-c", "protocol.file.allow=always")
	}
	return args
}

// isLocalRemote reports whether remote names a path on this machine or a
// file:// URL rather than a network URL. The file transport is enabled only
// for those (the tests' bare remotes): a clone URL minted for a GitHub
// installation is always https, so nothing in production ever needs it.
func isLocalRemote(remote string) bool {
	if remote == "" {
		return false
	}
	if strings.HasPrefix(remote, "file://") {
		return true
	}
	if strings.Contains(remote, "://") {
		return false
	}
	// scp-like syntax (host:path, no slash before the colon) is ssh.
	if i := strings.Index(remote, ":"); i >= 0 && !strings.Contains(remote[:i], "/") {
		return false
	}
	return true
}

// gitArgs prefixes every git invocation with the identity, the hardening
// overrides for remote (empty for a local-only command) and, when withAuth
// is set, a --config-env that scopes the auth header to https://github.com/
// only and reads its value from authEnvVar rather than argv. Nothing here is
// ever written to the repository's own config.
func (m *Manager) gitArgs(remote string, withAuth bool, args ...string) []string {
	base := []string{
		"-c", "user.name=" + m.BotName,
		"-c", "user.email=" + m.BotEmail,
	}
	base = append(base, hardeningArgs(remote)...)
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

// redactedError carries a secret-scrubbed message while still unwrapping to
// the real underlying error (an *exec.ExitError or the context error run or
// out wrapped exactly once via %w), so errors.Is and errors.As still see
// through a redacted git failure. Its own Error() is the redacted text;
// nothing here re-exposes the intermediate run/out wrapper, whose own
// Error() embeds the (pre-redaction) command line and output.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redactAuth scrubs auth (the base64 credential, not just the argv form of
// it) from s and from err's message, in case it ever surfaces in git's own
// output rather than staying confined to the env var. The returned error
// unwraps directly to the real *exec.ExitError or context error beneath
// run/out's own wrapper, never to that unredacted wrapper itself.
func redactAuth(s string, err error, auth string) (string, error) {
	if auth == "" {
		return s, err
	}
	s = strings.ReplaceAll(s, auth, "<redacted>")
	if err == nil {
		return s, nil
	}
	return s, &redactedError{
		msg: strings.ReplaceAll(err.Error(), auth, "<redacted>"),
		err: errors.Unwrap(err),
	}
}

// git runs one local git command with combined output: no token, no remote,
// no transport. Use gitOut instead when the output must be parsed, and
// gitRemote for anything that reaches the network.
func (m *Manager) git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	env, _ := gitEnv("")
	return run(ctx, dir, env, "git", m.gitArgs("", false, args...)...)
}

// gitOut runs one local git command and returns stdout alone, for callers
// that parse the result (a sha).
func (m *Manager) gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	env, _ := gitEnv("")
	return out(ctx, dir, env, "git", m.gitArgs("", false, args...)...)
}

// gitRemote runs the one class of git command that reaches the network, with
// the token in scope. remote is always passed positionally by the caller as
// well, never read from the workspace's config: a clone, fetch or push that
// resolved its URL through .git/config would be handing the installation
// token to whatever the agent last wrote there.
func (m *Manager) gitRemote(ctx context.Context, dir, token, remote string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	env, auth := gitEnv(token)
	s, err := run(ctx, dir, env, "git", m.gitArgs(remote, token != "", args...)...)
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
// workspace. It is the second layer behind hardeningArgs, which does not
// depend on the file's contents at all. Refuses (rather than following) a
// .git that is not a real directory, since a symlink there could point the
// rewrite anywhere.
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
// as merge conflict markers for the agent. Every ref this reads is named by
// its full path under refs/remotes/: the agent owns .git, so a branch called
// "origin/main" under refs/heads would otherwise shadow the remote-tracking
// ref that "origin/main" resolves to.
func (m *Manager) checkout(ctx context.Context, repository, cloneURL, branch, defaultBranch, token string) (Workspace, error) {
	path, err := m.workspacePath(repository)
	if err != nil {
		return Workspace{}, err
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return Workspace{}, err
		}
		if _, err := m.gitRemote(ctx, filepath.Dir(path), token, cloneURL, cloneTimeout,
			"clone", "--no-tags", "--branch", defaultBranch, cloneURL, path); err != nil {
			return Workspace{}, fmt.Errorf("clone %s: %w", repository, err)
		}
	} else {
		// A previous attempt's container may have rewritten this config;
		// never trust it before running host git again.
		if err := resetGitConfig(path, cloneURL); err != nil {
			return Workspace{}, fmt.Errorf("reset git config for %s: %w", repository, err)
		}
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
	}
	baseRef := "refs/remotes/origin/" + defaultBranch
	baseOut, err := m.gitOut(ctx, path, "rev-parse", baseRef)
	if err != nil {
		return Workspace{}, fmt.Errorf("default branch %s: %w", defaultBranch, err)
	}
	base, err := validateSha(baseOut)
	if err != nil {
		return Workspace{}, fmt.Errorf("default branch %s: %w", defaultBranch, err)
	}
	// The lease CommitAndPush pushes under is recorded here, from the fetch
	// this call just did, and never re-read at push time: by then the agent
	// has had a container on /work and could point
	// refs/remotes/origin/<branch> at anything it liked.
	branchRef := "refs/remotes/origin/" + branch
	remoteHead := ""
	if sha, err := m.gitOut(ctx, path, "rev-parse", "--verify", "--quiet", branchRef); err == nil {
		if remoteHead, err = validateSha(sha); err != nil {
			return Workspace{}, fmt.Errorf("remote head for %s: %w", branch, err)
		}
	}
	start := baseRef
	if remoteHead != "" {
		start = branchRef
	}
	if _, err := m.git(ctx, path, "checkout", "-q", "-B", branch, start); err != nil {
		return Workspace{}, err
	}
	if start != baseRef {
		if _, err := m.git(ctx, path, "rebase", baseRef); err != nil {
			m.logf("rebase %s onto %s conflicted; leaving markers for the agent", branch, defaultBranch)
			_, _ = m.git(ctx, path, "rebase", "--abort")
			_, _ = m.git(ctx, path, "merge", "--no-commit", "--no-ff", baseRef)
		}
	}
	return Workspace{
		Path:          path,
		Repository:    repository,
		CloneURL:      cloneURL,
		Branch:        branch,
		DefaultBranch: defaultBranch,
		BaseSha:       base,
		RemoteHead:    remoteHead,
	}, nil
}

// CommitAndPush ends the agent phase, stages everything, commits when there
// is anything to commit and pushes the branch under the lease Prepare
// recorded. The first push of a fresh branch carries an empty expected
// value, which git enforces as "this ref must not exist yet".
func (m *Manager) CommitAndPush(ctx context.Context, ws Workspace, token, message string) (string, bool, error) {
	// The runner's own order is Start, the agent's turns, CommitAndPush,
	// Teardown: this is the only point that can guarantee the container is
	// gone before host git runs. Fails closed rather than trusting the
	// workspace while the agent might still be rewriting .git/config.
	if err := m.endAgentPhase(ctx, ws); err != nil {
		return "", false, err
	}
	// The container may have rewritten .git/config before it was removed;
	// never trust it before pushing with the token in scope.
	if err := resetGitConfig(ws.Path, ws.CloneURL); err != nil {
		return "", false, fmt.Errorf("reset git config for %s: %w", ws.Repository, err)
	}
	if _, err := m.git(ctx, ws.Path, "add", "-A"); err != nil {
		return "", false, err
	}
	if _, err := m.git(ctx, ws.Path, "diff", "--cached", "--quiet"); err != nil {
		if _, err := m.git(ctx, ws.Path, "commit", "-q", "-m", message); err != nil {
			return "", false, err
		}
	}
	headOut, err := m.gitOut(ctx, ws.Path, "rev-parse", "HEAD")
	if err != nil {
		return "", false, err
	}
	head, err := validateSha(headOut)
	if err != nil {
		return "", false, err
	}
	lease := "--force-with-lease=refs/heads/" + ws.Branch + ":" + ws.RemoteHead
	if _, err := m.gitRemote(ctx, ws.Path, token, ws.CloneURL, gitTimeout,
		"push", lease, ws.CloneURL, "HEAD:refs/heads/"+ws.Branch); err != nil {
		return head, false, err
	}
	return head, true, nil
}

// conflictOpen and conflictClose are the two markers a merge writes around a
// hunk it could not resolve. A run of equals signs is the third marker git
// writes, but it is also how markdown underlines a heading, so it is not on
// its own evidence of anything: a branch counts as conflicted only when the
// commits added both an opening and a closing marker.
const (
	conflictOpen  = "<<<<<<< "
	conflictClose = ">>>>>>> "
)

// ConflictMarkers reports whether the commits this branch adds on top of its
// base left merge conflict markers in the tree. checkout leaves them there
// deliberately when the rebase onto the default branch conflicts, so
// resolving them is the agent's first job; an agent that instead committed
// them would otherwise reach a pull request.
//
// Valid only after CommitAndPush, which is what removes the agent's
// container and commits the working tree: called any earlier this either
// races the container over .git or reads a HEAD the agent's work has not
// reached yet. Runs through the same hardened local-git path as every other
// host command, so a rewritten .git/config cannot turn the scan into an exec
// surface.
func (m *Manager) ConflictMarkers(ctx context.Context, ws Workspace) (bool, error) {
	base, err := validateSha(ws.BaseSha)
	if err != nil {
		return false, fmt.Errorf("conflict markers for %s: %w", ws.Repository, err)
	}
	// Added and modified files only, with no context lines: every line the
	// scan sees is one the branch itself wrote, so a marker that was already
	// on the default branch is not this attempt's doing.
	diff, err := m.gitOut(ctx, ws.Path, "diff", base+"..HEAD", "--diff-filter=AM", "--unified=0")
	if err != nil {
		return false, fmt.Errorf("conflict markers for %s: %w", ws.Repository, err)
	}
	var opened, closed bool
	for _, line := range strings.Split(diff, "\n") {
		// Added lines only. "+++" is the file header, not content.
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		added := line[1:]
		if strings.HasPrefix(added, conflictOpen) {
			opened = true
		}
		if strings.HasPrefix(added, conflictClose) {
			closed = true
		}
		if opened && closed {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}
