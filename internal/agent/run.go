package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/guygrigsby/jess"
	"github.com/guygrigsby/jess/ledger"
	ac "github.com/voocel/agentcore"

	"github.com/guygrigsby/autophage/internal/resolution"
)

const systemPrompt = `You are autophage, an unattended coding agent working in a sandboxed checkout at /work with no network. Your tools are the repository's files and a shell. Decide and act; nobody will answer questions. Commit as you go with git. When told to wrap up, commit what you have and reply with the final summary in the required shape.`

const wrapUp = "Budget is nearly used up. Stop investigating. Commit what you have now with git, then reply with your final summary in the required shape."

const summaryPrompt = "The run has ended. Reply now with only your final summary in exactly this shape:\n\n" + resolution.SummaryShape

// RunInput is everything one attempt run needs.
type RunInput struct {
	Model      ac.ChatModel
	Tools      []ac.Tool
	Ledger     ledger.DurableSink
	Budget     resolution.Budget
	Brief      string
	AgentID    string // "autophage/<repo>#<n>/<ordinal>"
	DiffLines  func(ctx context.Context) (int, error)
	Clock      resolution.Clock
	Logf       func(string, ...any)
	OnRunBegan func(runID string) // called once, before the first model call returns
}

// Stop says why a run ended before the agent finished on its own.
type Stop string

const (
	StopNone      Stop = ""
	StopTurns     Stop = "turns"
	StopWallClock Stop = "wall_clock"
	StopDiffLines Stop = "diff_lines"
	StopCancelled Stop = "cancelled"
	StopModelErr  Stop = "model_error"
)

// RunReport is the outcome of one budgeted attempt run.
type RunReport struct {
	RunID   string
	Usage   resolution.Usage
	Summary string // the final message in the fixed shape, or the best text available
	Stop    Stop
	Err     error // set with StopModelErr
}

