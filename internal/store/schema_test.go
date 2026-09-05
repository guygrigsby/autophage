package store_test

import (
	"testing"

	"github.com/guygrigsby/autophage/internal/storetest"
)

func TestOpenAppliesMigrations(t *testing.T) {
	s := storetest.Open(t)
	var n int
	if err := s.Pool().QueryRow(t.Context(), "select count(*) from pg_tables where schemaname = 'public' and tablename not like 'goose%'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 34 {
		t.Errorf("tables = %d, want 34", n)
	}
	var states int
	if err := s.Pool().QueryRow(t.Context(), "select count(*) from case_states").Scan(&states); err != nil || states != 8 {
		t.Errorf("case_states seeded = %d %v", states, err)
	}
}
