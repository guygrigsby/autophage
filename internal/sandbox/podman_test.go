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
		t.Skip("podman not on PATH; sandbox container tests run on trig")
	}
	image := "localhost/autophage-sandbox:latest"
	if err := exec.Command(bin, "image", "exists", image).Run(); err != nil {
		t.Skipf("image %s not built; run make image", image)
	}
	return &Manager{Podman: bin, Image: image, WorkspacesDir: t.TempDir(), Memory: "1g", CPUs: "1", Pids: 256, BotName: "autophage[bot]", BotEmail: "autophage[bot]@users.noreply.github.com", Logf: t.Logf}
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
		"--network=none", "--userns=keep-id", "--cap-drop=all",
		"--security-opt=no-new-privileges", "--read-only",
		"--tmpfs /tmp:rw,size=1g", "--tmpfs /home/agent:rw,size=256m",
		"--memory 1g", "--cpus 1", "--pids-limit 256", "--replace",
		"GOPROXY=off",
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
// shows the rm -f call, and the push still lands (which the code can only
// reach once killAgentContainer has returned nil).
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