// RunAttempt drives one budgeted jess run and always returns a report, even
// when the model failed or the caller cancelled.
func RunAttempt(ctx context.Context, in RunInput) RunReport {
	logf := in.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	clock := in.Clock
	if clock == nil {
		clock = resolution.SystemClock{}
	}
	diffLines := in.DiffLines
	if diffLines == nil {
		diffLines = func(context.Context) (int, error) { return 0, nil }
	}
	started := clock.Now()
	capture := &captureRun{DurableSink: in.Ledger, began: in.OnRunBegan}
	flag := &stopFlag{}
	warnTurns, warnWall := in.Budget.WarnAt()

	var agent *ac.Agent
	tools := make([]ac.Tool, 0, len(in.Tools))
	for _, t := range in.Tools {
		tools = append(tools, &budgetTool{Tool: t, limit: in.Budget.MaxDiffLines(), diffLines: diffLines, stop: flag, abort: func() { agent.Abort() }, logf: logf})
	}
	// Turn steer must fire in lockstep with the loop, not react to it: jess
	// forwards events to this function over two buffered channels, so a fast
	// (or scripted) model can finish every turn before this call ever reads
	// the first event, and the steer would land after the run is already
	// over. ac.WithOnMessage runs synchronously inside the loop's own
	// goroutine as each assistant message is committed, one call per turn,
	// strictly before that turn's steering queue is polled, so it cannot
	// miss the window. mainRunEnded stops it from firing during the forced
	// summary turn, which reuses the same agent (and so the same hook) but
	// must never be told to wrap up early.
	var turnsSeen atomic.Int32
	var turnSteered atomic.Bool
	var mainRunEnded atomic.Bool
	onMessage := func(msg ac.AgentMessage) {
		if mainRunEnded.Load() || msg.GetRole() != ac.RoleAssistant {
			return
		}
		if n := turnsSeen.Add(1); int(n) >= warnTurns && turnSteered.CompareAndSwap(false, true) {
			agent.Steer(ac.UserMsg(wrapUp))
		}
	}
	agent = jess.New(
		jess.WithModel(in.Model),
		jess.WithTools(tools...),
		jess.WithLedger(capture),
		jess.WithAgentID(in.AgentID),
		jess.WithSystemPrompt(systemPrompt),
		jess.WithMaxTurns(in.Budget.MaxTurns()),
		jess.AllowAll(),
		jess.WithAgentcoreOptions(ac.WithMaxToolErrors(25), ac.WithOnMessage(onMessage)),
	)
	defer jess.ReleaseAgent(agent)

	runCtx, cancel := context.WithTimeout(ctx, in.Budget.MaxWallClock())
	defer cancel()
	wallTimer := time.AfterFunc(warnWall, func() { agent.Steer(ac.UserMsg(wrapUp)) })
	defer wallTimer.Stop()

	turns := 0
	var last string
	var modelErr error
	events, wait := jess.Stream(runCtx, agent, in.Brief)
	for ev := range events {
		switch ev.Type {
		case ac.EventTurnEnd:
			turns++
		case ac.EventMessageEnd:
			if t := assistantText(ev); t != "" {
				last = t
			}
		case ac.EventError:
			if ev.Err != nil {
				modelErr = ev.Err
			}
		}
	}
	sum := wait()
	mainRunEnded.Store(true)

	stop := flag.get()
	if sum != nil {
		switch sum.EndReason {
		case ac.EndReasonMaxTurns:
			if stop == StopNone {
				stop = StopTurns
			}
		case ac.EndReasonAborted:
			if stop == StopNone {
				if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
					stop = StopWallClock
				} else {
					stop = StopCancelled
				}
			}
		case ac.EndReasonError:
			stop = StopModelErr
		}
	} else if ctx.Err() != nil {
		stop = StopCancelled
	} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		stop = StopWallClock
	}

	// Only a genuine model error is reported through Err. StopTurns and
	// StopDiffLines are structural cutoffs, not model failures, yet their
	// EventError carries a *ac.MaxTurnsError or the ctx cancellation used to
	// abort the agent (see budgetTool.abort); neither belongs in the report
	// as "the model failed".
	var reportErr error
	if stop == StopModelErr {
		reportErr = modelErr
		if reportErr == nil {
			reportErr = errors.New("model run ended with an error")
		}
	}

	// A caller cancellation is the only hard ceiling that skips the summary
	// turn outright: the caller wants out, full stop, so there is no point
	// spending the summary turn's own (uncancellable) grace period on it.
	// Every other stop, including wall clock, still gets one clean shot at a
	// summary: the summary turn runs on its own context.WithoutCancel(ctx)
	// with a fresh deadline, so an already-expired wall clock does not
	// constrain it.
	skipSummaryTurn := stop == StopModelErr || (stop == StopCancelled && ctx.Err() != nil)
	summaryTurns := 0
	if !strings.Contains(last, "What I found:") && !skipSummaryTurn {
		agent.SetTools()
		agent.ClearAllQueues()
		sumCtx, sumCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		sevents, swait := jess.Stream(sumCtx, agent, summaryPrompt)
		for ev := range sevents {
			if t := assistantText(ev); t != "" {
				last = t
			}
		}
		if s := swait(); s != nil {
			summaryTurns = s.TurnCount
		}
		sumCancel()
	}
	if !strings.Contains(last, "What I found:") {
		if last == "" {
			last = "The agent produced no summary."
		} else {
			last = "The agent produced no summary in the required shape. Its last message:\n\n" + last
		}
	}

	usage := agent.TotalUsage()
	diffCtx, diffCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	lines, err := diffLines(diffCtx)
	diffCancel()
	if err != nil {
		lines = flag.diffLines()
	}
	turnCount := turns + summaryTurns
	if sum != nil && sum.TurnCount > turns {
		turnCount = sum.TurnCount + summaryTurns
	}
	return RunReport{
		RunID:   capture.RunID(),
		Usage:   resolution.Usage{Turns: turnCount, InputTokens: usage.Input + usage.CacheRead, OutputTokens: usage.Output, WallClock: clock.Now().Sub(started), DiffLines: lines},
		Summary: last,
		Stop:    stop,
		Err:     reportErr,
	}
}

var _ ledger.DurableSink = (*captureRun)(nil)

// assistantText returns the text of ev when it is a real assistant message,
// and "" otherwise. Synthetic abort markers (jess/agentcore's own "[Request
// interrupted...]" stand-in, emitted with StopReasonAborted) are excluded:
// they are bookkeeping, not something the model said, and must not be
// mistaken for the model's final answer.
func assistantText(ev ac.Event) string {
	if ev.Type != ac.EventMessageEnd || ev.Message == nil || ev.Message.GetRole() != ac.RoleAssistant {
		return ""
	}
	if msg, ok := ev.Message.(ac.Message); ok && msg.StopReason == ac.StopReasonAborted {
		return ""
	}
	return ev.Message.TextContent()
}
