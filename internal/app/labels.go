package app

import (
	"context"
	"log"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
)

// LabelSetup ensures the approved label exists on every enrolled repository.
type LabelSetup struct {
	Store  *store.Store
	GitHub resolution.GitHub
	Label  string
}

func (l *LabelSetup) Run(ctx context.Context) error {
	repos, err := l.Store.RepositoriesNeedingLabel(ctx)
	if err != nil {
		return err
	}
	for _, r := range repos {
		if err := l.GitHub.EnsureLabel(ctx, r.FullName, l.Label); err != nil {
			log.Printf("label %s on %s: %v (will retry)", l.Label, r.FullName, err)
			continue
		}
		if err := l.Store.RecordLabelSetup(ctx, r.FullName, l.Label); err != nil {
			log.Printf("record label setup %s: %v (will retry)", r.FullName, err)
			continue
		}
	}
	return nil
}
