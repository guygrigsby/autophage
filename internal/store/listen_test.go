package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/storetest"
)

func TestListenReceivesCaseNotifications(t *testing.T) {
	s := storetest.Open(t)
	seedRepo(t, s, "guy/repo")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	got := make(chan string, 8)
	ready := make(chan struct{})
	go func() { _ = s.Listen(ctx, func() { close(ready) }, func(p string) { got <- p }) }()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("listen never became ready")
	}
	newCase(t, s, 7, owner(t))
	select {
	case p := <-got:
		if p != "case:guy/repo#7:received" {
			t.Errorf("payload = %q", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notification")
	}
	if _, err := s.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error {
		return c.RecordTriage(resolution.Triage{Size: resolution.Large, Rationale: "r", Model: "m", TriagedAt: t0})
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-got:
		if p != "case:guy/repo#7:awaiting_approval" {
			t.Errorf("payload = %q", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no second notification")
	}
}
