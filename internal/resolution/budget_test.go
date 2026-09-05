package resolution

import (
	"testing"
	"time"
)

func TestNewBudgetRefusesNonPositive(t *testing.T) {
	for _, c := range []struct {
		turns, diff int
		wall        time.Duration
	}{{0, 1, time.Minute}, {1, 0, time.Minute}, {1, 1, 0}, {-1, 1, time.Minute}} {
		if _, err := NewBudget(c.turns, c.wall, c.diff); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

func TestBudgetWarnAtIsEightyPercent(t *testing.T) {
	b, _ := NewBudget(50, 100*time.Minute, 400)
	turns, wall := b.WarnAt()
	if turns != 40 || wall != 80*time.Minute {
		t.Errorf("warn at = %d, %s", turns, wall)
	}
}

func TestBudgetExceededReportsFirstLimit(t *testing.T) {
	b, _ := NewBudget(50, 100*time.Minute, 400)
	if l, ok := b.Exceeded(Usage{Turns: 49, WallClock: 99 * time.Minute, DiffLines: 399}); ok {
		t.Errorf("within budget reported %s", l)
	}
	if l, ok := b.Exceeded(Usage{Turns: 50}); !ok || l != LimitTurns {
		t.Errorf("turns: %s %v", l, ok)
	}
	if l, ok := b.Exceeded(Usage{WallClock: 100 * time.Minute}); !ok || l != LimitWallClock {
		t.Errorf("wall: %s %v", l, ok)
	}
	if l, ok := b.Exceeded(Usage{DiffLines: 400}); !ok || l != LimitDiffLines {
		t.Errorf("diff: %s %v", l, ok)
	}
}

func TestParseCaseStateRoundTrips(t *testing.T) {
	for _, s := range AllCaseStates() {
		got, err := ParseCaseState(string(s))
		if err != nil || got != s {
			t.Errorf("%s: %v", s, err)
		}
	}
	if _, err := ParseCaseState("limbo"); err == nil {
		t.Error("unknown state accepted")
	}
}
