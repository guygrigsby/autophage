package storetest

import (
	"errors"
	"fmt"
	"testing"
)

// fakeSkipper records which method skipOrFatal called, without a real
// *testing.T's Goexit-on-Fatal/Skip behavior, so the decision logic is
// testable in isolation.
type fakeSkipper struct {
	skipped, fataled  bool
	skipMsg, fatalMsg string
}

func (f *fakeSkipper) Helper() {}

func (f *fakeSkipper) Skipf(format string, args ...any) {
	f.skipped = true
	f.skipMsg = fmt.Sprintf(format, args...)
}

func (f *fakeSkipper) Fatalf(format string, args ...any) {
	f.fataled = true
	f.fatalMsg = fmt.Sprintf(format, args...)
}

// TestSkipOrFatal proves the regression fixed here: a container-managed DSN
// (AUTOPHAGE_TEST_DSN unset) skips on ANY failure, regardless of the error
// text testcontainers happens to return (the old code matched substrings of
// the error message and missed testcontainers' bare "failed to create Docker
// provider" wrapping when Docker is absent entirely, so it fataled instead
// of skipping). An operator-supplied DSN still fatals: that failure is real.
//
// skipOrFatal is scoped to the container-start failure only. Once a
// database is reachable (container up, or an operator-supplied DSN),
// OpenTest calls Open directly and fatals unconditionally on any error from
// it (connect, ping or migrate) via a bare t.Fatalf, on both the container
// path and the explicit-DSN path: that failure is never environmental, so
// it never goes through skipOrFatal and is not exercised by this fake-based
// unit test (asserting it would mean actually failing a *testing.T, which
// is exactly what a fake avoids; the call site itself is a single
// unconditional t.Fatalf with no branch to hide a regression in).
func TestSkipOrFatal(t *testing.T) {
	t.Run("container-managed DSN skips on any error, never fatals", func(t *testing.T) {
		for _, err := range []error{
			errors.New("failed to create Docker provider"), // the exact case that used to slip through
			errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock"),
			errors.New("connection refused"),
			errors.New("anything at all"),
		} {
			f := &fakeSkipper{}
			skipOrFatal(f, err, false)
			if !f.skipped || f.fataled {
				t.Errorf("skipOrFatal(%q, explicitDSN=false): skipped=%v fataled=%v, want skipped only", err, f.skipped, f.fataled)
			}
		}
	})

	t.Run("operator-supplied DSN fatals, never skips", func(t *testing.T) {
		f := &fakeSkipper{}
		skipOrFatal(f, errors.New("connection refused"), true)
		if !f.fataled || f.skipped {
			t.Errorf("skipOrFatal(explicitDSN=true): skipped=%v fataled=%v, want fataled only", f.skipped, f.fataled)
		}
	})
}
