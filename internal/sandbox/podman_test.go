package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func realPodman(t *testing.T) *Manager {
	t.Helper()
	bin, err := exec.LookPath("podman")
	if err != nil {
		t.Skip("podman not on PATH; sandbox container tests run on the deploy host")
	}
	image := "localhost/autophage-sandbox:latest"
	if err := exec.Command(bin, "image", "exists", image).Run(); err != nil {
		t.Skipf("image %s not built; run make image", image)
	}
	m := &Manager{Podman: bin, Image: image, WorkspacesDir: t.TempDir(), Memory: "1g", CPUs: "1", Pids: 256, BotName: "autophage[bot]", BotEmail: "autophage[bot]@users.noreply.github.com", Logf: t.Logf}
	// The cache volumes outlive the containers by design, so a test run that
	// did not clean up would leave three more of them on the host every time.
	t.Cleanup(func() {
		for _, v := range cacheVolumes("guy/repo") {
			name, _, ok := strings.Cut(v, ":")
			if !ok { // the "-v" flag itself, not a mount spec
				continue
			}
			_ = exec.Command(bin, "volume", "rm", "-f", name).Run()
		}
	})
	return m
}

func TestContainerLifecycleToolsAndDiff(t *testing.T) {
	m := realPodman(t)
	bare := origin(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	ws, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.Start(ctx, ws, "test-attempt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Teardown(context.Background(), c) })

	// The label is how a later Prepare finds this container without having
	// remembered its name, so prove podman really recorded it.
	labelled, err := exec.Command(m.Podman, "inspect", "--format", `{{index .Config.Labels "autophage.workspace"}}`, c.Name).Output()
	if err != nil {
		t.Fatalf("inspect %s: %v", c.Name, err)
	}
	if got := strings.TrimSpace(string(labelled)); got != volumeSlug("guy/repo") {
		t.Errorf("autophage.workspace label = %q, want %q", got, volumeSlug("guy/repo"))
	}

	tools, closer, err := m.Tools(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	var bash, write interface {
		Execute(context.Context, json.RawMessage) (json.RawMessage, error)
	}
	for _, tool := range tools {
		switch tool.Name() {
		case "bash":
			bash = tool
		case "write":
			write = tool
		}
	}
	if bash == nil || write == nil {
		t.Fatal("bash or write tool missing from the toolbox")
	}
	out, err := bash.Execute(ctx, json.RawMessage(`{"command":"id -u && ls /work && (curl -sS -m 2 https://example.com >/dev/null 2>&1 && echo NET || echo NONET)"}`))
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if !strings.Contains(string(out), "1000") || !strings.Contains(string(out), "README.md") || !strings.Contains(string(out), "NONET") {
		t.Errorf("bash out = %s", out)
	}

	// The shape the agent actually needs out of the production container:
	// a writable /tmp and home on the tmpfs mounts, a Go build that reaches
	// the module and build caches on the volumes with no network, and the
	// other two runtimes on PATH. The build happens under /work (the only
	// place an attempt's code lives) and is removed again, so the diff
	// assertions further down still see just the two edits.
	toolcheck := `set -e
test -w /tmp && test -w "$HOME" && echo TMPHOME_OK
node -e 'console.log("NODE_OK")'
python3 -c 'print("PY_OK")'
mkdir -p /work/.toolcheck && cd /work/.toolcheck
cat > main.go <<'GOEOF'
package main

import "fmt"

func main() { fmt.Println("GO_OK") }
GOEOF
go mod init toolcheck >/dev/null
go build -o /tmp/toolcheck .
/tmp/toolcheck
cd /work && rm -rf /work/.toolcheck
`
	arg, err := json.Marshal(map[string]string{"command": toolcheck})
	if err != nil {
		t.Fatal(err)
	}
	out, err = bash.Execute(ctx, arg)
	if err != nil {
		t.Fatalf("toolchain check: %v", err)
	}
	for _, want := range []string{"TMPHOME_OK", "NODE_OK", "PY_OK", "GO_OK"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("toolchain check missing %s:\n%s", want, out)
		}
	}
	if _, err := write.Execute(ctx, json.RawMessage(`{"file_path":"new.txt","content":"a\nb\nc\n"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := bash.Execute(ctx, json.RawMessage(`{"command":"printf 'x\\n' >> README.md"}`)); err != nil {
		t.Fatal(err)
	}
	n, err := m.DiffLines(ctx, c, ws.BaseSha)
	if err != nil || n != 4 {
		t.Errorf("diff lines = %d %v, want 4 (three new, one appended)", n, err)
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "new.txt")); err != nil {
		t.Error("write inside the container did not land in the host workspace")
	}
	if err := m.Teardown(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(m.Podman, "container", "exists", c.Name).Run(); err == nil {
		t.Error("container still exists after teardown")
	}
}

// TestStartHardensTheAgentContainer runs on the Mac against a stub podman
// that records its argv and asserts the full hardening flag set from ADR
// 0002 is present: no network, hardened user namespace, dropped
// capabilities, no new privileges, read-only root with writable /tmp and
// /home/agent, resource limits and --replace so a crash between Start and
// Teardown cannot wedge the next attempt on a name collision.
func TestStartHardensTheAgentContainer(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "podman")
	script := "#!/bin/sh\necho \"$@\" >> " + argsFile + "\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Podman: bin, Image: "img", WorkspacesDir: t.TempDir(), Memory: "1g", CPUs: "1", Pids: 256, BotName: "b", BotEmail: "b@x", Logf: t.Logf}
	ws := Workspace{Path: t.TempDir(), Repository: "guy/repo"}
	if _, err := m.Start(t.Context(), ws, "attempt-1"); err != nil {
		t.Fatal(err)
	}
	recorded, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	line := string(recorded)
	for _, want := range []string{
		"--network=none", "--userns=keep-id:uid=1000,gid=1000", "--cap-drop=all",
		"--security-opt=no-new-privileges", "--read-only",
		"--tmpfs /tmp:rw,size=1g", "--mount " + agentHomeMount,
		"--memory 1g", "--cpus 1", "--pids-limit 256", "--replace",
		"GOPROXY=off",
		"--label autophage.workspace=" + volumeSlug("guy/repo"),
	} {
		if !strings.Contains(line, want) {
			t.Errorf("podman run args missing %q:\n%s", want, line)
		}
	}
}

// TestTeardownToleratesMissingContainer runs on the Mac against a stub
// podman that fails as podman rm does for a container that is already gone.
func TestTeardownToleratesMissingContainer(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "podman")
	script := "#!/bin/sh\necho 'Error: no such container' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Podman: bin, Logf: t.Logf}
	c := Container{Name: "gone", Workspace: Workspace{Repository: "guy/repo"}}
	if err := m.Teardown(t.Context(), c); err != nil {
		t.Fatalf("Teardown should tolerate a missing container: %v", err)
	}
}

// TestVolumeSlugAvoidsCollisions proves the cache volume slug cannot let two
// distinct repositories share a cache: a plain "/" -> "-" replace collides
// "a/b-c" with "a-b/c".
func TestVolumeSlugAvoidsCollisions(t *testing.T) {
	a := volumeSlug("a/b-c")
	b := volumeSlug("a-b/c")
	if a == b {
		t.Errorf("volumeSlug collided for %q and %q: %q", "a/b-c", "a-b/c", a)
	}
}

// loggingPodmanStub writes a stub podman that appends its argv (one line
// per invocation) to argsFile and exits 0, except that when failRM is set an
// "rm" invocation instead exits 1 with an error other than "no such
// container" on stderr - simulating a podman rm that genuinely failed
// rather than one that found the container already gone.
func loggingPodmanStub(t *testing.T, argsFile string, failRM bool) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "podman")
	script := "#!/bin/sh\necho \"$@\" >> " + argsFile + "\n"
	if failRM {
		script += "case \"$1\" in\n  rm) echo 'Error: something went wrong' >&2; exit 1 ;;\n  *) exit 0 ;;\nesac\n"
	} else {
		script += "exit 0\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestCommitAndPushEndsTheAgentPhaseBeforeGitRuns proves CommitAndPush kills
// the agent container before it runs any host git command: the argv log
// shows the rm -f call and the label sweep, and the push still lands (which
// the code can only reach once endAgentPhase has returned nil).
func TestCommitAndPushEndsTheAgentPhaseBeforeGitRuns(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	bin := loggingPodmanStub(t, argsFile, false)
	m := &Manager{Podman: bin, Image: "img", WorkspacesDir: t.TempDir(), BotName: "autophage[bot]", BotEmail: "autophage[bot]@users.noreply.github.com", Logf: t.Logf}
	bare := origin(t)
	ctx := t.Context()

	ws, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.Start(ctx, ws, "attempt-x")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "fix.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sha, pushed, err := m.CommitAndPush(ctx, ws, "tok", "autophage: end agent phase first")
	if err != nil || !pushed || len(sha) != 40 {
		t.Fatalf("push: %s %v %v", sha, pushed, err)
	}

	recorded, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(recorded), "rm -f "+c.Name) {
		t.Errorf("CommitAndPush did not remove the agent container %s:\n%s", c.Name, recorded)
	}
	if !strings.Contains(string(recorded), "ps -aq --filter label="+workspaceLabel("guy/repo")) {
		t.Errorf("CommitAndPush did not sweep the workspace's containers by label:\n%s", recorded)
	}
	if got := git(t, bare, "log", "-1", "--format=%s", "autophage/7"); got != "autophage: end agent phase first" {
		t.Errorf("push did not land: %q", got)
	}
	if err := m.Teardown(ctx, c); err != nil {
		t.Fatal(err)
	}
}

// TestCommitAndPushFailsClosedWhenItCannotConfirmTheContainerIsGone proves
// CommitAndPush refuses to touch git, let alone push with the token in
// scope, when it cannot confirm the agent container was removed.
func TestCommitAndPushFailsClosedWhenItCannotConfirmTheContainerIsGone(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	bin := loggingPodmanStub(t, argsFile, true)
	m := &Manager{Podman: bin, Image: "img", WorkspacesDir: t.TempDir(), BotName: "autophage[bot]", BotEmail: "autophage[bot]@users.noreply.github.com", Logf: t.Logf}
	bare := origin(t)
	ctx := t.Context()

	ws, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(ctx, ws, "attempt-y"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "fix.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, pushed, err := m.CommitAndPush(ctx, ws, "tok", "should not land")
	if err == nil {
		t.Fatal("expected CommitAndPush to fail closed when it cannot confirm the container is gone")
	}
	if pushed {
		t.Error("pushed should be false on a fail-closed CommitAndPush")
	}
	if got := git(t, bare, "for-each-ref", "--format=%(refname)"); got != "refs/heads/main" {
		t.Errorf("a commit landed despite the fail-closed container removal: refs = %q", got)
	}
}

// sweepPodmanStub writes a stub podman that appends its argv (one line per
// invocation) to argsFile, answers "ps" with the given container ids (one
// per line, on stdout) and either removes or refuses to remove them.
func sweepPodmanStub(t *testing.T, argsFile string, ids []string, failPS, failRM bool) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "podman")
	script := "#!/bin/sh\necho \"$@\" >> " + argsFile + "\ncase \"$1\" in\n"
	if failPS {
		script += "  ps) echo 'Error: cannot connect to the podman socket' >&2; exit 125 ;;\n"
	} else {
		script += "  ps) printf '%s' '" + strings.Join(ids, "\n") + "'; exit 0 ;;\n"
	}
	if failRM {
		script += "  rm) echo 'Error: something went wrong' >&2; exit 1 ;;\n"
	}
	script += "  *) exit 0 ;;\nesac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestPrepareSweepsContainersLeftOnTheWorkspace proves Prepare removes every
// container labelled for the repository before it does anything else. The
// in-process lock cannot stand in for this: it is empty after a daemon
// restart, and a Teardown whose removal failed unlocks anyway, so without
// the sweep an orphaned container could still be rewriting .git/config while
// host git runs there with the installation token in scope.
func TestPrepareSweepsContainersLeftOnTheWorkspace(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	bin := sweepPodmanStub(t, argsFile, []string{"c0ffee1"}, false, false)
	m := &Manager{Podman: bin, Image: "img", WorkspacesDir: t.TempDir(), BotName: "b", BotEmail: "b@x", Logf: t.Logf}
	bare := origin(t)

	if _, err := m.Prepare(t.Context(), "guy/repo", bare, "autophage/7", "main", "tok"); err != nil {
		t.Fatal(err)
	}
	recorded, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(recorded)), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected a list and a removal before anything else:\n%s", recorded)
	}
	if want := "ps -aq --filter label=" + workspaceLabel("guy/repo"); lines[0] != want {
		t.Errorf("first podman call = %q, want %q", lines[0], want)
	}
	if lines[1] != "rm -f c0ffee1" {
		t.Errorf("second podman call = %q, want %q", lines[1], "rm -f c0ffee1")
	}
}

// TestPrepareFailsClosedWhenAStaleContainerSurvives proves Prepare stops
// before any git runs when it cannot remove a container that is still on the
// workspace: nothing is cloned and the remote is untouched.
func TestPrepareFailsClosedWhenAStaleContainerSurvives(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	bin := sweepPodmanStub(t, argsFile, []string{"c0ffee1"}, false, true)
	m := &Manager{Podman: bin, Image: "img", WorkspacesDir: t.TempDir(), BotName: "b", BotEmail: "b@x", Logf: t.Logf}
	bare := origin(t)
	before := git(t, bare, "for-each-ref", "--format=%(refname) %(objectname)")

	if _, err := m.Prepare(t.Context(), "guy/repo", bare, "autophage/7", "main", "tok"); err == nil {
		t.Fatal("expected Prepare to fail closed when a stale container cannot be removed")
	}
	if _, err := os.Stat(filepath.Join(m.WorkspacesDir, "guy", "repo")); !os.IsNotExist(err) {
		t.Errorf("git ran despite the failed sweep: workspace exists (%v)", err)
	}
	if after := git(t, bare, "for-each-ref", "--format=%(refname) %(objectname)"); after != before {
		t.Errorf("the remote changed despite the failed sweep:\n%s\n%s", before, after)
	}
	// The sweep runs under the lock, so failing it must not strand the lock.
	m.Podman = stubPodman(t)
	bounded, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := m.Prepare(bounded, "guy/repo", bare, "autophage/7", "main", "tok"); err != nil {
		t.Errorf("Prepare after a failed sweep should not be blocked by a leaked lock: %v", err)
	}
}

// TestPrepareFailsWhenItCannotListContainers proves a podman that cannot
// answer the question at all is a failure, not an empty answer treated as
// "nothing is holding the workspace".
func TestPrepareFailsWhenItCannotListContainers(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	bin := sweepPodmanStub(t, argsFile, nil, true, false)
	m := &Manager{Podman: bin, Image: "img", WorkspacesDir: t.TempDir(), BotName: "b", BotEmail: "b@x", Logf: t.Logf}
	bare := origin(t)

	if _, err := m.Prepare(t.Context(), "guy/repo", bare, "autophage/7", "main", "tok"); err == nil {
		t.Fatal("expected Prepare to fail when the container listing fails")
	}
	if _, err := os.Stat(filepath.Join(m.WorkspacesDir, "guy", "repo")); !os.IsNotExist(err) {
		t.Errorf("git ran despite the failed listing: workspace exists (%v)", err)
	}
}

// TestStartRejectsAMalformedAttemptID proves the attempt id cannot carry
// anything into a container name, a label filter or podman's argv.
func TestStartRejectsAMalformedAttemptID(t *testing.T) {
	m := &Manager{Podman: stubPodman(t), Image: "img", WorkspacesDir: t.TempDir(), Logf: t.Logf}
	ws := Workspace{Path: t.TempDir(), Repository: "guy/repo"}
	for _, bad := range []string{"", "has space", "semi;colon", "slash/es", "dollar$sign", "quote'", strings.Repeat("a", 61)} {
		if _, err := m.Start(t.Context(), ws, bad); err == nil {
			t.Errorf("Start accepted attempt id %q", bad)
		}
	}
	for _, good := range []string{"a", "attempt-1", "ATTEMPT_2", strings.Repeat("a", 60)} {
		if _, err := m.Start(t.Context(), ws, good); err != nil {
			t.Errorf("Start rejected attempt id %q: %v", good, err)
		}
	}
}
