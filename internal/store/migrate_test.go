package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestMigrationMatchesContracts proves 0001_init.sql is the contracts
// document's DDL, block by block, in order. Edit the document, then the
// migration, never one without the other.
func TestMigrationMatchesContracts(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "specs", "2026-09-04-autophage-contracts.md"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile("(?s)```sql\n(.*?)```")
	var want strings.Builder
	for _, m := range re.FindAllSubmatch(doc, -1) {
		want.Write(m[1])
		want.WriteString("\n")
	}
	mig, err := migrations.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimPrefix(string(mig), "-- +goose Up\n")
	if strings.TrimSpace(body) != strings.TrimSpace(want.String()) {
		t.Fatalf("migration drifted from the contracts DDL; regenerate with:\n  scripts/ddl-from-contracts.sh > internal/store/migrations/0001_init.sql")
	}
}

func TestOpenAppliesMigrations(t *testing.T) {
	s := OpenTest(t)
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
