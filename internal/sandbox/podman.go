package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	jessmcp "github.com/guygrigsby/jess/mcp"
	ac "github.com/voocel/agentcore"
)

func (m *Manager) podman() string {
	if m.Podman == "" {
		return "podman"
	}
	return m.Podman
}

// volumeSlug names one repository's cache volumes. Appending 8 hex
// characters of sha256(repository) after the "/" -> "-" replace keeps
// "a/b-c" and "a-b/c" from colliding on the same slug.
func volumeSlug(repository string) string {
	sum := sha256.Sum256([]byte(repository))
	return strings.ReplaceAll(repository, "/", "-") + "-" + hex.EncodeToString(sum[:])[:8]
}

func cacheVolumes(repository string) []string {
	slug := volumeSlug(repository)
	return []string{
		"-v", "autophage-cache-go-" + slug + ":/cache/go",
		"-v", "autophage-cache-npm-" + slug + ":/cache/npm",
		"-v", "autophage-cache-uv-" + slug + ":/cache/uv",
	}
}

// resourceLimitArgs builds the --memory/--cpus/--pids-limit flags shared by
// the prep and agent containers, omitting whichever the Manager left unset.
func (m *Manager) resourceLimitArgs() []string {
	var args []string
	if m.Memory != "" {
		args = append(args, "--memory", m.Memory)
	}
	if m.CPUs != "" {
		args = append(args, "--cpus", m.CPUs)
	}
	if m.Pids > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(m.Pids))
	}
	return args
}

// warm runs the dependency warmer in a networked prep container. It carries
// the same non-root-namespace and capability hardening as the agent
// container (network stays on: this is the only phase allowed to reach the
// registries the lockfiles name).
func (m *Manager) warm(ctx context.Context, ws Workspace) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	args := []string{"run", "--rm", "--userns=keep-id", "--cap-drop=all", "--security-opt=no-new-privileges"}
	args = append(args, m.resourceLimitArgs()...)
	args = append(args, "-v", ws.Path+":/work:Z")
	args = append(args, cacheVolumes(ws.Repository)...)
	args = append(args, "-w", "/work", m.Image, "warm-deps")
	stdout, err := run(ctx, "", nil, m.podman(), args...)
	m.logf("warm-deps %s: %s", ws.Repository, lastLine(stdout))
	return err
}

// Start runs the agent container: no network, hardened, workspace and
// caches mounted, idling until the daemon execs the toolbox into it.
// --replace clears out any container left by a name collision with a
// crashed previous attempt, so a crash between Start and Teardown cannot
// wedge the next attempt on "name already in use". On its own failure Start
// releases the workspace lock Prepare acquired, since the runner only calls
// Teardown (which also releases it) after a successful Start.
func (m *Manager) Start(ctx context.Context, ws Workspace, attemptID string) (Container, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	name := "autophage-" + attemptID
	args := []string{"run", "-d", "--replace", "--name", name, "--network=none", "--userns=keep-id",
		"--cap-drop=all", "--security-opt=no-new-privileges", "--read-only",
		"--tmpfs", "/tmp:rw,size=1g", "--tmpfs", "/home/agent:rw,size=256m",
		"-e", "GOPROXY=off", "-e", "GOFLAGS=-mod=mod",
		"-v", ws.Path + ":/work:Z"}
	args = append(args, m.resourceLimitArgs()...)
	args = append(args, cacheVolumes(ws.Repository)...)
	args = append(args, "-w", "/work", m.Image, "sleep", "infinity")
	if _, err := run(ctx, "", nil, m.podman(), args...); err != nil {
		m.unlock(ws.Repository)
		return Container{}, fmt.Errorf("start container: %w", err)
	}
	m.recordContainer(ws.Path, name)
	return Container{Name: name, Workspace: ws}, nil
}

// Tools dials the toolbox inside the container over podman exec stdio.
func (m *Manager) Tools(ctx context.Context, c Container) ([]ac.Tool, io.Closer, error) {
	return jessmcp.Tools(ctx, []jessmcp.Server{{
		Name:    "toolbox",
		Command: m.podman(),
		Args:    []string{"exec", "-i", c.Name, "autophage-toolbox", "-workdir", "/work"},
		Bare:    true,
	}}, m.Logf)
}

// DiffLines counts added plus removed lines against base, inside the
// container so nothing the agent did can reach the host through a path.
// baseSha is validated before it is spliced into the shell script: an
// unvalidated value here would be a shell injection into podman exec.
func (m *Manager) DiffLines(ctx context.Context, c Container, baseSha string) (int, error) {
	baseSha, err := validateSha(baseSha)
	if err != nil {
		return 0, fmt.Errorf("diff lines: refusing base sha: %w", err)
	}
	script := "cd /work && git add -N . && git diff --numstat " + baseSha + " | awk '{a+=$1; d+=$2} END {print a+d}'"
	stdout, err := run(ctx, "", nil, m.podman(), "exec", c.Name, "sh", "-c", script)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(lastLine(stdout))
	if err != nil {
		return 0, fmt.Errorf("diff lines: %q", stdout)
	}
	return n, nil
}

// Teardown removes the container and releases the workspace lock Prepare
// acquired and Start held onto. The unlock is deferred, so it runs after the
// podman rm attempt returns (or if this function panics), not before: the
// next attempt's Prepare must not start touching /work while this container
// might still have it mounted. The 2 minute ctx bounds a hung podman.
func (m *Manager) Teardown(ctx context.Context, c Container) error {
	defer m.unlock(c.Workspace.Repository)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	stdout, err := run(ctx, "", nil, m.podman(), "rm", "-f", c.Name)
	if err != nil && !strings.Contains(stdout, "no such container") {
		return err
	}
	return nil
}

// killAgentContainer ends the agent phase for wsPath before CommitAndPush
// runs any host git command: the runner's fixed call order (Start, the
// agent's turns, CommitAndPush, Teardown) leaves this as the only point
// that can guarantee the container is gone before host git trusts the
// workspace again. Fails closed: any removal failure other than the
// container already being gone stops the caller before it touches git or
// pushes with the token in scope. A wsPath with no recorded container (no
// Start was ever called, or a previous call already consumed it) is a no-op.
func (m *Manager) killAgentContainer(ctx context.Context, wsPath string) error {
	name := m.forgetContainer(wsPath)
	if name == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	stdout, err := run(ctx, "", nil, m.podman(), "rm", "-f", name)
	if err != nil && !strings.Contains(stdout, "no such container") {
		return fmt.Errorf("end agent phase: remove container %s: %w", name, err)
	}
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
