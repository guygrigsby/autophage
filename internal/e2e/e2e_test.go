//go:build e2e

// Package e2e drives autophaged's own components, wired as cmd/autophaged
// wires them, from a signed webhook delivery to an opened pull request.
// Everything is real except GitHub (an httptest fake) and the model (a
// scripted one): a real Postgres, a real podman container from the built
// sandbox image, a real local git remote and the real jess ledger.
package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guygrigsby/jess"
	"github.com/guygrigsby/jess/ledger"
	ac "github.com/voocel/agentcore"

	"github.com/guygrigsby/autophage/internal/agent"
	"github.com/guygrigsby/autophage/internal/api"
	"github.com/guygrigsby/autophage/internal/app"
	"github.com/guygrigsby/autophage/internal/github"
	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/sandbox"
	"github.com/guygrigsby/autophage/internal/storetest"
)

const (
	defaultBranch = "main"
	// fixFile is what the scripted agent writes and commits, and what the
	// pushed branch is checked for.
	fixFile = "FIX.md"
	// sweepFloor keeps the dispatcher moving without waiting on its minute
	// long production floor. Store notifications drive every step here; this
	// is the backstop that makes each service's "will retry" true in time.
	sweepFloor = 2 * time.Second
	// doneWait is how long the case has to reach Done. The clone, the prep
	// container and the agent container all happen inside it, so it is
	// generous, and it still leaves room inside the target's 15 minute
	// timeout for the teardown and the assertions.
	doneWait = 8 * time.Minute
)

var webhookSecret = []byte("e2e-webhook-secret")

// podmanOrSkip finds podman and the sandbox image, skipping with a printed
// reason when either is missing, exactly as internal/sandbox's container
// tests do. The image name comes from AUTOPHAGE_E2E_IMAGE.
func podmanOrSkip(t *testing.T) (bin, image string) {
	t.Helper()
	bin, err := exec.LookPath("podman")
	if err != nil {
		t.Skip("podman not on PATH; the end to end test runs on trig")
	}
	image = os.Getenv("AUTOPHAGE_E2E_IMAGE")
	if image == "" {
		image = "localhost/autophage-sandbox:latest"
	}
	if err := exec.Command(bin, "image", "exists", image).Run(); err != nil {
		t.Skipf("image %s not built; run make image", image)
	}
	return bin, image
}

// git runs one git command for the test's own bookkeeping (the bare remote
// and the assertions against it), with a fixed identity so the host's global
// config contributes nothing.
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

// bareOrigin creates the bare repository that stands in for GitHub's git
// side: one commit on main, which is what the sandbox clones and what the
// attempt's branch is pushed back to.
func bareOrigin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	git(t, root, "init", "--bare", "-b", defaultBranch, bare)
	seed := filepath.Join(root, "seed")
	git(t, root, "clone", "-q", bare, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("# seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", "README.md")
	git(t, seed, "commit", "-q", "-m", "seed")
	git(t, seed, "push", "-q", "origin", defaultBranch)
	return bare
}

const summary = "What I found: the repository has no " + fixFile + ".\n" +
	"What I did: wrote " + fixFile + " and committed it.\n" +
	"What is left: nothing.\n" +
	"What I would do with more budget: nothing."

// scriptedModel stands in for the OpenRouter tier on both routes the runner
// takes to a model. A triage call (recognised by its system prompt) answers
// with the sizing JSON; an attempt call writes the file, commits it through
// the shell and then answers with the summary in the required shape. A turn
// with no tools offered is the forced summary turn, which a real model could
// only answer with text.
func scriptedModel(t *testing.T) ac.ChatModel {
	var turn atomic.Int32
	return jess.Once(true, func(_ context.Context, msgs []ac.Message, tools []ac.ToolSpec) (*ac.LLMResponse, error) {
		if isTriage(msgs) {
			return text(`{"size": "small", "rationale": "One new file at the repository root."}`), nil
		}
		if len(tools) == 0 {
			return text(summary), nil
		}
		switch turn.Add(1) {
		case 1:
			t.Log("scripted model: writing " + fixFile)
			return call("w1", "write", map[string]any{
				"file_path": fixFile,
				"content":   "# Fix\n\nautophage wrote this file to resolve the issue.\n",
			}), nil
		case 2:
			t.Log("scripted model: committing " + fixFile)
			// No identity on the command line: the image bakes one in
			// (deploy/Containerfile), and an agent that had to supply its
			// own would be burning turns on "Author identity unknown".
			return call("b1", "bash", map[string]any{
				"command": "set -e\n" +
					"git add " + fixFile + "\n" +
					"git commit -q -m 'add " + fixFile + "'\n" +
					"git log -1 --format=%s\n",
			}), nil
		}
		t.Log("scripted model: summarising")
		return text(summary), nil
	})
}

