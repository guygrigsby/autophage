package agent

import (
	"context"
	"encoding/json"
	"sync"

	ac "github.com/voocel/agentcore"
)

// budgetTool runs the inner tool, then measures the diff. Crossing the
// diff-line limit records the stop and aborts the agent; the run's summary
// turn still happens afterwards.
type budgetTool struct {
	ac.Tool
	limit     int
	diffLines func(ctx context.Context) (int, error)
	stop      *stopFlag
	abort     func()
	logf      func(string, ...any)
}

func (b *budgetTool) Execute(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	out, err := b.Tool.Execute(ctx, args)
	n, derr := b.diffLines(ctx)
	if derr != nil {
		b.logf("diff lines: %v", derr)
		return out, err
	}
	b.stop.diff(n)
	if n >= b.limit && b.stop.set(StopDiffLines) {
		b.logf("diff lines %d reached the budget of %d; stopping", n, b.limit)
		b.abort()
	}
	return out, err
}

// stopFlag is the first stop reason, set once, plus the last diff count.
type stopFlag struct {
	mu    sync.Mutex
	stop  Stop
	lines int
}

func (s *stopFlag) set(v Stop) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != StopNone {
		return false
	}
	s.stop = v
	return true
}

func (s *stopFlag) get() Stop {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stop
}

func (s *stopFlag) diff(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = n
}

func (s *stopFlag) diffLines() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lines
}
