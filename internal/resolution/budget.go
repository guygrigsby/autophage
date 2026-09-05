package resolution

import "time"

// Budget is the limits on one attempt. All three are positive; NewBudget is
// the only constructor.
type Budget struct {
	maxTurns     int
	maxWallClock time.Duration
	maxDiffLines int
}

func NewBudget(maxTurns int, maxWallClock time.Duration, maxDiffLines int) (Budget, error) {
	if maxTurns <= 0 || maxWallClock <= 0 || maxDiffLines <= 0 {
		return Budget{}, Invalid("budget limits must be positive: turns %d, wall %s, diff %d", maxTurns, maxWallClock, maxDiffLines)
	}
	return Budget{maxTurns: maxTurns, maxWallClock: maxWallClock, maxDiffLines: maxDiffLines}, nil
}

func (b Budget) MaxTurns() int               { return b.maxTurns }
func (b Budget) MaxWallClock() time.Duration { return b.maxWallClock }
func (b Budget) MaxDiffLines() int           { return b.maxDiffLines }

// WarnAt is the point, 80% of turns and of wall clock, at which the agent is
// told to wrap up.
func (b Budget) WarnAt() (turns int, wall time.Duration) {
	return b.maxTurns * 8 / 10, b.maxWallClock * 8 / 10
}

// Exceeded reports the first limit the usage has reached, checking turns,
// then wall clock, then diff lines.
func (b Budget) Exceeded(u Usage) (Limit, bool) {
	switch {
	case u.Turns >= b.maxTurns:
		return LimitTurns, true
	case u.WallClock >= b.maxWallClock:
		return LimitWallClock, true
	case u.DiffLines >= b.maxDiffLines:
		return LimitDiffLines, true
	}
	return "", false
}

// Usage is what an attempt consumed. Zeros mean it never reached the agent.
type Usage struct {
	Turns        int
	InputTokens  int
	OutputTokens int
	WallClock    time.Duration
	DiffLines    int
}
