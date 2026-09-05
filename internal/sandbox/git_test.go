package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// origin creates a bare remote with one commit on main and returns its path.
func origin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	git(t, root, "init", "--bare", "-b", "main", bare)
	seed := filepath.Join(root, "seed")
	git(t, root, "clone", "-q", bare, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("# seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", "README.md")
	git(t, seed, "commit", "-q", "-m", "seed")
	git(t, seed, "push", "-q", "origin", "main")
	return bare
}

func stubPodman(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "podman")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func manager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{Podman: stubPodman(t), Image: "x", WorkspacesDir: t.TempDir(), BotName: "autophage[bot]", BotEmail: "autophage[bot]@users.noreply.github.com", Logf: t.Logf}
}

func TestPrepareClonesThenFetchesAndBranches(t *testing.T) {
	m := manager(t)
	bare := origin(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	ws, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Path != filepath.Join(m.WorkspacesDir, "guy", "repo") || ws.Branch != "autophage/7" || len(ws.BaseSha) != 40 {
		t.Errorf("ws = %+v", ws)
	}
	if got := git(t, ws.Path, "branch", "--show-current"); got != "autophage/7" {
		t.Errorf("branch = %q", got)
	}
	if cfg := git(t, ws.Path, "config", "--list"); strings.Contains(cfg, "tok") || strings.Contains(cfg, "extraheader") {
		t.Errorf("token or header leaked into config:\n%s", cfg)
	}
	// A real attempt always ends in Teardown, which releases this
	// repository's workspace lock for the next attempt; mirror that here
	// before Prepare-ing again, or the second call blocks forever.
	if err := m.Teardown(ctx, Container{Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	ws2, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err != nil || ws2.BaseSha != ws.BaseSha {
		t.Errorf("second prepare: %+v %v", ws2, err)
	}
}

func TestCommitAndPushThenResumeRebases(t *testing.T) {
	m := manager(t)
	bare := origin(t)
	ctx := t.Context()
	ws, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "fix.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, pushed, err := m.CommitAndPush(ctx, ws, "tok", "autophage: first pass")
	if err != nil || !pushed || len(sha) != 40 {
		t.Fatalf("push: %s %v %v", sha, pushed, err)
	}
	if got := git(t, bare, "log", "-1", "--format=%s", "autophage/7"); got != "autophage: first pass" {
		t.Errorf("remote branch log = %q", got)
	}
	if author := git(t, bare, "log", "-1", "--format=%an <%ae>", "autophage/7"); author != "autophage[bot] <autophage[bot]@users.noreply.github.com>" {
		t.Errorf("author = %q", author)
	}
	if cfg := git(t, ws.Path, "config", "--list"); strings.Contains(cfg, "tok") || strings.Contains(cfg, "extraheader") {
		t.Errorf("token or header leaked into config after push:\n%s", cfg)
	}
	// A real attempt always ends in Teardown, which releases this
	// repository's workspace lock for the next attempt.
	if err := m.Teardown(ctx, Container{Workspace: ws}); err != nil {
		t.Fatal(err)
	}

	// main moves on; the resumed attempt must rebase onto it.
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", "-q", bare, other)
	if err := os.WriteFile(filepath.Join(other, "NEWS.md"), []byte("news\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, other, "add", "NEWS.md")
	git(t, other, "commit", "-q", "-m", "news")
	git(t, other, "push", "-q", "origin", "main")

	ws2, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if ws2.BaseSha == ws.BaseSha {
		t.Error("base sha did not advance")
	}
	if _, err := os.Stat(filepath.Join(ws2.Path, "NEWS.md")); err != nil {
		t.Error("rebase did not bring NEWS.md in")
	}
	if _, err := os.Stat(filepath.Join(ws2.Path, "fix.txt")); err != nil {
		t.Error("rebase lost fix.txt")
	}
	_, pushed, err = m.CommitAndPush(ctx, ws2, "tok", "autophage: nothing new")
	if err != nil || !pushed {
		t.Errorf("force-with-lease push after rebase: %v %v", pushed, err)
	}
}

func TestPrepareLeavesConflictMarkersOnRebaseConflict(t *testing.T) {
	m := manager(t)
	bare := origin(t)
	ctx := t.Context()
	ws, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "README.md"), []byte("# ours\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.CommitAndPush(ctx, ws, "tok", "ours"); err != nil {
		t.Fatal(err)
	}
	// A real attempt always ends in Teardown, which releases this
	// repository's workspace lock for the next attempt.
	if err := m.Teardown(ctx, Container{Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", "-q", bare, other)
	if err := os.WriteFile(filepath.Join(other, "README.md"), []byte("# theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, other, "commit", "-q", "-am", "theirs")
	git(t, other, "push", "-q", "origin", "main")

	ws2, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err != nil {
		t.Fatalf("prepare must not fail on conflict: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(ws2.Path, "README.md"))
	if !strings.Contains(string(b), "<<<<<<<") {
		t.Errorf("no conflict markers left for the agent:\n%s", b)
	}
}

// TestCommitAndPushIgnoresHostileGitConfig proves the critical fix: /work is
// bind-mounted whole, so a compromised agent container can rewrite
// .git/config (remote.origin.url, a credential helper, an sshCommand, an
// insteadOf rewrite) before the host ever runs git again. CommitAndPush must
// discard whatever config it finds and push to the real origin only.
func TestCommitAndPushIgnoresHostileGitConfig(t *testing.T) {
	m := manager(t)
	bare := origin(t)
	hostile := origin(t)
	ctx := t.Context()
	ws, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if ws.CloneURL != bare {
		t.Fatalf("ws.CloneURL = %q, want %q", ws.CloneURL, bare)
	}

	hostileConfig := fmt.Sprintf(`[remote "origin"]
	url = %s
	fetch = +refs/heads/*:refs/remotes/origin/*
[core]
	sshCommand = touch /tmp/autophage-pwned
[credential]
	helper = "!echo pwned"
[alias]
	push = "!echo pwned"
`, hostile)
	if err := os.WriteFile(filepath.Join(ws.Path, ".git", "config"), []byte(hostileConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "fix.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sha, pushed, err := m.CommitAndPush(ctx, ws, "tok", "autophage: ignore hostile config")
	if err != nil || !pushed || len(sha) != 40 {
		t.Fatalf("push: %s %v %v", sha, pushed, err)
	}
	if got := git(t, bare, "log", "-1", "--format=%s", "autophage/7"); got != "autophage: ignore hostile config" {
		t.Errorf("push did not land on the real remote: %q", got)
	}
	if got := git(t, hostile, "for-each-ref", "--format=%(refname)"); got != "refs/heads/main" {
		t.Errorf("push leaked to the hostile remote: refs = %q", got)
	}
	if cfg := git(t, ws.Path, "config", "--list"); strings.Contains(cfg, "tok") || strings.Contains(cfg, "extraheader") || strings.Contains(cfg, "pwned") {
		t.Errorf("hostile or leaked config survived:\n%s", cfg)
	}
}

// TestPrepareLocksPerRepository proves two concurrent attempts on the same
// repository cannot share one workspace: the scheduler's concurrency limit
// is across all cases, not per repository, so this package owns the
// exclusion itself.
func TestPrepareLocksPerRepository(t *testing.T) {
	m := manager(t)
	bare := origin(t)
	ctx := t.Context()

	ws1, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err != nil {
		t.Fatal(err)
	}
	c1 := Container{Name: "fake-container-1", Workspace: ws1}

	// The goroutine reports its result over a channel rather than calling
	// t.Error/t.Fatal itself: a testing.T method called from a goroutine
	// other than the test's own is not safe, so every assertion happens
	// back in this goroutine.
	results := make(chan error, 1)
	go func() {
		_, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
		results <- err
	}()

	select {
	case err := <-results:
		t.Fatalf("second Prepare returned (err=%v) before the first attempt's Teardown released the lock", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := m.Teardown(ctx, c1); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-results:
		if err != nil {
			t.Errorf("second Prepare failed once the lock was released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Prepare did not unblock after Teardown released the lock")
	}
}

// TestPrepareReleasesLockOnItsOwnFailure proves a Prepare that fails before
// Start is ever called does not leak the repository's lock forever.
func TestPrepareReleasesLockOnItsOwnFailure(t *testing.T) {
	m := manager(t)
	bare := origin(t)
	ctx := t.Context()
	badURL := filepath.Join(t.TempDir(), "does-not-exist.git")
	if _, err := m.Prepare(ctx, "guy/repo", badURL, "autophage/7", "main", "tok"); err == nil {
		t.Fatal("expected Prepare to fail against a nonexistent remote")
	}
	boundedCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := m.Prepare(boundedCtx, "guy/repo", bare, "autophage/7", "main", "tok"); err != nil {
		t.Fatalf("Prepare after a failed Prepare should not be blocked by a leaked lock: %v", err)
	}
}

// TestStartReleasesLockOnItsOwnFailure proves a Start that fails does not
// leak the repository's lock forever either (Teardown is never called in
// this case, so Start must release what Prepare acquired).
func TestStartReleasesLockOnItsOwnFailure(t *testing.T) {
	m := manager(t)
	bare := origin(t)
	ctx := t.Context()
	ws, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err != nil {
		t.Fatal(err)
	}
	workingPodman := m.Podman
	m.Podman = filepath.Join(t.TempDir(), "no-such-podman-binary")
	if _, err := m.Start(ctx, ws, "attempt-1"); err == nil {
		t.Fatal("expected Start to fail against a nonexistent podman binary")
	}
	m.Podman = workingPodman // isolate the assertion to the lock, not to Podman working
	boundedCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := m.Prepare(boundedCtx, "guy/repo", bare, "autophage/7", "main", "tok"); err != nil {
		t.Fatalf("Prepare after a failed Start should not be blocked by a leaked lock: %v", err)
	}
}

// TestWorkspacePathRejectsMalformedRepository proves the workspace path
// builder refuses anything that is not exactly two non-empty segments with
// no ".." component, so a repository string cannot walk WorkspacesDir.
func TestWorkspacePathRejectsMalformedRepository(t *testing.T) {
	m := &Manager{WorkspacesDir: t.TempDir()}
	for _, bad := range []string{"noSlash", "a/b/c", "/leading", "trailing/", "a/..", "../b", "a/b/../c", ""} {
		if _, err := m.workspacePath(bad); err == nil {
			t.Errorf("workspacePath(%q) should have failed", bad)
		}
	}
	if _, err := m.workspacePath("guy/repo"); err != nil {
		t.Errorf("workspacePath(good) failed: %v", err)
	}
}

// TestRedactedGitErrorPreservesTheChain proves redactAuth's scrubbing does
// not cut errors.Is/errors.As off from the real underlying failure: a
// caller still needs to tell a timeout from an ordinary git failure, and
// still needs *exec.ExitError (for its exit code) through a redacted error.
func TestRedactedGitErrorPreservesTheChain(t *testing.T) {
	m := manager(t)

	// rev-parse against an empty directory: git exits non-zero, so the
	// redacted error must still unwrap to the real *exec.ExitError.
	_, err := m.git(t.Context(), t.TempDir(), "super-secret-token", "rev-parse", "HEAD")
	if err == nil {
		t.Fatal("expected an error against an empty directory")
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("token leaked into redacted error: %v", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("errors.As(*exec.ExitError) failed through the redacted error: %v", err)
	}

	// A command run against an already-expired context must still be
	// errors.Is(context.DeadlineExceeded) through the same redaction path.
	expired, cancel := context.WithTimeout(t.Context(), 0)
	defer cancel()
	<-expired.Done()
	_, err = m.git(expired, t.TempDir(), "super-secret-token", "fetch", "origin")
	if err == nil {
		t.Fatal("expected an error against an expired context")
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("token leaked into redacted timeout error: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(context.DeadlineExceeded) failed through the redacted error: %v", err)
	}
}
