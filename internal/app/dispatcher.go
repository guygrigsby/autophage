package app

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/guygrigsby/autophage/internal/github"
	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
)

// defaultFloor is how long the dispatcher will go without a sweep. Every
// service logs "will retry" when GitHub refuses it, and a store
// notification is the only other thing that wakes one: a failure that
// writes nothing to the store would otherwise wait for an unrelated change
// to arrive, which on a quiet repository is for ever.
const defaultFloor = 60 * time.Second

// ShutdownWait bounds how long Run will hold the daemon open for runners
// that have not returned. Five minutes, because a runner that is going down
// still has real work to finish: the re-mint, then CommitAndPush, which
// removes the agent container and pushes the branch. Cut short there, the
// attempt's commits stay on host disk and the re-queued attempt starts from
// nothing. cmd/autophaged waits the same again on the scheduler before it
// closes the pool, and deploy/autophaged.service.template gives systemd
// TimeoutStopSec=600 to cover both.
const ShutdownWait = 5 * time.Minute

// Canceller cancels a running attempt; the runner implements it.
type Canceller interface {
	Cancel(attemptID string, reason resolution.AbortReason) bool
}

// Dispatcher runs recovery once, sweeps every service, then sweeps again on
// every store notification and at least once per Floor. Sweeps never
// overlap; wake-ups that arrive during a sweep coalesce into one more.
type Dispatcher struct {
	Store      *store.Store
	Translator *github.Translator
	Triage     *Triage
	Scheduler  *Scheduler
	Commenter  *Commenter
	Enrollment *Enrollment
	Recovery   *Recovery
	// Canceller cancels the attempt of a case the issue-closed webhook
	// closed out from under it. Nil disables the step, which the tests that
	// do not care about it rely on.
	Canceller Canceller
	// Floor is the cadence that makes every "will retry" true. Zero means
	// defaultFloor.
	Floor time.Duration

	mu   sync.Mutex
	wake chan struct{}
}

// Run subscribes to store notifications before doing anything else, so a
// change that lands while Recovery or the boot sweep is still running is
// still caught: either the boot scan sees it (if it lands before the scan
// reads the table) or the subscription queues a wake for it (if it lands
// after). Only once the subscription is confirmed live does Run run Recovery
// once, then the boot sweep, then loop on wake-ups.
func (d *Dispatcher) Run(ctx context.Context) error {
	listenCtx, stopListen := context.WithCancel(ctx)
	defer stopListen()

	d.wake = make(chan struct{}, 1)
	ready := make(chan struct{}, 1)
	go func() {
		_ = d.Store.Listen(listenCtx, func() {
			select {
			case ready <- struct{}{}:
			default:
			}
		}, func(string) {
			select {
			case d.wake <- struct{}{}:
			default:
			}
		})
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		return nil
	}

	floor := d.Floor
	if floor <= 0 {
		floor = defaultFloor
	}
	ticker := time.NewTicker(floor)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				select {
				case d.wake <- struct{}{}:
				default:
				}
			}
		}
	}()

	if err := d.Recovery.Run(ctx); err != nil {
		return err
	}
	d.Sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			if !d.Scheduler.WaitTimeout(ShutdownWait) {
				log.Printf("shutdown: gave up after %s on running attempts: %s", ShutdownWait, strings.Join(d.Scheduler.Running(), ", "))
			}
			return nil
		case <-d.wake:
			d.Sweep(ctx)
		}
	}
}

// Sweep runs every service once, in dependency order. Exported for tests
// and for the operator run endpoint.
func (d *Dispatcher) Sweep(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	steps := []struct {
		name string
		run  func(context.Context) error
	}{
		{"translate", d.Translator.ProcessPending},
		{"cancel", d.cancelClosed},
		{"enroll", d.Enrollment.Run},
		{"triage", d.Triage.Run},
		{"comment", d.Commenter.Run},
		{"schedule", d.Scheduler.Run},
	}
	for _, s := range steps {
		if err := s.run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("sweep %s: %v", s.name, err)
		}
	}
}

// cancelClosed cancels every open attempt whose case has been closed out
// from under it. A closed case cannot ever run again, so an attempt left
// running would spend turns and tokens on an outcome nothing will read.
func (d *Dispatcher) cancelClosed(ctx context.Context) error {
	if d.Canceller == nil {
		return nil
	}
	ids, err := d.Store.OpenAttemptsOnClosedCases(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if !d.Canceller.Cancel(id, resolution.AbortIssueClosed) {
			log.Printf("cancel %s: not running here", id)
		}
	}
	return nil
}
