package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
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

// volumeSlug names one repository's cache volumes and labels its
// containers. Appending 8 hex characters of sha256(repository) after the
// "/" -> "-" replace keeps "a/b-c" and "a-b/c" from colliding on the same
// slug.
func volumeSlug(repository string) string {
	sum := sha256.Sum256([]byte(repository))
	return strings.ReplaceAll(repository, "/", "-") + "-" + hex.EncodeToString(sum[:])[:8]
}

// workspaceLabel is the podman label every container this package starts
// carries, so a container can be found by the workspace it holds rather
// than by a name this process has to remember. The daemon's own memory of
// its containers does not survive a restart; the label does.
func workspaceLabel(repository string) string {
	return "autophage.workspace=" + volumeSlug(repository)
}

// cacheVolumes mounts one repository's three caches. The ":z" suffix is
// load-bearing on an SELinux host: without it every file the prep container
// writes into a cache carries that container's private MCS category, and the
// agent container that mounts the same volume a moment later is denied even
// read access to it. The whole point of the prep phase is that what it
// fetches is there for the agent phase, so these volumes are shared-label by
// construction. /work stays ":Z" (private, relabelled per container): only
// one container is ever on a workspace at a time.
func cacheVolumes(repository string) []string {
	slug := volumeSlug(repository)
	return []string{
		"-v", "autophage-cache-go-" + slug + ":/cache/go:z",
		"-v", "autophage-cache-npm-" + slug + ":/cache/npm:z",
		"-v", "autophage-cache-uv-" + slug + ":/cache/uv:z",
	}
}

// agentHomeMount gives the agent a writable home. A plain --tmpfs mounts as
// root with mode 0755, which leaves the agent user unable to write to its own
// $HOME: npm, uv, go and every tool that keeps state there then fails on a
// read-only root filesystem with nowhere else to go. U=true chowns the mount
// to the container user, and 0700 keeps it private. The size is in bytes
// (256m) because the --mount form takes no suffix.
const agentHomeMount = "type=tmpfs,destination=/home/agent,tmpfs-size=268435456,tmpfs-mode=0700,U=true"

// userns maps the daemon's own host uid onto the image's agent user
// (uid 1000, gid 1000) rather than onto whatever uid the daemon happens to
// run as. Plain keep-id maps the host uid to the same number inside the
// container, which is only uid 1000 by luck: on any other host uid the
// agent user's home, caches and /work ownership would all be wrong.
const userns = "--userns=keep-id:uid=1000,gid=1000"

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
// registries the lockfiles name). GOPROXY names the proxy alone, with no
// ",direct" fallback, so a go.mod committed by a previous attempt cannot
// send this phase to fetch a module straight from a host of the attacker's
// choosing; -mod=readonly keeps it from rewriting go.mod to widen that.
// npm and uv still follow the URLs in their own lockfiles (see ADR 0002).
func (m *Manager) warm(ctx context.Context, ws Workspace) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	args := []string{"run", "--rm", userns, "--cap-drop=all", "--security-opt=no-new-privileges",
		"--label", workspaceLabel(ws.Repository),
		"-e", "GOPROXY=https://proxy.golang.org", "-e", "GOFLAGS=-mod=readonly", "-e", "GOSUMDB=sum.golang.org"}
	args = append(args, m.resourceLimitArgs()...)
	args = append(args, "-v", ws.Path+":/work:Z")
	args = append(args, cacheVolumes(ws.Repository)...)
	args = append(args, "-w", "/work", m.Image, "warm-deps")
	stdout, err := run(ctx, "", nil, m.podman(), args...)
	// warm-deps never fails the attempt, so every failure it reports is
	// only ever visible here: log all of them, not just whichever landed
	// last. The agent finds out about the rest when it builds.
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "failed") {
			m.logf("warm-deps %s: %s", ws.Repository, strings.TrimSpace(line))
		}
	}
	m.logf("warm-deps %s: %s", ws.Repository, lastLine(stdout))
	return err
}

