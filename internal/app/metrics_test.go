package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/github"
	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/storetest"
)

// recordMetrics records every hook this package fires, with the labels it
// fired them with. One fake for all three interfaces: the dispatcher, the
// triage and the commenter are wired together in the same tests, and a
// separate recorder per interface would be three copies of the same mutex.
type recordMetrics struct {
	mu      sync.Mutex
	sweeps  int
	steps   []string
	triages []string
	posts   []string
	slow    int
}

// Step records the step name, suffixed "!" when the step failed. A step is
// timed, so a duration that never advanced is a hook wired to the wrong
// place.
func (m *recordMetrics) Step(step string, d time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		step += "!"
	}
	if d > 0 {
		m.slow++
	}
	m.steps = append(m.steps, step)
}

func (m *recordMetrics) Sweep() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweeps++
}

func (m *recordMetrics) Triage(size, result string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d > 0 {
		m.slow++
	}
	m.triages = append(m.triages, size+"/"+result)
}

func (m *recordMetrics) Post(result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.posts = append(m.posts, result)
}

// timed is how many hooks carried a duration above zero, which is what
// proves a step or a model call was measured rather than reported flat.
func (m *recordMetrics) timed() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.slow
}

func (m *recordMetrics) recorded() (int, []string, []string, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sweeps, append([]string(nil), m.steps...), append([]string(nil), m.triages...), append([]string(nil), m.posts...)
}

// stubCanceller is the dispatcher's cancel step wired to nothing. A nil
// Canceller skips the step outright, which would leave one of the six steps
// unmeasured.
type stubCanceller struct{}

func (stubCanceller) Cancel(string, resolution.AbortReason) bool { return false }

func newMeteredDispatcher(t *testing.T, m *recordMetrics) *Dispatcher {
	t.Helper()
	st := storetest.Open(t)
	gh := &fakeGitHub{issues: map[string]resolution.IssueDetail{}}
	return &Dispatcher{
		Store:      st,
		Translator: &github.Translator{Store: st, Clock: fixedClock{t0}, ApprovedLabel: "approved", BotLogin: "autophage[bot]"},
		Triage:     &Triage{Store: st, Triager: fakeTriager{size: resolution.Small}, GitHub: gh, Clock: fixedClock{t0}},
		Scheduler:  &Scheduler{Store: st, Runner: &fakeRunner{release: make(chan struct{})}, Concurrency: 1, Clock: fixedClock{t0}, Budgets: policy(t), GitHub: gh},
		Commenter:  &Commenter{Store: st, GitHub: gh, Label: "approved"},
		Enrollment: &Enrollment{Store: st, GitHub: gh, Label: "approved"},
		Recovery:   &Recovery{Store: st, Clock: fixedClock{t0}},
		Canceller:  stubCanceller{},
		Metrics:    m,
	}
}

// The catalogue's sweep step vocabulary, in the order the dispatcher runs
// them. The dashboard groups by these names, so a renamed step is a broken
// panel.
var sweepSteps = []string{"translate", "cancel", "enroll", "triage", "comment", "schedule"}

func TestSweepRecordsEveryStep(t *testing.T) {
	m := &recordMetrics{}
	d := newMeteredDispatcher(t, m)
	d.Sweep(t.Context())

	sweeps, steps, _, _ := m.recorded()
	if sweeps != 1 {
		t.Errorf("sweeps = %d, want 1", sweeps)
	}
	if strings.Join(steps, ",") != strings.Join(sweepSteps, ",") {
		t.Errorf("steps = %v, want %v", steps, sweepSteps)
	}
	if m.timed() == 0 {
		t.Error("no step was timed")
	}
}

// A step that fails is counted as a failure, whatever failed it. The sweep
// on the way down is the commonest one on the production floor: every store
// call refuses at once, and an operator reading the dashboard should see
// that rather than a silent gap.
func TestSweepRecordsAStepError(t *testing.T) {
	m := &recordMetrics{}
	d := newMeteredDispatcher(t, m)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	d.Sweep(ctx)

	_, steps, _, _ := m.recorded()
	if len(steps) != len(sweepSteps) {
		t.Fatalf("steps = %v", steps)
	}
	for i, step := range steps {
		if step != sweepSteps[i]+"!" {
			t.Errorf("step %d = %q, want %q", i, step, sweepSteps[i]+"!")
		}
	}
}

func TestTriageRecordsSizeAndResult(t *testing.T) {
	st := storetest.Open(t)
	seed(t, st)
	gh := &fakeGitHub{issues: map[string]resolution.IssueDetail{keyOf("guy/repo", 7): issue(true)}}
	m := &recordMetrics{}
	tr := &Triage{Store: st, Triager: fakeTriager{size: resolution.Large}, GitHub: gh, Clock: fixedClock{t0}, Metrics: m}
	if err := tr.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, triages, _ := m.recorded(); len(triages) != 1 || triages[0] != "large/ok" {
		t.Errorf("triages = %v, want [large/ok]", triages)
	}
	if m.timed() == 0 {
		t.Error("the model call was not timed")
	}
}

// A failed model call is an error until the give-up parks the case, and the
// park is its own result: it is the one triage no model sized.
func TestTriageRecordsErrorsThenThePark(t *testing.T) {
	st := storetest.Open(t)
	seed(t, st)
	gh := &fakeGitHub{issues: map[string]resolution.IssueDetail{keyOf("guy/repo", 7): issue(true)}}
	m := &recordMetrics{}
	clock := &movingClock{t: t0}
	tr := &Triage{Store: st, Triager: fakeTriager{err: errors.New("openrouter: 402")}, GitHub: gh, Clock: clock, Model: "fake/triage", Metrics: m}
	for range triageGiveUp {
		if err := tr.Run(t.Context()); err != nil {
			t.Fatal(err)
		}
		clock.advance(triageBackoffCap)
	}
	_, _, triages, _ := m.recorded()
	want := []string{"none/error", "none/error", "none/error", "none/error", "none/parked"}
	if strings.Join(triages, ",") != strings.Join(want, ",") {
		t.Errorf("triages = %v, want %v", triages, want)
	}
}

func TestCommenterRecordsPosts(t *testing.T) {
	st := storetest.Open(t)
	seed(t, st)
	ctx := t.Context()
	if _, err := st.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error {
		return c.RecordTriage(resolution.Triage{Size: resolution.Large, Rationale: "touches the auth layer", Model: "m", TriagedAt: t0})
	}); err != nil {
		t.Fatal(err)
	}
	m := &recordMetrics{}
	gh := &fakeGitHub{failPost: true}
	cm := &Commenter{Store: st, GitHub: gh, Label: "approved", Metrics: m}
	if err := cm.Run(ctx); err != nil {
		t.Fatal(err)
	}
	gh.setFailPost(false)
	if err := cm.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, _, posts := m.recorded(); strings.Join(posts, ",") != "error,ok" {
		t.Errorf("posts = %v, want [error ok]", posts)
	}
}
