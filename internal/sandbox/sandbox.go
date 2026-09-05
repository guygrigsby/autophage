package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	ac "github.com/voocel/agentcore"
)

// Workspace is a case's checkout on host disk, on its branch.
type Workspace struct {
	Path          string
	Repository    string
	CloneURL      string // trusted origin URL; never taken from the workspace's own config
	Branch        string
	DefaultBranch string
	BaseSha       string // origin/<default> the branch was rebased onto
}

// Container is a running agent container.
type Container struct {
	Name string
	// Workspace is the workspace this container was started against, so
	// Teardown knows which repository's lock to release.
	Workspace Workspace
}

// Sandbox is what the runner needs. The podman Manager implements it.
type Sandbox interface {
	Prepare(ctx context.Context, repository, cloneURL, branch, defaultBranch, token string) (Workspace, error)
	Start(ctx context.Context, ws Workspace, attemptID string) (Container, error)
	Tools(ctx context.Context, c Container) ([]ac.Tool, io.Closer, error)
	DiffLines(ctx context.Context, c Container, baseSha string) (int, error)
	CommitAndPush(ctx context.Context, ws Workspace, token, message string) (headSha string, pushed bool, err error)
	Teardown(ctx context.Context, c Container) error
}

type Manager struct {
	Podman        string // binary, default "podman"
	Image         string
	WorkspacesDir string
	Memory        string // e.g. "4g"
	CPUs          string // e.g. "4"
	Pids          int    // e.g. 512
	BotName       string // git identity for commits
	BotEmail      string
	Logf          func(string, ...any)

	mu    sync.Mutex
	locks map[string]chan struct{} // repository -> a capacity-1 mutex-as-channel
}

// lock acquires the exclusive workspace lock for repository, blocking until
// it is free or ctx is done. Two attempts on the same repository must never
// share one workspace: the scheduler's concurrency limit is across all
// cases, not per repository, so this package owns the exclusion itself.
func (m *Manager) lock(ctx context.Context, repository string) error {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = map[string]chan struct{}{}
	}
	ch, ok := m.locks[repository]
	if !ok {
		ch = make(chan struct{}, 1)
		m.locks[repository] = ch
	}
	m.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unlock releases the workspace lock for repository. Safe to call more than
// once, or for a repository whose lock was never acquired (a Prepare that
// failed before reaching the lock, or a Teardown for a Container whose
// Workspace is zero): draining an already-empty or nonexistent channel is a
// no-op rather than a panic.
func (m *Manager) unlock(repository string) {
	m.mu.Lock()
	ch, ok := m.locks[repository]
	m.mu.Unlock()
	if !ok {
		return
	}
	select {
	case <-ch:
	default:
	}
}

// run executes name with args under ctx, returning trimmed combined output.
// env is appended to the process environment. Use out instead when the
// output must be parsed (a sha, for example): combined output mixes stderr
// warnings into stdout and can corrupt a value read from it.
func run(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("%s %s: %w: %s", name, strings.Join(redact(args), " "), err, s)
	}
	return s, nil
}

// out executes name with args under ctx, returning trimmed STDOUT only;
// stderr is captured solely to compose the error text on failure, never
// mixed into the value the caller parses. A git advice or hint line on
// stderr must not be able to corrupt a sha read from stdout.
func out(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	s := strings.TrimSpace(stdout.String())
	if err != nil {
		combined := strings.TrimSpace(stdout.String() + stderr.String())
		return s, fmt.Errorf("%s %s: %w: %s", name, strings.Join(redact(args), " "), err, combined)
	}
	return s, nil
}

// redact hides header values in error messages built from argv. Kept as a
// defense-in-depth pass over the command line even though the auth header
// itself now travels only through an env var (see git.go's redactAuth for
// that half): nothing here relies on this being the only scrubbing.
func redact(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "http.extraheader=") {
			a = "http.extraheader=<redacted>"
		}
		out[i] = a
	}
	return out
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// validateSha reports an error unless s is exactly 40 lowercase hex
// characters. Every sha this package hands to a caller or splices into a
// shell string is validated first, so a corrupted parse (a git warning
// leaking into what should have been just the sha) or an untrusted value
// can never carry shell metacharacters or truncate into a different commit.
func validateSha(s string) (string, error) {
	if !shaPattern.MatchString(s) {
		return "", fmt.Errorf("not a 40-character hex sha: %q", s)
	}
	return s, nil
}
