package upkeep

import (
	"errors"
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
)

var (
	t0   = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	sha1 = "1111111111111111111111111111111111111111"
)

func newTestBump(t *testing.T) *Bump {
	t.Helper()
	b, err := NewBump("guygrigsby/autophage", 7, "dependabot/go_modules/golang.org/x/net-0.38.0", "main", sha1, t0)
	if err != nil {
		t.Fatalf("NewBump: %v", err)
	}
	return b
}

func TestNewBumpStartsAwaitingChecks(t *testing.T) {
	b := newTestBump(t)
	if got := b.State(); got != AwaitingChecks {
		t.Errorf("state = %s, want %s", got, AwaitingChecks)
	}
	if got := b.HeadSha(); got != sha1 {
		t.Errorf("head sha = %s, want %s", got, sha1)
	}
	if got := b.Rounds(); got != 0 {
		t.Errorf("rounds = %d, want 0", got)
	}
}

func TestNewBumpRefusesBadInput(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		repo                     string
		number                   int
		branch, baseBranch, head string
	}{
		{"empty repository", "", 7, "dependabot/x", "main", sha1},
		{"zero number", "o/r", 0, "dependabot/x", "main", sha1},
		{"empty branch", "o/r", 7, "", "main", sha1},
		{"empty base branch", "o/r", 7, "dependabot/x", "", sha1},
		{"short sha", "o/r", 7, "dependabot/x", "main", "abc"},
		{"upper case sha", "o/r", 7, "dependabot/x", "main", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewBump(tc.repo, tc.number, tc.branch, tc.baseBranch, tc.head, t0); !errors.Is(err, resolution.ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
		})
	}
}
