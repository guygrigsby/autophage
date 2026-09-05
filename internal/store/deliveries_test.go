package store_test

import (
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/store"
	"github.com/guygrigsby/autophage/internal/storetest"
)

func TestStoreDeliveryIdempotent(t *testing.T) {
	s := storetest.Open(t)
	ctx := t.Context()
	d := store.Delivery{ID: "d-1", Event: "issues", Action: "opened", SenderLogin: "guy", Payload: []byte(`{"a":1}`), ReceivedAt: t0}
	dup, err := s.StoreDelivery(ctx, d)
	if err != nil || dup {
		t.Fatalf("first: dup=%v err=%v", dup, err)
	}
	dup, err = s.StoreDelivery(ctx, d)
	if err != nil || !dup {
		t.Fatalf("second: dup=%v err=%v", dup, err)
	}
	un, err := s.UnprocessedDeliveries(ctx)
	if err != nil || len(un) != 1 || string(un[0].Payload) != `{"a":1}` {
		t.Fatalf("unprocessed = %+v %v", un, err)
	}
	if err := s.RecordProcessing(ctx, "d-1", "translated", "IssueOpened guy/repo#1"); err != nil {
		t.Fatal(err)
	}
	un, _ = s.UnprocessedDeliveries(ctx)
	if len(un) != 0 {
		t.Errorf("still unprocessed: %+v", un)
	}
	if err := s.RecordProcessing(ctx, "d-1", "ignored", "again"); err == nil {
		t.Error("second processing accepted")
	}
	_ = time.Now
}

func TestLastDeliveryAt(t *testing.T) {
	s := storetest.Open(t)
	ctx := t.Context()
	if _, ok, err := s.LastDeliveryAt(ctx); err != nil || ok {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}
	if _, err := s.StoreDelivery(ctx, store.Delivery{ID: "d-1", Event: "issues", Action: "opened", SenderLogin: "guy", Payload: []byte(`{}`), ReceivedAt: t0}); err != nil {
		t.Fatal(err)
	}
	later := t0.Add(time.Hour)
	if _, err := s.StoreDelivery(ctx, store.Delivery{ID: "d-2", Event: "issues", Action: "closed", SenderLogin: "guy", Payload: []byte(`{}`), ReceivedAt: later}); err != nil {
		t.Fatal(err)
	}
	at, ok, err := s.LastDeliveryAt(ctx)
	if err != nil || !ok || !at.Equal(later) {
		t.Errorf("last delivery = %v %v %v, want %v", at, ok, err, later)
	}
}