// attemptIDPattern bounds what can reach a container name. attemptID comes
// from the caller, and the name is spliced into podman argv and into a
// label filter; anything outside this set has no business in either.
var attemptIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,60}$`)

// Start runs the agent container: no network, hardened, workspace and
// caches mounted, idling until the daemon execs the toolbox into it.
// --replace clears out any container left by a name collision with a
// crashed previous attempt, so a crash between Start and Teardown cannot
// wedge the next attempt on "name already in use". The workspace label is
// what lets a later Prepare find and remove this container without having
// to remember its name across a restart. On its own failure Start releases
// the workspace lock Prepare acquired, since the runner only calls Teardown
// (which also releases it) after a successful Start.
func (m *Manager) Start(ctx context.Context, ws Workspace, attemptID string) (Container, error) {
	if !attemptIDPattern.MatchString(attemptID) {
		return Container{}, fmt.Errorf("invalid attempt id %q: want 1 to 60 characters of [A-Za-z0-9_-]", attemptID)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	name := "autophage-" + attemptID
	args := []string{"run", "-d", "--replace", "--name", name, "--network=none", userns,
		"--label", workspaceLabel(ws.Repository),
		"--cap-drop=all", "--security-opt=no-new-privileges", "--read-only",
		"--tmpfs", "/tmp:rw,size=1g", "--mount", agentHomeMount,
		"-e", "GOPROXY=off", "-e", "GOFLAGS=-mod=mod",
		"-v", ws.Path + ":/work:Z"}
	args = append(args, m.resourceLimitArgs()...)
	args = append(args, cacheVolumes(ws.Repository)...)
	args = append(args, "-w", "/work", m.Image, "sleep", "infinity")
	if _, err := run(ctx, "", nil, m.podman(), args...); err != nil {
		m.unlock(ws.Repository, ws.lease)
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
// unvalidated value here would be a shell injection into podman exec. The
// count is advisory, not enforcement: it is computed in the container from
// a .git the agent owns.
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
// might still have it mounted. It releases even when the removal failed, and
// the error says so; Prepare's label sweep, not this unlock, is what
// guarantees no container is left on the workspace. The 2 minute ctx bounds
// a hung podman.
func (m *Manager) Teardown(ctx context.Context, c Container) error {
	defer m.unlock(c.Workspace.Repository, c.Workspace.lease)
	m.forgetContainer(c.Workspace.Path)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	stdout, err := run(ctx, "", nil, m.podman(), "rm", "-f", c.Name)
	if err != nil && !strings.Contains(stdout, "no such container") {
		return err
	}
	return nil
}

// sweepContainers removes every container labelled for repository's
// workspace, whether or not this process started it. Any listing or removal
// failure is returned, so a caller that cannot prove the workspace is
// unattended stops rather than running host git against it. A container
// that is already gone by the time its removal runs is not a failure: only
// podman itself produces that message, and the race it reports (the
// container exited between the list and the removal) is the outcome this
// wanted anyway.
func (m *Manager) sweepContainers(ctx context.Context, repository string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	stdout, err := out(ctx, "", nil, m.podman(), "ps", "-aq", "--filter", "label="+workspaceLabel(repository))
	if err != nil {
		return fmt.Errorf("list containers holding %s: %w", repository, err)
	}
	for _, id := range strings.Fields(stdout) {
		removed, err := run(ctx, "", nil, m.podman(), "rm", "-f", id)
		if err != nil && !strings.Contains(removed, "no such container") {
			return fmt.Errorf("remove container %s holding %s: %w", id, repository, err)
		}
		m.logf("removed container %s left on %s", id, repository)
	}
	return nil
}

// endAgentPhase ends the agent phase for ws before CommitAndPush runs any
// host git command: the runner's fixed call order (Start, the agent's
// turns, CommitAndPush, Teardown) leaves this as the only point that can
// guarantee no container is on the workspace before host git trusts it
// again. Fails closed: any removal failure other than the container already
// being gone stops the caller before it touches git or pushes with the
// token in scope. The recorded name is removed first (a workspace with no
// recorded container is a no-op), then the label sweep catches anything
// this process never recorded, such as a container from before a restart.
func (m *Manager) endAgentPhase(ctx context.Context, ws Workspace) error {
	if name := m.forgetContainer(ws.Path); name != "" {
		removeCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		stdout, err := run(removeCtx, "", nil, m.podman(), "rm", "-f", name)
		cancel()
		if err != nil && !strings.Contains(stdout, "no such container") {
			return fmt.Errorf("end agent phase: remove container %s: %w", name, err)
		}
	}
	if err := m.sweepContainers(ctx, ws.Repository); err != nil {
		return fmt.Errorf("end agent phase: %w", err)
	}
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
