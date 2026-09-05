package sandbox

import (
	"context"
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

func cacheVolumes(repository string) []string {
	slug := strings.ReplaceAll(repository, "/", "-")
	return []string{
		"-v", "autophage-cache-go-" + slug + ":/cache/go",
		"-v", "autophage-cache-npm-" + slug + ":/cache/npm",
		"-v", "autophage-cache-uv-" + slug + ":/cache/uv",
	}
}

// warm runs the dependency warmer in a networked prep container.
func (m *Manager) warm(ctx context.Context, ws Workspace) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	args := append([]string{"run", "--rm", "--userns=keep-id", "-v", ws.Path + ":/work:Z"}, cacheVolumes(ws.Repository)...)
	args = append(args, "-w", "/work", m.Image, "warm-deps")
	out, err := run(ctx, "", nil, m.podman(), args...)
	m.logf("warm-deps %s: %s", ws.Repository, lastLine(out))
	return err
}

// Start runs the agent container: no network, hardened, workspace and caches
// mounted, idling until the daemon execs the toolbox into it.
func (m *Manager) Start(ctx context.Context, ws Workspace, attemptID string) (Container, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	name := "autophage-" + attemptID
	args := []string{"run", "-d", "--name", name, "--network=none", "--userns=keep-id",
		"--cap-drop=all", "--security-opt=no-new-privileges", "--read-only", "--tmpfs", "/tmp:rw,size=1g",
		"-e", "GOPROXY=off", "-e", "GOFLAGS=-mod=mod",
		"-v", ws.Path + ":/work:Z"}
	if m.Memory != "" {
		args = append(args, "--memory", m.Memory)
	}
	if m.CPUs != "" {
		args = append(args, "--cpus", m.CPUs)
	}
	if m.Pids > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(m.Pids))
	}
	args = append(args, cacheVolumes(ws.Repository)...)
	args = append(args, "-w", "/work", m.Image, "sleep", "infinity")
	if _, err := run(ctx, "", nil, m.podman(), args...); err != nil {
		return Container{}, fmt.Errorf("start container: %w", err)
	}
	return Container{Name: name}, nil
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
func (m *Manager) DiffLines(ctx context.Context, c Container, baseSha string) (int, error) {
	script := "cd /work && git add -N . && git diff --numstat " + baseSha + " | awk '{a+=$1; d+=$2} END {print a+d}'"
	out, err := run(ctx, "", nil, m.podman(), "exec", c.Name, "sh", "-c", script)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(lastLine(out))
	if err != nil {
		return 0, fmt.Errorf("diff lines: %q", out)
	}
	return n, nil
}

func (m *Manager) Teardown(ctx context.Context, c Container) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := run(ctx, "", nil, m.podman(), "rm", "-f", c.Name)
	if err != nil && !strings.Contains(out, "no such container") {
		return err
	}
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