// isTriage reports whether these messages are the triage call rather than an
// attempt turn. The triage system prompt is the one thing that is only ever
// sent by internal/agent's Triager.
func isTriage(msgs []ac.Message) bool {
	for _, m := range msgs {
		if m.Role == ac.RoleSystem && strings.Contains(m.TextContent(), "You size GitHub issues") {
			return true
		}
	}
	return false
}

func text(s string) *ac.LLMResponse {
	return &ac.LLMResponse{Message: ac.Message{Role: ac.RoleAssistant, Content: []ac.ContentBlock{ac.TextBlock(s)}, StopReason: ac.StopReasonStop}}
}

func call(id, name string, args map[string]any) *ac.LLMResponse {
	raw, err := json.Marshal(args)
	if err != nil {
		panic(err) // the arguments are literals in this file
	}
	return &ac.LLMResponse{Message: ac.Message{Role: ac.RoleAssistant,
		Content:    []ac.ContentBlock{ac.ToolCallBlock(ac.ToolCall{ID: id, Name: name, Args: raw})},
		StopReason: ac.StopReasonToolUse}}
}

// sign is the X-Hub-Signature-256 GitHub sends and the webhook handler
// verifies.
func sign(body []byte) string {
	m := hmac.New(sha256.New, webhookSecret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestEndToEnd(t *testing.T) {
	podmanBin, image := podmanOrSkip(t)
	st := storetest.Open(t)
	setupCtx, cancelSetup := context.WithTimeout(context.Background(), time.Minute)
	defer cancelSetup()

	fake, ghSrv := newFakeGitHub(t)
	bare := bareOrigin(t)
	configDir := t.TempDir()
	clock := resolution.SystemClock{}

	gh, err := github.NewClient(github.ClientConfig{AppID: 1, PrivateKeyPEM: testKey(t), BaseURL: ghSrv.URL + "/", UserAgent: "autophage-e2e",
		Installations: func(ctx context.Context, repo string) (int64, error) {
			r, err := st.GetRepository(ctx, repo)
			return r.InstallationID, err
		}})
	if err != nil {
		t.Fatal(err)
	}

	ledgerDB := st.SQLDB()
	defer func() { _ = ledgerDB.Close() }()
	pg, err := ledger.NewPostgres(ledgerDB)
	if err != nil {
		t.Fatal(err)
	}

	// The cache volumes outlive every container by design, so remove this
	// repository's on the way out. They are found by name prefix rather than
	// by recomputing internal/sandbox's slug here, which would be the same
	// fact written twice.
	t.Cleanup(func() { removeCacheVolumes(t, podmanBin) })

	budget, err := resolution.NewBudget(10, 5*time.Minute, 400)
	if err != nil {
		t.Fatal(err)
	}
	runner := &agent.Runner{
		Store:  st,
		GitHub: gh,
		Sandbox: &sandbox.Manager{Podman: podmanBin, Image: image, WorkspacesDir: t.TempDir(), Memory: "2g", CPUs: "2", Pids: 512,
			BotName: "autophage[bot]", BotEmail: "autophage[bot]@users.noreply.github.com", Logf: t.Logf},
		AttemptModel:  "scripted/auto",
		ApprovedModel: "scripted/approved",
		TriageModel:   "scripted/triage",
		Ledger:        pg,
		Clock:         clock,
		// The bare repository stands in for https://github.com/<repo>.git.
		CloneURL:      func(string) string { return bare },
		Logf:          t.Logf,
		Metrics:       api.RunnerMetrics{},
		ModelOverride: scriptedModel(t),
	}
	dispatcher := &app.Dispatcher{
		Store:      st,
		Translator: &github.Translator{Store: st, Clock: clock, ApprovedLabel: approvedLabel, BotLogin: "autophage[bot]"},
		Triage:     &app.Triage{Store: st, Triager: runner.Triager(), GitHub: gh, Clock: clock, Model: "scripted/triage"},
		Scheduler:  &app.Scheduler{Store: st, Runner: runner, Concurrency: 1, Clock: clock, Budgets: app.BudgetPolicy{Auto: budget, Approved: budget}, GitHub: gh},
		Commenter:  &app.Commenter{Store: st, GitHub: gh, Label: approvedLabel},
		Enrollment: &app.Enrollment{Store: st, GitHub: gh, Label: approvedLabel},
		Recovery:   &app.Recovery{Store: st, Clock: clock},
		Canceller:  runner,
		Floor:      sweepFloor,
	}

	daemonCtx, stopDaemon := context.WithCancel(context.Background())
	handler := api.New(configDir, nil, api.Deps{
		Base: daemonCtx, Store: st, GitHub: gh, Clock: clock, OperatorLogin: requesterLogin,
		Webhook: github.WebhookHandler(st, webhookSecret, clock),
		Sweep:   dispatcher.Sweep, Ledger: pg, Stop: runner.Stop,
		Version: "e2e", StartedAt: clock.Now(), Concurrency: 1,
	})
	daemonSrv := httptest.NewServer(handler)
	t.Cleanup(daemonSrv.Close)

	// Enrollment through the store rather than through an installation
	// delivery: this test is about the issue path, and EnrollRepository is
	// the one way in that github.Translator's own enroll step calls anyway.
	repo, err := resolution.NewRepository(repository, installationID, defaultBranch, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnrollRepository(setupCtx, repo); err != nil {
		t.Fatal(err)
	}

	var daemon sync.WaitGroup
	daemon.Add(1)
	go func() {
		defer daemon.Done()
		if err := dispatcher.Run(daemonCtx); err != nil {
			t.Errorf("dispatcher: %v", err)
		}
	}()
	// Registered first, so it runs last: the daemon stops before the fake
	// GitHub and the daemon's own server close under it.
	defer func() {
		stopDaemon()
		daemon.Wait()
	}()

	operatorToken := mintOperatorToken(t, daemonSrv.URL)
	postDelivery(t, daemonSrv.URL)

	c := waitForDone(t, st)
	attempts := c.Attempts()
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
	a := attempts[0]
	if a.Outcome == nil || a.Outcome.Kind != resolution.PullRequestOpened {
		t.Fatalf("outcome = %+v, want a pull request", a.Outcome)
	}
	if a.Run == nil || a.Run.RunID == "" {
		t.Fatal("no run id recorded for the attempt")
	}
	t.Logf("attempt %s ran %s and opened pull request %d", a.ID, a.Run.RunID, a.Outcome.PRNumber)

	// The fake saw one pull request, from the case branch to the default
	// branch, closing the issue.
	pulls := fake.pullRequests()
	if len(pulls) != 1 {
		t.Fatalf("pull requests = %d, want 1: %v", len(pulls), pulls)
	}
	branch := fmt.Sprintf("autophage/%d", issueNumber)
	if pulls[0]["head"] != branch || pulls[0]["base"] != defaultBranch {
		t.Errorf("pull request = %v, want %s into %s", pulls[0], branch, defaultBranch)
	}
	body, _ := pulls[0]["body"].(string)
	if !strings.Contains(body, fmt.Sprintf("Fixes #%d", issueNumber)) {
		t.Errorf("pull request body does not close the issue:\n%s", body)
	}

	// The work is on the remote, not just on host disk.
	if files := git(t, bare, "ls-tree", "--name-only", branch); !strings.Contains(files, fixFile) {
		t.Errorf("%s on the remote branch = %q, want %s", branch, files, fixFile)
	}

	// The ledger recorded what the agent did, and /why serves it.
	chain, err := pg.Chain(a.Run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	tools := actionTools(chain)
	if !containsAny(tools, "bash", "write") {
		t.Errorf("ledger actions = %v, want at least one bash or write", tools)
	}
	served := whyChain(t, daemonSrv.URL, operatorToken, a.ID)
	if served.RunID != a.Run.RunID {
		t.Errorf("why run id = %q, want %q", served.RunID, a.Run.RunID)
	}
	if got := actionTools(served.Chain); !equal(got, tools) {
		t.Errorf("why actions = %v, want %v", got, tools)
	}

	// The attempt's container is gone.
	name := "autophage-" + a.ID
	out, err := exec.Command(podmanBin, "ps", "-a", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("podman ps: %v", err)
	}
	for _, line := range strings.Fields(string(out)) {
		if line == name {
			t.Errorf("container %s is still there", name)
		}
	}

	// Nothing the run minted may reach a log line the operator reads.
	if len(fake.mintedTokens()) == 0 {
		t.Error("no installation token was minted, so the run did not reach GitHub")
	}
}

// mintOperatorToken takes the operator token the way the CLI does, over the
// loopback-only mint endpoint.
func mintOperatorToken(t *testing.T, base string) string {
	t.Helper()
	resp, err := http.Post(base+"/api/auth/mint", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mint: status %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Token == "" {
		t.Fatal("mint returned an empty token")
	}
	return out.Token
}

// postDelivery posts the signed issues.opened delivery to the daemon's public
// webhook endpoint, as GitHub would.
func postDelivery(t *testing.T, base string) {
	t.Helper()
	payload := map[string]any{
		"action": "opened",
		"issue": map[string]any{
			"number": issueNumber, "title": issueTitle, "body": issueBody, "state": "open",
			"author_association": "OWNER", "user": map[string]any{"login": requesterLogin},
		},
		"repository":   map[string]any{"full_name": repository, "default_branch": defaultBranch},
		"sender":       map[string]any{"login": requesterLogin},
		"installation": map[string]any{"id": installationID},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/webhook/github", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-GitHub-Delivery", "e2e-1")
	req.Header.Set("X-Hub-Signature-256", sign(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("webhook: status %d: %s", resp.StatusCode, out)
	}
}

// waitForDone polls the case until it reaches Done, failing early on a state
// that says the attempt ended some other way rather than burning the whole
// deadline on it.
func waitForDone(t *testing.T, st interface {
	GetCase(context.Context, string, int) (*resolution.Case, error)
}) *resolution.Case {
	t.Helper()
	deadline := time.Now().Add(doneWait)
	last := resolution.CaseState("")
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		c, err := st.GetCase(ctx, repository, issueNumber)
		cancel()
		if err == nil {
			if s := c.State(); s != last {
				t.Logf("case is %s", s)
				last = s
			}
			switch c.State() {
			case resolution.Done:
				return c
			case resolution.Failed, resolution.AwaitingApproval, resolution.Gated, resolution.Closed:
				t.Fatalf("case reached %s, not done: %s", c.State(), outcomeDetail(c))
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("case is %s after %s, not done", last, doneWait)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// outcomeDetail is what the last attempt said, for a failure message worth
// reading.
func outcomeDetail(c *resolution.Case) string {
	attempts := c.Attempts()
	if len(attempts) == 0 {
		return "no attempt ran"
	}
	o := attempts[len(attempts)-1].Outcome
	if o == nil {
		return "the last attempt is still open"
	}
	return fmt.Sprintf("%s: %s %s", o.Kind, o.Message, o.Summary)
}

// whyChain reads /api/attempts/<id>/why with the operator token.
func whyChain(t *testing.T, base, token, attemptID string) struct {
	AttemptID string       `json:"attempt_id"`
	RunID     string       `json:"run_id"`
	Chain     ledger.Chain `json:"chain"`
} {
	t.Helper()
	var out struct {
		AttemptID string       `json:"attempt_id"`
		RunID     string       `json:"run_id"`
		Chain     ledger.Chain `json:"chain"`
	}
	req, err := http.NewRequest(http.MethodGet, base+"/api/attempts/"+attemptID+"/why", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("why: status %d: %s", resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func actionTools(chain ledger.Chain) []string {
	out := make([]string, 0, len(chain.Actions))
	for _, a := range chain.Actions {
		out = append(out, a.Intent.Tool)
	}
	return out
}

func containsAny(have []string, want ...string) bool {
	for _, h := range have {
		for _, w := range want {
			if h == w {
				return true
			}
		}
	}
	return false
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// removeCacheVolumes deletes the repository's three cache volumes. They are
// named autophage-cache-<tool>-<slug> by internal/sandbox, where the slug
// starts with the repository with its slash replaced; the prefix filter
// finds all three without this test having to recompute that slug.
func removeCacheVolumes(t *testing.T, podmanBin string) {
	t.Helper()
	prefix := "autophage-cache-"
	out, err := exec.Command(podmanBin, "volume", "ls", "-q").Output()
	if err != nil {
		t.Logf("podman volume ls: %v", err)
		return
	}
	slug := strings.ReplaceAll(repository, "/", "-")
	for _, name := range strings.Fields(string(out)) {
		if strings.HasPrefix(name, prefix) && strings.Contains(name, slug) {
			if err := exec.Command(podmanBin, "volume", "rm", "-f", name).Run(); err != nil {
				t.Logf("podman volume rm %s: %v", name, err)
			}
		}
	}
}
