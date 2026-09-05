package store

import (
	"context"
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
)

func TestListenReceivesCaseNotifications(t *testing.T) {
	s := OpenTest(t)
	seedRepo(t, s, "guy/repo")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	got := make(chan string, 8)
	go func() { _ = s.Listen(ctx, func(p string) { got <- p }) }()
	time.Sleep(200 * time.Millisecond)
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
