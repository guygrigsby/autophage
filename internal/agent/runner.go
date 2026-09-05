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
// answers. Detached, because the fact that a run began, the work it
// committed and the outcome it reached must survive the cancellation that
// produced them.
const (
	storeTimeout  = 30 * time.Second
	githubTimeout = 60 * time.Second
)

// errShutdown is what execute returns when the daemon is going down under a
// running attempt. It is not an end of the attempt: no outcome is recorded,
// so the attempt stays open and app.Recovery ends it with
// Aborted{DaemonRestart} on the next boot, which re-queues the case. Every
// other way an attempt ends produces an outcome.
var errShutdown = errors.New("the daemon is shutting down")

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
// failure per case and leaves the case Received.
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

// unregister takes the attempt out of flight. It removes only this run's own
// slot, so calling it twice (once when the attempt ends, once from the
// deferred backstop) cannot take a later run of the same attempt out with it.
func (r *Runner) unregister(attemptID string, slot *running) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running[attemptID] == slot {
		delete(r.running, attemptID)
	}
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

// shuttingDown reports the daemon going down under a running attempt: the
// caller's context was done and nobody asked for this attempt to stop. It is
// the one end that records no outcome, because an outcome here would spend
// the case's answer on a cancellation nothing about the attempt earned.
// callerDone is a snapshot the caller reads once rather than a live check: a
// cancellation arriving after the attempt has already finished its work is
// not what this decides.
func (r *Runner) shuttingDown(callerDone bool, slot *running) bool {
	return callerDone && r.reason(slot, "") == ""
}

// Run executes one attempt end to end. It records an outcome for every end
// but a daemon shutdown, which leaves the attempt open for app.Recovery.
func (r *Runner) Run(ctx context.Context, attemptID string) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Registered before the case is loaded, so an operator stop arriving
	// while the load is still running is not answered with "not running
	// here". The deferred unregister is the backstop for the early returns
	// below; the ordinary path unregisters as soon as execute returns.
	slot := r.register(attemptID, cancel)
	defer r.unregister(attemptID, slot)

	// The load runs on the caller's context rather than the attempt's: a
	// stop this early has nothing to stop yet, and the pipeline maps the
	// cancellation to an outcome once there is something to map.
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

	outcome, err := r.execute(runCtx, ctx, c, *a, slot)
	// Out of flight before the outcome is written: the attempt is over, so
	// a stop arriving now must say so rather than cancel a context nothing
	// is listening to any more.
	r.unregister(attemptID, slot)
	if err != nil {
		r.logf("runner %s: %v; leaving the attempt open for recovery", attemptID, err)
		return
	}
	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancelWrite()
	if _, err := r.Store.UpdateCase(writeCtx, c.Repository(), c.Number(), func(c *resolution.Case) error {
		return c.RecordOutcome(attemptID, outcome)
	}); err != nil {
		// The attempt stays open and the scheduler's backstop closes it.
		r.logf("runner %s: record outcome: %v", attemptID, err)
	}
	// Reported whether or not the row landed: the turns and the tokens were
	// spent either way, and a metric that hides the failed writes is the
	// wrong shape of missing.
	if r.Metrics != nil {
		r.Metrics.Ended(string(a.Kind), string(outcome.Kind), outcome.Usage)
	}
}

