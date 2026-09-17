package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	ac "github.com/voocel/agentcore"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/sandbox"
	"github.com/guygrigsby/autophage/internal/store"
	"github.com/guygrigsby/autophage/internal/upkeep"
	"github.com/guygrigsby/jess/ledger"
)

// Repairer takes a started repair round from token to outcome. It is the
// Upkeep counterpart of Runner and shares its machinery (RunAttempt, the
// model tiers, the step metrics) without sharing its pipeline: a repair
// works a branch autophage did not create, pins the sha CI judged, and
// stops at the push because the pull request already exists.
type Repairer struct {
	Store   *store.Store
	GitHub  resolution.GitHub
	Sandbox sandbox.Sandbox
	Models  Models
	// Model is the OpenRouter id repair rounds run on.
	Model    string
	RoundCap int
	Ledger   ledger.DurableSink
	Clock    resolution.Clock
	// CloneURL builds the origin the sandbox clones and pushes; nil means
	// https://github.com/<repository>.git.
	CloneURL func(repository string) string
	Logf     func(string, ...any)
	Metrics  Metrics
	// ModelOverride replaces the built tier. Test-only, exactly as Runner's.
	ModelOverride ac.ChatModel

	mu      sync.Mutex
	running map[string]*repairInFlight
}

// repairInFlight is one round in flight: the cancel func Stop and Cancel
// call and the reason they recorded.
type repairInFlight struct {
	cancel context.CancelFunc
	reason upkeep.RepairAbortReason
}

func (p *Repairer) metrics() Metrics {
	if p.Metrics == nil {
		return NopMetrics{}
	}
	return p.Metrics
}

func (p *Repairer) tier() (ac.ChatModel, error) {
	if p.ModelOverride != nil {
		return p.ModelOverride, nil
	}
	return p.Models.Tier(p.Model)
}

func (p *Repairer) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}

func (p *Repairer) now() time.Time {
	if p.Clock != nil {
		return p.Clock.Now()
	}
	return time.Now().UTC()
}

// Stop cancels a running round as an operator stop.
func (p *Repairer) Stop(repairID string) bool {
	return p.cancelWith(repairID, upkeep.AbortOperatorStop)
}

// Cancel cancels a running round with a named reason, for a pull request
// that closed out from under it.
func (p *Repairer) Cancel(repairID string, reason upkeep.RepairAbortReason) bool {
	return p.cancelWith(repairID, reason)
}

func (p *Repairer) cancelWith(repairID string, reason upkeep.RepairAbortReason) bool {
	p.mu.Lock()
	slot, ok := p.running[repairID]
	if ok {
		slot.reason = reason
	}
	p.mu.Unlock()
	if !ok {
		return false
	}
	slot.cancel()
	return true
}

func (p *Repairer) register(repairID string, cancel context.CancelFunc) *repairInFlight {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running == nil {
		p.running = map[string]*repairInFlight{}
	}
	slot := &repairInFlight{cancel: cancel}
	p.running[repairID] = slot
	return slot
}

// unregister removes the round from the registry, but only if it is still
// the one registered: a second call after the round ended must not evict a
// later round that reused the map entry.
func (p *Repairer) unregister(repairID string, slot *repairInFlight) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running[repairID] == slot {
		delete(p.running, repairID)
	}
}

// reason reads the abort reason a stop recorded, or fallback.
func (p *Repairer) reason(slot *repairInFlight, fallback upkeep.RepairAbortReason) upkeep.RepairAbortReason {
	p.mu.Lock()
	defer p.mu.Unlock()
	if slot.reason != "" {
		return slot.reason
	}
	return fallback
}

// shuttingDown reports whether the round ended because the daemon is going
// down with nobody having asked for a stop, which is the one end that
// records no outcome at all.
func (p *Repairer) shuttingDown(callerDone bool, slot *repairInFlight) bool {
	return callerDone && p.reason(slot, "") == ""
}

// Run executes one started round and records its outcome.
func (p *Repairer) Run(ctx context.Context, repairID string) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	slot := p.register(repairID, cancel)
	defer p.unregister(repairID, slot)

	b, err := p.Store.GetBumpByRepair(ctx, repairID)
	if err != nil {
		p.logf("repair %s: %v", repairID, err)
		return
	}
	a := b.OpenRepair()
	if a == nil || a.ID != repairID {
		p.logf("repair %s: round is not open", repairID)
		return
	}

	outcome, err := p.execute(runCtx, ctx, b, *a, slot)
	p.unregister(repairID, slot)
	if err != nil {
		p.logf("repair %s: %v; leaving the round open for recovery", repairID, err)
		return
	}
	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancelWrite()
	if _, err := p.Store.UpdateBumpByRepair(writeCtx, repairID, func(b *upkeep.Bump) error {
		return b.RecordOutcome(repairID, outcome, p.RoundCap)
	}); err != nil {
		// The round stays open and the scheduler's backstop closes it.
		p.logf("repair %s: record outcome: %v", repairID, err)
	}
}

