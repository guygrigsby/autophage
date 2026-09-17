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
	// 34 from 0001, 20 from 0002 (Upkeep: 7 vocabularies and 13 tables).
	if n != 54 {
		t.Errorf("tables = %d, want 54", n)
	}
	var states int
	if err := s.Pool().QueryRow(t.Context(), "select count(*) from case_states").Scan(&states); err != nil || states != 8 {
		t.Errorf("case_states seeded = %d %v", states, err)
	}
	var bumpStates, causes int
	if err := s.Pool().QueryRow(t.Context(), "select count(*) from bump_states").Scan(&bumpStates); err != nil || bumpStates != 6 {
		t.Errorf("bump_states seeded = %d %v", bumpStates, err)
	}
	if err := s.Pool().QueryRow(t.Context(), "select count(*) from bump_transition_causes").Scan(&causes); err != nil || causes != 17 {
		t.Errorf("bump_transition_causes seeded = %d %v", causes, err)
	}
}
