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

func (d *Dispatcher) Run(ctx context.Context) error {
	if err := d.Recovery.Run(ctx); err != nil {
		return err
	}
	d.wake = make(chan struct{}, 1)
	d.Sweep(ctx)
	go func() {
		_ = d.Store.Listen(ctx, func(string) {
			select {
			case d.wake <- struct{}{}:
			default:
			}
		})
	}()
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