// execute is the pipeline: token, workspace, container, toolbox, the
// budgeted run, the push and the pull request. Every step's failure maps to
// an outcome. ctx is the attempt's, cancelled by Stop and Cancel; caller is
// the daemon's, and its cancellation with no recorded reason is the one end
// that returns errShutdown and no outcome at all.
func (r *Runner) execute(ctx, caller context.Context, c *resolution.Case, a resolution.Attempt, slot *running) (resolution.Outcome, error) {
	failed := func(class resolution.FailureClass, message string, u resolution.Usage) resolution.Outcome {
		o, cerr := resolution.OutcomeFailed(class, message, u, r.now())
		return r.settled(o, cerr, u)
	}
	// setup ends the attempt on a failure before the agent ran. A
	// cancellation is not an infrastructure failure: it is the stop
	// somebody asked for, or the daemon going down under the attempt.
	setup := func(step string, err error) (resolution.Outcome, error) {
		if errors.Is(err, context.Canceled) {
			if reason := r.reason(slot, ""); reason != "" {
				o, cerr := resolution.OutcomeAborted(reason, "The attempt was stopped during "+step+", before the agent ran.", resolution.Usage{}, r.now())
				return r.settled(o, cerr, resolution.Usage{}), nil
			}
			if caller.Err() != nil {
				return resolution.Outcome{}, errShutdown
			}
		}
		return failed(resolution.FailureInfra, step+": "+err.Error(), resolution.Usage{}), nil
	}

	repo, err := r.Store.GetRepository(ctx, c.Repository())
	if err != nil {
		return setup("repository", err)
	}
	token, err := r.GitHub.MintToken(ctx, repo)
	if err != nil {
		return setup("mint token", err)
	}
	repo = r.refreshDefaultBranch(ctx, repo)

	cloneURL := "https://github.com/" + repo.FullName + ".git"
	if r.CloneURL != nil {
		cloneURL = r.CloneURL(repo.FullName)
	}
	ws, err := r.Sandbox.Prepare(ctx, repo.FullName, cloneURL, c.Branch(), repo.DefaultBranch, token.Value)
	if err != nil {
		return setup("prepare workspace", err)
	}
	ctr, err := r.Sandbox.Start(ctx, ws, a.ID)
	if err != nil {
		// A failed Start releases the workspace lock itself, so only a
		// container that exists gets a teardown.
		return setup("start container", err)
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
		return setup("dial toolbox", err)
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
			return setup("model", err)
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
	summary := report.Summary
	if summary == "" {
		summary = "The agent produced no summary."
	}

	// Read once, before the work below: a shutdown that arrives while the
	// push is running must not change an answer for an attempt that has
	// already finished everything it was going to do.
	callerDone := caller.Err() != nil

	// Mint again for the push. An installation token lives an hour at most
	// (github.Client.MintToken) and an approved attempt's wall clock is
	// three, so the token Prepare used would 401 at exactly the moment
	// there is work to save. A failed mint does not skip the push: the
	// commit leg runs either way, and the token already in hand is often
	// still good. Both the mint and the push run detached from the
	// attempt's context, whatever ended the run: a stop or a model failure
	// still leaves commits worth keeping, and CommitAndPush is what ends
	// the agent phase on the workspace.
	pushCtx := context.WithoutCancel(ctx)
	mintCtx, cancelMint := context.WithTimeout(pushCtx, githubTimeout)
	fresh, mintErr := r.GitHub.MintToken(mintCtx, repo)
	cancelMint()
	pushToken := token
	if mintErr != nil {
		r.logf("runner %s: mint push token: %v", a.ID, mintErr)
	} else {
		pushToken = fresh
	}
	head, pushed, pushErr := r.Sandbox.CommitAndPush(pushCtx, ws, pushToken.Value, fmt.Sprintf("autophage: attempt %d", a.Ordinal))
	if pushErr != nil {
		r.logf("runner %s: commit and push: %v", a.ID, pushErr)
	}

	if r.shuttingDown(callerDone, slot) {
		return resolution.Outcome{}, errShutdown
	}
	// A branch that did not reach the remote is an infrastructure failure
	// whatever ended the run: the work exists on host disk only, and a
	// budget or abort outcome would send the operator to a branch that is
	// not there. A re-mint that failed is named here too, since it is the
	// likely reason the push was refused. The agent's summary rides along
	// as detail, since it is the only account of what the attempt did.
	if pushErr != nil || !pushed {
		why := "the branch did not reach the remote"
		if pushErr != nil {
			why = pushErr.Error()
		}
		if mintErr != nil {
			why += " (the push token could not be re-minted: " + mintErr.Error() + ")"
		}
		return failed(resolution.FailureInfra, "push: "+why+"\n\n"+summary, report.Usage), nil
	}

	switch report.Stop {
	case StopModelErr:
		message := "the model run ended with an error"
		if report.Err != nil {
			message = report.Err.Error()
		}
		return failed(resolution.FailureModel, message, report.Usage), nil
	case StopCancelled:
		o, cerr := resolution.OutcomeAborted(r.reason(slot, resolution.AbortOperatorStop), summary, report.Usage, r.now())
		return r.settled(o, cerr, report.Usage), nil
	case StopTurns, StopWallClock, StopDiffLines:
		// The Stop vocabulary and the Limit vocabulary are the same three
		// words on purpose; keep them equal.
		o, cerr := resolution.OutcomeExhausted(resolution.Limit(report.Stop), summary, report.Usage, r.now())
		return r.settled(o, cerr, report.Usage), nil
	}
	if head == ws.BaseSha {
		// Nothing was committed. Earlier commits on a resumed attempt's
		// branch are this attempt's work too, so the comparison is against
		// the base the branch was rebased onto, not against the remote head.
		return failed(resolution.FailureAgent, summary, report.Usage), nil
	}

	ghCtx, cancelGH := context.WithTimeout(pushCtx, githubTimeout)
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
		return failed(resolution.FailureInfra, "open pull request: "+err.Error()+"\n\n"+summary, report.Usage), nil
	}
	o, cerr := resolution.OutcomePullRequest(pr, head, summary, report.Usage, r.now())
	return r.settled(o, cerr, report.Usage), nil
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
