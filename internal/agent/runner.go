package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/guygrigsby/jess/ledger"
	ac "github.com/voocel/agentcore"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/sandbox"
	"github.com/guygrigsby/autophage/internal/store"
)

// storeTimeout and githubTimeout bound the writes and calls the runner makes
// on a context detached from the attempt's, so an operator stop or a
// shutdown cannot be held open for ever by a database or an API that never
// answers. Detached, because the fact that a run began and the outcome it
// reached must survive the cancellation that produced them.
const (
	storeTimeout  = 30 * time.Second
	githubTimeout = 60 * time.Second
)

// Metrics is what the runner reports; the api package's Prometheus vectors
// implement it in the daemon.
type Metrics interface {
	Ended(kind, outcome string, usage resolution.Usage)
}

// Runner takes a started attempt from token to outcome. It implements
// resolution.Runner and owns the running attempts, so the operator can stop
// one and a closed issue can cancel one.
type Runner struct {
	Store   *store.Store
	GitHub  resolution.GitHub
	Sandbox sandbox.Sandbox
	Models  Models
	// AttemptModel is the OpenRouter id attempts run on. ApprovedModel
	// overrides it for an approved attempt when set; TriageModel is the
	// tier Triager sizes on.
	AttemptModel  string
	ApprovedModel string
	TriageModel   string
	Ledger        ledger.DurableSink
	Clock         resolution.Clock
	// CloneURL builds the origin the sandbox clones and pushes; nil means
	// https://github.com/<repository>.git.
	CloneURL func(repository string) string
	Logf     func(string, ...any)
	Metrics  Metrics

	model   ac.ChatModel // test seam; nil in production
	mu      sync.Mutex
	running map[string]*running
}

// running is one attempt in flight: the cancel func Stop and Cancel call and
// the reason they recorded. reason is read and written under Runner.mu,
// since the two happen on different goroutines.
type running struct {
	cancel context.CancelFunc
	reason resolution.AbortReason
}

var _ resolution.Runner = (*Runner)(nil)

// Triager builds the triage-tier triager. A model that cannot be built is
// reported from Classify rather than at wiring time: the daemon builds this
// once at startup, and a missing key must leave every case Received for the
// next sweep instead of taking the process down.
func (r *Runner) Triager() resolution.Triager {
	m, err := r.Models.Tier(r.TriageModel)
	if err != nil {
		return brokenTriager{err}
	}
	return &Triager{Model: m, ModelID: r.TriageModel}
}

// brokenTriager reports the model it could not build. app.Triage logs the
// failure per case and leaves the case Received, so the daemon recovers on
// its own once the key or the model id is fixed.
type brokenTriager struct{ err error }

func (b brokenTriager) Classify(context.Context, string, string) (resolution.Triage, error) {
	return resolution.Triage{}, b.err
}

// Stop cancels a running attempt as an operator stop, reporting whether the
// attempt is running here.
func (r *Runner) Stop(attemptID string) bool {
	return r.Cancel(attemptID, resolution.AbortOperatorStop)
}

// Cancel cancels a running attempt with the given reason, reporting whether
// the attempt is running here.
func (r *Runner) Cancel(attemptID string, reason resolution.AbortReason) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.running[attemptID]
	if !ok {
		return false
	}
	run.reason = reason
	run.cancel()
	return true
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *Runner) now() time.Time {
	if r.Clock == nil {
		return resolution.SystemClock{}.Now()
	}
	return r.Clock.Now()
}

// register puts the attempt in flight so Stop and Cancel can reach it.
func (r *Runner) register(attemptID string, cancel context.CancelFunc) *running {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running == nil {
		r.running = map[string]*running{}
	}
	slot := &running{cancel: cancel}
	r.running[attemptID] = slot
	return slot
}

func (r *Runner) unregister(attemptID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, attemptID)
}

// reason is the abort reason Cancel recorded, or fallback when the run ended
// for a cancellation nobody here asked for.
func (r *Runner) reason(slot *running, fallback resolution.AbortReason) resolution.AbortReason {
	r.mu.Lock()
	defer r.mu.Unlock()
	if slot.reason == "" {
		return fallback
	}
	return slot.reason
}

