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
	// RemoteHead is refs/remotes/origin/<Branch> as Prepare's own fetch left
	// it, empty when the branch does not exist on the remote yet. It is the
	// expected value CommitAndPush pushes under: the ref cannot be read
	// again at push time, since by then the agent has had a container on
	// /work and owns every ref in it.
	RemoteHead string
	// lease identifies the acquisition of the repository's workspace lock
	// that produced this Workspace. unlock releases only for the holder, so
	// a stale Teardown (a second call with a Workspace whose attempt is long
	// over) cannot release the lock a later attempt is holding.
	lease *lease
}

// lease is a lock acquisition token, compared by pointer. The fields are
// what make one acquisition distinct from the next: Go is free to give every
// allocation of a zero-size struct the same address, so an empty lease would
// make each attempt's token equal to every other's.
type lease struct {
	repository string
	seq        uint64
}

// Container is a running agent container.
type Container struct {
	Name string
	// Workspace is the workspace this container was started against, so
	// Teardown knows which repository's lock to release.
	Workspace Workspace
}

// Sandbox is what the runner needs. The podman Manager implements it.
//
// CommitAndPush ends the agent phase itself before running any host git
// command: it removes the workspace's agent container first and fails
// closed if it cannot confirm the removal, since the runner's own call
// order (Start, the agent's turns, CommitAndPush, Teardown) leaves no other
// point that can guarantee the container is gone before host git trusts the
// workspace's files again. Tools' returned closer and DiffLines are invalid
// for that container once CommitAndPush has been called, whether or not it
// succeeded. The closer must still be closed afterwards: it reaps the
// podman exec child the toolbox runs in. It returns quickly, because the
// container it was exec'd into is already gone.
//
// DiffLines is advisory. It runs inside the container, where the agent owns
// .git and can make the count say whatever it likes; the enforced budget
// legs are turns and wall clock, which the daemon counts itself.
//
// ConflictMarkers is valid only after CommitAndPush, and only then: it runs
// host git against the workspace, which is safe only once CommitAndPush has
// confirmed the agent's container is gone, and it reads the commits that
// same call made.
type Sandbox interface {
	Prepare(ctx context.Context, repository, cloneURL, branch, defaultBranch, token string) (Workspace, error)
	Start(ctx context.Context, ws Workspace, attemptID string) (Container, error)
	Tools(ctx context.Context, c Container) ([]ac.Tool, io.Closer, error)
	DiffLines(ctx context.Context, c Container, baseSha string) (int, error)
	CommitAndPush(ctx context.Context, ws Workspace, token, message string) (headSha string, pushed bool, err error)
	ConflictMarkers(ctx context.Context, ws Workspace) (bool, error)
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

	mu         sync.Mutex
	seq        uint64                   // lease serial, so no two acquisitions share a token
	locks      map[string]chan struct{} // repository -> a capacity-1 mutex-as-channel
	holders    map[string]*lease        // repository -> the token of whoever holds that lock now
	containers map[string]string        // workspace path -> the agent container Start last recorded there
}

// recordContainer remembers which container Start just created for wsPath,
// so CommitAndPush can end the agent phase itself without the Sandbox
// interface having to pass a Container back into a method that only takes a
// Workspace.
func (m *Manager) recordContainer(wsPath, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.containers == nil {
		m.containers = map[string]string{}
	}
	m.containers[wsPath] = name
}

// forgetContainer removes and returns the container name recorded for
// wsPath, or "" if none is recorded (Start was never called for this
// workspace, or a previous call already consumed it).
func (m *Manager) forgetContainer(wsPath string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	name := m.containers[wsPath]
	delete(m.containers, wsPath)
	return name
}

// lock acquires the exclusive workspace lock for repository, blocking until
// it is free or ctx is done, and returns the token that owns the
// acquisition. Two attempts on the same repository must never share one
// workspace: the scheduler's concurrency limit is across all cases, not per
// repository, so this package owns the exclusion itself.
func (m *Manager) lock(ctx context.Context, repository string) (*lease, error) {
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
		m.mu.Lock()
		m.seq++
		held := &lease{repository: repository, seq: m.seq}
		if m.holders == nil {
			m.holders = map[string]*lease{}
		}
		m.holders[repository] = held
		m.mu.Unlock()
		return held, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// unlock releases the workspace lock for repository, but only when held is
// the token that currently owns it. Anything else is a no-op: a Teardown
// called twice, or called with a Workspace from an attempt that has already
// ended, must not hand the workspace to a third attempt while the second one
// is still running in it. A zero Workspace (a Container built by hand, a
// Prepare that failed before the lock) carries a nil token and releases
// nothing.
func (m *Manager) unlock(repository string, held *lease) {
	if held == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.holders[repository] != held {
		return
	}
	delete(m.holders, repository)
	ch, ok := m.locks[repository]
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
// The match is on ".extraheader=" anywhere in the argument, not on a
// leading "http.extraheader=": the key is scoped per URL
// ("http.https://github.com/.extraheader="), so a prefix match would miss
// every form this package actually builds.
func redact(args []string) []string {
	const key = ".extraheader="
	out := make([]string, len(args))
	for i, a := range args {
		if j := strings.Index(a, key); j >= 0 {
			a = a[:j+len(key)] + "<redacted>"
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
