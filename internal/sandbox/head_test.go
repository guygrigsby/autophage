package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bumpBranch pushes a dependabot-shaped branch to bare, one commit ahead of
// main, and returns its head sha.
func bumpBranch(t *testing.T, bare, branch string) string {
	t.Helper()
	work := t.TempDir()
	clone := filepath.Join(work, "w")
	git(t, work, "clone", "-q", bare, clone)
	git(t, clone, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(clone, "go.mod"), []byte("module x\n\nrequire y v1.2.3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, clone, "add", "go.mod")
	git(t, clone, "commit", "-q", "-m", "bump y to v1.2.3")
	git(t, clone, "push", "-q", "origin", branch)
	return git(t, clone, "rev-parse", "HEAD")
}

// advanceMain puts a commit on main that the bump branch does not have, so a
// rebase would visibly change the tree.
func advanceMain(t *testing.T, bare string) {
	t.Helper()
	work := t.TempDir()
	clone := filepath.Join(work, "w")
	git(t, work, "clone", "-q", bare, clone)
	if err := os.WriteFile(filepath.Join(clone, "MOVED.md"), []byte("main moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, clone, "add", "MOVED.md")
	git(t, clone, "commit", "-q", "-m", "move main")
	git(t, clone, "push", "-q", "origin", "main")
}

// forcePush replaces branch on bare with a single commit carrying body, so
// the remote head is a sha nothing local has seen.
func forcePush(t *testing.T, bare, branch, body string) string {
	t.Helper()
	work := t.TempDir()
	clone := filepath.Join(work, "w")
	git(t, work, "clone", "-q", bare, clone)
	git(t, clone, "checkout", "-q", "-B", branch, "origin/main")
	if err := os.WriteFile(filepath.Join(clone, "go.mod"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, clone, "add", "go.mod")
	git(t, clone, "commit", "-q", "-m", "rebase "+body)
	git(t, clone, "push", "-q", "-f", "origin", branch)
	return git(t, clone, "rev-parse", "HEAD")
}

func TestPrepareHeadPinsTheExactShaAndDoesNotRebase(t *testing.T) {
	m := manager(t)
	bare := origin(t)
	branch := "dependabot/go_modules/y-1.2.3"
	head := bumpBranch(t, bare, branch)
	advanceMain(t, bare)

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	ws, err := m.PrepareHead(ctx, "guy/repo", bare, branch, head, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, ws.Path, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want %s: the checkout did not pin the sha CI judged", got, head)
	}
	// The whole point: rebasing onto the advanced main would change the tree
	// the verdict was about.
	if _, err := os.Stat(filepath.Join(ws.Path, "MOVED.md")); err == nil {
		t.Error("main's later commit is in the tree: PrepareHead rebased")
	}
	if ws.BaseSha != head || ws.RemoteHead != head {
		t.Errorf("BaseSha = %s, RemoteHead = %s, want both %s", ws.BaseSha, ws.RemoteHead, head)
	}
	if got := git(t, ws.Path, "branch", "--show-current"); got != branch {
		t.Errorf("branch = %s, want %s", got, branch)
	}
}

func TestPrepareHeadRefusesAShaThatIsNotOnTheRemote(t *testing.T) {
	m := manager(t)
	bare := origin(t)
	branch := "dependabot/go_modules/y-1.2.3"
	bumpBranch(t, bare, branch)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	missing := "0000000000000000000000000000000000000000"
	if _, err := m.PrepareHead(ctx, "guy/repo", bare, branch, missing, "tok"); err == nil {
		t.Fatal("PrepareHead accepted a sha the remote does not have")
	}
}

func TestPrepareHeadRefusesAMalformedSha(t *testing.T) {
	m := manager(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	// Refused before any network or disk work, so a bad sha cannot reach git
	// as an argument at all.
	if _, err := m.PrepareHead(ctx, "guy/repo", origin(t), "dependabot/x", "--upload-pack=evil", "tok"); err == nil {
		t.Fatal("PrepareHead accepted a sha that is not 40 hex")
	}
}

// A repair pushes back onto dependabot's branch under the lease PrepareHead
// recorded, so a force-push that lands mid-round loses the race instead of
// being clobbered.
func TestRepairPushLosesToADependabotForcePush(t *testing.T) {
	m := manager(t)
	bare := origin(t)
	branch := "dependabot/go_modules/y-1.2.3"
	head := bumpBranch(t, bare, branch)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	ws, err := m.PrepareHead(ctx, "guy/repo", bare, branch, head, "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer m.unlock("guy/repo", ws.lease)

	// dependabot rebases the branch out from under the round, which is a
	// force-push to a genuinely different commit.
	forcePush(t, bare, branch, "module x\n\nrequire y v1.2.3 // rebased\n")

	if err := os.WriteFile(filepath.Join(ws.Path, "go.mod"), []byte("module x\n\nrequire y v1.2.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, pushed, err := m.CommitAndPush(ctx, ws, "tok", "repair the bump")
	if pushed {
		t.Error("the repair push clobbered a force-push that landed mid-round")
	}
	if err == nil {
		t.Error("err = nil, want the lease rejection")
	} else if !strings.Contains(err.Error(), "reject") && !strings.Contains(err.Error(), "stale info") {
		t.Logf("push failed as expected: %v", err)
	}
}
