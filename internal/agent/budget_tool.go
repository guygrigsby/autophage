package agent

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	ac "github.com/voocel/agentcore"

	"github.com/guygrigsby/jess"
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
	// metrics counts the call and times it. Nil reports to nothing, which
	// is what a budgetTool built by hand in a test does.
	metrics Metrics
}

// meter tolerates an unwired metrics.
func (b *budgetTool) meter() Metrics {
	if b.metrics == nil {
		return NopMetrics{}
	}
	return b.metrics
}

func (b *budgetTool) Execute(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	// Timed around the tool itself, not around the diff count that follows:
	// the count is autophage's own bookkeeping, and charging it to the tool
	// would put a podman exec inside every write tool's latency.
	start := time.Now()
	out, err := b.Tool.Execute(ctx, args)
	b.meter().ToolCall(b.Name(), time.Since(start), err)
	// A read-only call cannot have moved the diff, and the count is not
	// free: it is a podman exec into the agent's container running git over
	// the whole tree. Reading a file is the commonest thing an agent does,
	// so counting after one spends most of the budget check on an answer
	// that cannot have changed.
	if b.ReadOnly(args) {
		return out, err
	}
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

// Safe reports whether the wrapped tool may bypass the gate's durable-record
// requirement. Embedding only ac.Tool drops this and the two interfaces
// below, so budgetTool forwards each explicitly: the inner tool's answer
// when it declares one, the conservative default (not safe, not read-only,
// not concurrency-safe) otherwise.
func (b *budgetTool) Safe() bool {
	if s, ok := b.Tool.(jess.SafeTool); ok {
		return s.Safe()
	}
	return false
}

// ReadOnly implements ac.ReadOnlyer by forwarding to the wrapped tool.
func (b *budgetTool) ReadOnly(args json.RawMessage) bool {
	if r, ok := b.Tool.(ac.ReadOnlyer); ok {
		return r.ReadOnly(args)
	}
	return false
}

// ConcurrencySafe implements ac.ConcurrencySafer by forwarding to the wrapped
// tool.
func (b *budgetTool) ConcurrencySafe(args json.RawMessage) bool {
	if c, ok := b.Tool.(ac.ConcurrencySafer); ok {
		return c.ConcurrencySafe(args)
	}
	return false
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