// execute is the repair pipeline: token, workspace pinned to the failing
// sha, container, toolbox, the budgeted run, the push. Every step's failure
// maps to an outcome. There is no pull request step: the pull request is
// dependabot's and already exists.
func (p *Repairer) execute(ctx, caller context.Context, b *upkeep.Bump, a upkeep.RepairAttempt, slot *repairInFlight) (upkeep.RepairOutcome, error) {
	m := p.metrics()
	failed := func(class resolution.FailureClass, message string, u resolution.Usage) upkeep.RepairOutcome {
		return upkeep.RepairOutcome{Kind: upkeep.RepairFailed, Class: class, Message: message, Summary: message, Usage: u, EndedAt: p.now()}
	}
	// infra ends the round on an infrastructure failure. The raw error is
	// logged against the round id and never put in the outcome: a host path,
	// a git argv or a podman mount flag is detail the operator would rather
	// have from `autophage why`, and an outcome message can reach a comment.
	infra := func(step string, err error, detail string, u resolution.Usage) upkeep.RepairOutcome {
		if err != nil {
			p.logf("repair %s: %s: %v", a.ID, step, err)
		}
		message := fmt.Sprintf("infrastructure failure at %s; see `autophage why %s` on the daemon host", step, a.ID)
		if detail != "" {
			message += "\n\n" + detail
		}
		return failed(resolution.FailureInfra, message, u)
	}
	// setup ends the round on a failure before the agent ran. A cancellation
	// is the stop somebody asked for, or the daemon going down; which it was
	// is decided by the context at the point of failure, never by the shape
	// of the error, since a killed git reports its own ExitError rather than
	// the context's.
	setup := func(step string, err error) (upkeep.RepairOutcome, error) {
		if ctx.Err() != nil {
			if reason := p.reason(slot, ""); reason != "" {
				return upkeep.RepairOutcome{
					Kind: upkeep.RepairAborted, Reason: reason,
					Summary: "The round was stopped during " + step + ", before the agent ran: " + err.Error(),
					EndedAt: p.now(),
				}, nil
			}
			if caller.Err() != nil {
				p.logf("repair %s: %s: %v", a.ID, step, err)
				return upkeep.RepairOutcome{}, errShutdown
			}
		}
		return infra(step, err, "", resolution.Usage{}), nil
	}

	repo, err := p.Store.GetRepository(ctx, b.Repository())
	if err != nil {
		return setup("repository", err)
	}
	token, err := timeStep(m, stepMint, func() (resolution.Token, error) { return p.GitHub.MintToken(ctx, repo) })
	if err != nil {
		return setup("mint token", err)
	}
	cloneURL := "https://github.com/" + repo.FullName + ".git"
	if p.CloneURL != nil {
		cloneURL = p.CloneURL(repo.FullName)
	}
	// PrepareHead, not Prepare: the round works the exact tree CI judged, so
	// nothing here rebases onto the default branch.
	ws, err := timeStep(m, stepPrepare, func() (sandbox.Workspace, error) {
		return p.Sandbox.PrepareHead(ctx, repo.FullName, cloneURL, b.Branch(), a.BaseSha, token.Value)
	})
	if err != nil {
		return setup("prepare workspace", err)
	}
	ctr, err := timeStep(m, stepStart, func() (sandbox.Container, error) { return p.Sandbox.Start(ctx, ws, a.ID) })
	if err != nil {
		return setup("start container", err)
	}
	defer func() {
		start := time.Now()
		err := p.Sandbox.Teardown(context.WithoutCancel(ctx), ctr)
		m.Step(stepTeardown, time.Since(start), err)
		if err != nil {
			p.logf("repair %s: teardown %s: %v", a.ID, ctr.Name, err)
		}
	}()
	toolsStart := time.Now()
	tools, closer, err := p.Sandbox.Tools(ctx, ctr)
	m.Step(stepTools, time.Since(toolsStart), err)
	if err != nil {
		return setup("dial toolbox", err)
	}
	// Closed after CommitAndPush: the closer reaps the podman exec child the
	// toolbox runs in, and CommitAndPush is what removes the container that
	// child lives in. Deferred second, so it runs before the teardown above.
	defer func() { _ = closer.Close() }()

	model, err := p.tier()
	if err != nil {
		return setup("model", err)
	}

	callerDone := caller.Err() != nil
	runStart := time.Now()
	report := RunAttempt(ctx, RunInput{
		Model:     model,
		ModelID:   p.Model,
		Tools:     tools,
		Ledger:    p.Ledger,
		Budget:    a.Budget,
		Brief:     a.Brief,
		AgentID:   fmt.Sprintf("autophage/%s#%d/repair/%d", b.Repository(), b.Number(), a.Round),
		DiffLines: func(ctx context.Context) (int, error) { return p.Sandbox.DiffLines(ctx, ctr, ws.BaseSha) },
		Clock:     p.Clock,
		Logf:      p.Logf,
		OnRunBegan: func(runID string) {
			p.recordRun(ctx, a.ID, resolution.Run{RunID: runID, Model: p.Model, BaseSha: ws.BaseSha, BeganAt: p.now()})
		},
		Metrics:      m,
		ModelMetrics: p.Models.metrics(),
	})
	m.Step(stepRun, time.Since(runStart), report.Err)
	if report.Stop == StopCancelled {
		callerDone = callerDone || caller.Err() != nil
	}
	summary := report.Summary
	if summary == "" {
		summary = "The agent produced no summary."
	}

	// Mint again for the push, for the same reason the issue side does: an
	// installation token lives an hour at most, and the token Prepare used
	// would 401 at exactly the moment there is work to save. Detached from
	// the round's context whatever ended the run, since a stop still leaves
	// commits worth keeping and CommitAndPush is what ends the agent phase.
	pushCtx := context.WithoutCancel(ctx)
	mintCtx, cancelMint := context.WithTimeout(pushCtx, remintTimeout)
	remintStart := time.Now()
	fresh, mintErr := p.GitHub.MintToken(mintCtx, repo)
	cancelMint()
	m.Step(stepRemint, time.Since(remintStart), mintErr)
	pushToken := token
	if mintErr != nil {
		p.logf("repair %s: mint push token: %v", a.ID, mintErr)
	} else {
		pushToken = fresh
	}
	pushStart := time.Now()
	head, pushed, pushErr := p.Sandbox.CommitAndPush(pushCtx, ws, pushToken.Value, fmt.Sprintf("autophage: repair round %d", a.Round))
	stepErr := pushErr
	if stepErr == nil && !pushed {
		stepErr = errUnpushedBranch
	}
	m.Step(stepPush, time.Since(pushStart), stepErr)
	if pushErr != nil {
		p.logf("repair %s: commit and push: %v", a.ID, pushErr)
	}

	if p.shuttingDown(callerDone, slot) {
		return upkeep.RepairOutcome{}, errShutdown
	}
	// A branch that did not reach the remote is an infrastructure failure
	// whatever ended the run: the work exists on host disk only, so calling
	// it a push would leave the bump waiting on a sha nobody has.
	if pushErr != nil || !pushed {
		why := stepErr
		if mintErr != nil {
			why = fmt.Errorf("%w (the push token could not be re-minted: %v)", why, mintErr)
		}
		return infra("push", why, summary, report.Usage), nil
	}

	switch report.Stop {
	case StopModelErr:
		if report.Err != nil {
			p.logf("repair %s: model run: %v", a.ID, report.Err)
		}
		message := fmt.Sprintf("model failure; see `autophage why %s` on the daemon host\n\n%s", a.ID, summary)
		return failed(resolution.FailureModel, message, report.Usage), nil
	case StopCancelled:
		return upkeep.RepairOutcome{
			Kind: upkeep.RepairAborted, Reason: p.reason(slot, upkeep.AbortOperatorStop),
			Summary: summary, Usage: report.Usage, EndedAt: p.now(),
		}, nil
	case StopTurns, StopWallClock, StopDiffLines:
		return upkeep.RepairOutcome{
			Kind: upkeep.BudgetExhausted, Limit: resolution.Limit(report.Stop),
			Summary: summary, Usage: report.Usage, EndedAt: p.now(),
		}, nil
	}
	if head == ws.BaseSha {
		// Nothing was committed. On the issue side that is the agent failing
		// to do the one thing it was asked; here it is an answer: the agent
		// ran, looked at the break and had nothing to offer, so another
		// round would produce the same nothing.
		return upkeep.RepairOutcome{Kind: upkeep.NoChange, Summary: summary, Usage: report.Usage, EndedAt: p.now()}, nil
	}

	// The round started from a clean pinned checkout, so any conflict
	// markers in the tree are the agent's own. Checked after CommitAndPush,
	// which is what removed the container and made the commits this reads,
	// and failing closed: the branch is already pushed either way, and
	// markers on it become the next check run's failure on somebody else's
	// pull request.
	marked, err := timeStep(m, stepConflicts, func() (bool, error) { return p.Sandbox.ConflictMarkers(pushCtx, ws) })
	if err != nil {
		return infra("conflict check", err, summary, report.Usage), nil
	}
	if marked {
		return failed(resolution.FailureAgent, "left unresolved conflict markers on the branch\n\n"+summary, report.Usage), nil
	}

	return upkeep.RepairOutcome{Kind: upkeep.Pushed, HeadSha: head, Summary: summary, Usage: report.Usage, EndedAt: p.now()}, nil
}

// recordRun writes the run row as soon as the agent begins, so
// `autophage why` has something to read for a round that never finishes.
// Detached from the round's context: a stop must not lose the run id.
func (p *Repairer) recordRun(ctx context.Context, repairID string, run resolution.Run) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	if _, err := p.Store.UpdateBumpByRepair(writeCtx, repairID, func(b *upkeep.Bump) error {
		return b.RecordRun(repairID, run)
	}); err != nil {
		p.logf("repair %s: record run: %v", repairID, err)
	}
}

// Ensure the app's port is satisfied.
var _ interface {
	Run(ctx context.Context, repairID string)
} = (*Repairer)(nil)
