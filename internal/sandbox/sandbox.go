package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	ac "github.com/voocel/agentcore"
)

// Workspace is a case's checkout on host disk, on its branch.
type Workspace struct {
	Path          string
	Repository    string
	Branch        string
	DefaultBranch string
	BaseSha       string // origin/<default> the branch was rebased onto
}

// Container is a running agent container.
type Container struct {
	Name string
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
}

// run executes name with args under ctx, returning trimmed combined output.
// env is appended to the process environment.
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

// redact hides header values in error messages.
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
