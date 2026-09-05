package sandbox

import (
	"context"
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
	ws, _ := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err := os.WriteFile(filepath.Join(ws.Path, "README.md"), []byte("# ours\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.CommitAndPush(ctx, ws, "tok", "ours"); err != nil {
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
