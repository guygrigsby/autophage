package app

import (
	"context"
	"log"
	"sync"

	"github.com/guygrigsby/autophage/internal/github"
	"github.com/guygrigsby/autophage/internal/store"
)

// Dispatcher runs recovery once, sweeps every service, then sweeps again on
// every store notification. Sweeps never overlap; wake-ups that arrive
// during a sweep coalesce into one more.
type Dispatcher struct {
	Store      *store.Store
	Translator *github.Translator
	Triage     *Triage
	Scheduler  *Scheduler
	Commenter  *Commenter
	Labels     *LabelSetup
	Recovery   *Recovery

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

	if err := d.Recovery.Run(ctx); err != nil {
		return err
	}
	d.Sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			d.Scheduler.Wait()
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
		{"labels", d.Labels.Run},
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