// Run executes one attempt end to end and always records an outcome.
func (r *Runner) Run(ctx context.Context, attemptID string) {
	c, err := r.Store.GetCaseByAttempt(ctx, attemptID)
	if err != nil {
		r.logf("runner %s: %v", attemptID, err)
		return
	}
	a := c.OpenAttempt()
	if a == nil || a.ID != attemptID {
		r.logf("runner %s: attempt is not open", attemptID)
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	slot := r.register(attemptID, cancel)
	defer r.unregister(attemptID)

	outcome := r.execute(runCtx, c, *a, slot)

	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancelWrite()
	if _, err := r.Store.UpdateCase(writeCtx, c.Repository(), c.Number(), func(c *resolution.Case) error {
		return c.RecordOutcome(attemptID, outcome)
	}); err != nil {
		// The attempt stays open and the scheduler's backstop closes it.
		r.logf("runner %s: record outcome: %v", attemptID, err)
		return
	}
	if r.Metrics != nil {
		r.Metrics.Ended(string(a.Kind), string(outcome.Kind), outcome.Usage)
	}
}

// execute is the pipeline: token, workspace, container, toolbox, the
// budgeted run, the push and the pull request. Every step's failure maps to
// an outcome, so the attempt never ends without one.
func (r *Runner) execute(ctx context.Context, c *resolution.Case, a resolution.Attempt, slot *running) resolution.Outcome {
	infra := func(step string, err error, u resolution.Usage) resolution.Outcome {
		o, cerr := resolution.OutcomeFailed(resolution.FailureInfra, step+": "+err.Error(), u, r.now())
		return r.settled(o, cerr, u)
	}
	repo, err := r.Store.GetRepository(ctx, c.Repository())
	if err != nil {
		return infra("repository", err, resolution.Usage{})
	}
	token, err := r.GitHub.MintToken(ctx, repo)
	if err != nil {
		return infra("mint token", err, resolution.Usage{})
	}
	repo = r.refreshDefaultBranch(ctx, repo)

	cloneURL := "https://github.com/" + repo.FullName + ".git"
	if r.CloneURL != nil {
		cloneURL = r.CloneURL(repo.FullName)
	}
	ws, err := r.Sandbox.Prepare(ctx, repo.FullName, cloneURL, c.Branch(), repo.DefaultBranch, token.Value)
	if err != nil {
		return infra("prepare workspace", err, resolution.Usage{})
	}
	ctr, err := r.Sandbox.Start(ctx, ws, a.ID)
	if err != nil {
		// A failed Start releases the workspace lock itself, so only a
		// container that exists gets a teardown.
		return infra("start container", err, resolution.Usage{})
	}
	// The Container carries the Workspace Prepare made, including the lock
	// lease Teardown releases: pass back what Start returned, unchanged.
	defer func() {
		if err := r.Sandbox.Teardown(context.WithoutCancel(ctx), ctr); err != nil {
			r.logf("runner %s: teardown %s: %v", a.ID, ctr.Name, err)
		}
	}()
	tools, closer, err := r.Sandbox.Tools(ctx, ctr)
	if err != nil {
		return infra("dial toolbox", err, resolution.Usage{})
	}
	// Closed after CommitAndPush rather than before it: the closer reaps the
	// podman exec child the toolbox runs in, and CommitAndPush is what
	// removes the container that child lives in. Deferred second, so it runs
	// before the teardown above.
	defer func() { _ = closer.Close() }()

	modelID := r.AttemptModel
	if a.Kind == resolution.Approved && r.ApprovedModel != "" {
		modelID = r.ApprovedModel
	}
	model := r.model
	if model == nil {
		model, err = r.Models.Tier(modelID)
		if err != nil {
			return infra("model", err, resolution.Usage{})
		}
	}

	report := RunAttempt(ctx, RunInput{
		Model:   model,
		Tools:   tools,
		Ledger:  r.Ledger,
		Budget:  a.Budget,
		Brief:   a.Brief,
		AgentID: fmt.Sprintf("autophage/%s#%d/%d", c.Repository(), c.Number(), a.Ordinal),
		// Advisory, and counted inside the container while it is still
		// alive: RunAttempt takes the final count itself, before this
		// returns and CommitAndPush removes the container.
		DiffLines: func(ctx context.Context) (int, error) { return r.Sandbox.DiffLines(ctx, ctr, ws.BaseSha) },
		Clock:     r.Clock,
		Logf:      r.Logf,
		OnRunBegan: func(runID string) {
			r.recordRun(ctx, c, a.ID, resolution.Run{RunID: runID, Model: modelID, BaseSha: ws.BaseSha, BeganAt: r.now()})
		},
	})

	// Always push, whatever ended the run: a stop or a model failure still
	// leaves the agent's commits worth keeping, and CommitAndPush is what
	// ends the agent phase on the workspace.
	head, pushed, pushErr := r.Sandbox.CommitAndPush(context.WithoutCancel(ctx), ws, token.Value, fmt.Sprintf("autophage: attempt %d", a.Ordinal))
	if pushErr != nil {
		// Reported as the outcome only when nothing better happened below:
		// a stop or a model failure is the more useful answer, and the
		// operator sees this line either way.
		r.logf("runner %s: commit and push: %v", a.ID, pushErr)
	}

	summary := report.Summary
	if summary == "" {
		summary = "The agent produced no summary."
	}
	switch report.Stop {
	case StopModelErr:
		message := "the model run ended with an error"
		if report.Err != nil {
			message = report.Err.Error()
		}
		o, err := resolution.OutcomeFailed(resolution.FailureModel, message, report.Usage, r.now())
		return r.settled(o, err, report.Usage)
	case StopCancelled:
		o, err := resolution.OutcomeAborted(r.reason(slot, resolution.AbortOperatorStop), summary, report.Usage, r.now())
		return r.settled(o, err, report.Usage)
	case StopTurns, StopWallClock, StopDiffLines:
		// The Stop vocabulary and the Limit vocabulary are the same three
		// words on purpose; keep them equal.
		o, err := resolution.OutcomeExhausted(resolution.Limit(report.Stop), summary, report.Usage, r.now())
		return r.settled(o, err, report.Usage)
	}
	if pushErr != nil {
		return infra("push", pushErr, report.Usage)
	}
	if !pushed {
		return infra("push", errors.New("the branch did not reach the remote"), report.Usage)
	}
	if head == ws.BaseSha {
		// Nothing was committed. Earlier commits on a resumed attempt's
		// branch are this attempt's work too, so the comparison is against
		// the base the branch was rebased onto, not against the remote head.
		o, err := resolution.OutcomeFailed(resolution.FailureAgent, summary, report.Usage, r.now())
		return r.settled(o, err, report.Usage)
	}

	ghCtx, cancelGH := context.WithTimeout(context.WithoutCancel(ctx), githubTimeout)
	defer cancelGH()
	title := fmt.Sprintf("autophage: issue #%d", c.Number())
	if detail, err := r.GitHub.GetIssue(ghCtx, c.Repository(), c.Number()); err != nil {
		r.logf("runner %s: issue title: %v", a.ID, err)
	} else if t := strings.TrimSpace(detail.Title); t != "" {
		title = "autophage: " + truncate(t, 70)
	}
	body := fmt.Sprintf("%s\n\nFixes #%d", summary, c.Number())
	pr, err := r.GitHub.OpenPullRequest(ghCtx, c.Repository(), c.Branch(), repo.DefaultBranch, title, body)
	if err != nil {
		// The branch is pushed, so a retry after approval resumes from it.
		return infra("open pull request", err, report.Usage)
	}
	o, err := resolution.OutcomePullRequest(pr, head, summary, report.Usage, r.now())
	return r.settled(o, err, report.Usage)
}

// refreshDefaultBranch corrects what enrollment stored, since the
// installation payloads that enroll a repository do not carry the default
// branch. A removed repository is corrected in memory but not written back:
// EnrollRepository also clears the removal row, and a branch name is no
// reason to undo a removal.
func (r *Runner) refreshDefaultBranch(ctx context.Context, repo resolution.Repository) resolution.Repository {
	branch, err := r.GitHub.DefaultBranch(ctx, repo)
	if err != nil {
		r.logf("runner: default branch for %s: %v", repo.FullName, err)
		return repo
	}
	if branch == "" || branch == repo.DefaultBranch {
		return repo
	}
	repo.DefaultBranch = branch
	if !repo.Enrolled() {
		return repo
	}
	if err := r.Store.EnrollRepository(ctx, repo); err != nil {
		r.logf("runner: record default branch %s for %s: %v", branch, repo.FullName, err)
	}
	return repo
}

// recordRun marks the attempt as having reached the agent. A failure to
// record is logged and the run continues: the outcome is what the case needs
// to move, and the run id is only how the operator finds the ledger.
func (r *Runner) recordRun(ctx context.Context, c *resolution.Case, attemptID string, run resolution.Run) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	if _, err := r.Store.UpdateCase(writeCtx, c.Repository(), c.Number(), func(c *resolution.Case) error {
		return c.RecordRun(attemptID, run)
	}); err != nil {
		r.logf("runner %s: record run: %v", attemptID, err)
	}
}

// settled returns o, or an infra failure saying why the domain refused it.
// Only a value the pipeline computed can be refused here (a pull request
// number or a head sha that GitHub or the sandbox got wrong); the classes,
// limits and reasons are constants. Nothing may leave execute without an
// outcome: an attempt with none stays open until the scheduler's backstop
// notices.
func (r *Runner) settled(o resolution.Outcome, err error, u resolution.Usage) resolution.Outcome {
	if err == nil {
		return o
	}
	r.logf("runner: outcome refused: %v", err)
	failed, ferr := resolution.OutcomeFailed(resolution.FailureInfra, "outcome refused: "+err.Error(), u, r.now())
	if ferr != nil {
		r.logf("runner: failure outcome refused: %v", ferr)
	}
	return failed
}
