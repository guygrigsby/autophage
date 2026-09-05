package store

import (
	"testing"
	"time"
)

func TestStoreDeliveryIdempotent(t *testing.T) {
	s := OpenTest(t)
	ctx := t.Context()
	d := Delivery{ID: "d-1", Event: "issues", Action: "opened", SenderLogin: "guy", Payload: []byte(`{"a":1}`), ReceivedAt: t0}
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
