package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
)

// triageBackoff is the wait after one failed Classify, doubled per further
// failure up to triageBackoffCap. Without it a case whose model call keeps
// failing is retried on every sweep, which on the production floor is a paid
// model call a minute, for ever, on a case nobody is waiting on.
//
// triageGiveUp is how many failures park the case instead. Five failures
// means four waits of 1m, 2m, 4m and 8m, so the park lands about 15 minutes
// after the first failure: long enough to ride out a rate limit or a short
// provider outage, short enough that a case the model genuinely cannot size
// reaches a human while the operator is still looking. Parking is a Large
// triage through the ordinary path, so the case waits for the approval label
// with a rationale saying why, rather than sitting Received where nothing
// surfaces it.
const (
	triageBackoff    = time.Minute
	triageBackoffCap = time.Hour
	triageGiveUp     = 5
)

// unknownTriageModel is what the parked triage records when nothing wired a
// model id. The domain requires one, and refusing the park over a missing
// label would put the case back in the retry loop this exists to end.
const unknownTriageModel = "unknown"

// Triage sizes every Received case with one model call.
type Triage struct {
	Store   *store.Store
	Triager resolution.Triager
	GitHub  resolution.GitHub
	Clock   resolution.Clock
	// Model is the triage tier's model id, recorded on the triage that parks
	// a case the model would not size.
	Model string
	// Metrics reports each sizing call. Nil is NopTriageMetrics.
	Metrics TriageMetrics

	// mu guards failures, which the dispatcher's goroutine reads and writes
	// on every sweep. The record is in memory only: a restart forgets it and
	// the case is tried again at once, which is the right answer for the
	// failure a restart most often follows.
	mu       sync.Mutex
	failures map[store.CaseKey]triageFailure
}

// triageFailure is one case's run of failed Classify calls: how many, and
// when the next call may go out.
type triageFailure struct {
	count int
	next  time.Time
}

// Run sizes each Received case without a triage, skipping the ones inside a
// failure backoff. A failure on one case is logged and the case stays
// Received for a later sweep.
func (t *Triage) Run(ctx context.Context) error {
	keys, err := t.Store.ReceivedWithoutTriage(ctx)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if !t.eligible(k) {
			continue
		}
		if err := t.one(ctx, k); err != nil {
			log.Printf("triage %s#%d: %v", k.Repository, k.Number, err)
		}
	}
	return nil
}

func (t *Triage) one(ctx context.Context, k store.CaseKey) error {
	detail, err := t.GitHub.GetIssue(ctx, k.Repository, k.Number)
	if err != nil {
		return err
	}
	start := time.Now()
	tr, cerr := t.Triager.Classify(ctx, detail.Title, detail.Body)
	elapsed := time.Since(start)
	if cerr == nil {
		t.metrics().Triage(string(tr.Size), resultOK, elapsed)
	} else {
		count, next := t.fail(k)
		if count < triageGiveUp {
			t.metrics().Triage(sizeNone, resultError, elapsed)
			return fmt.Errorf("classify (failure %d, next attempt after %s): %w", count, next.Format(time.RFC3339), cerr)
		}
		// The model has refused this case often enough that waiting longer
		// is not an answer. Park it for a human, saying why. The size the
		// park records is the policy's, not the model's, so the metric
		// reports no size at all: what the operator needs from this row is
		// that a case reached a human without ever being sized.
		t.metrics().Triage(sizeNone, resultParked, elapsed)
		tr = resolution.Triage{
			Size:      resolution.Large,
			Rationale: fmt.Sprintf("triage failed %d times, last error: %s", count, oneLine(cerr.Error())),
			Model:     t.model(),
		}
	}
	tr.TriagedAt = t.Clock.Now()
	if _, err := t.Store.UpdateCase(ctx, k.Repository, k.Number, func(c *resolution.Case) error { return c.RecordTriage(tr) }); err != nil {
		return err
	}
	t.clear(k)
	return nil
}

// metrics tolerates an unwired Metrics.
func (t *Triage) metrics() TriageMetrics {
	if t.Metrics == nil {
		return NopTriageMetrics{}
	}
	return t.Metrics
}

func (t *Triage) model() string {
	if t.Model == "" {
		return unknownTriageModel
	}
	return t.Model
}

// eligible reports whether this case's backoff has passed.
func (t *Triage) eligible(k store.CaseKey) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.failures[k]
	return !ok || !t.Clock.Now().Before(f.next)
}

// fail records one more failed Classify for this case and returns the count
// and the time the next call may go out.
func (t *Triage) fail(k store.CaseKey) (int, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failures == nil {
		t.failures = map[store.CaseKey]triageFailure{}
	}
	f := t.failures[k]
	f.count++
	wait := triageBackoff
	for range f.count - 1 {
		if wait >= triageBackoffCap {
			break
		}
		wait *= 2
	}
	if wait > triageBackoffCap {
		wait = triageBackoffCap
	}
	f.next = t.Clock.Now().Add(wait)
	t.failures[k] = f
	return f.count, f.next
}

// clear forgets a case's failures once it has been triaged, whether the
// model sized it or the give-up parked it.
func (t *Triage) clear(k store.CaseKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failures, k)
}

// oneLine folds an error message onto one line and bounds it: the rationale
// it goes into is posted as an issue comment.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "..."
	}
	return s
}
