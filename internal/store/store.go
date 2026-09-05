package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// ErrNotFound is returned when a row the caller named does not exist.
var ErrNotFound = errors.New("not found")

// Store is a connected, migrated database.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to dsn, applies migrations and returns the store.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// Pool exposes the pool for readers that need it.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// SQLDB opens a database/sql handle over the same pool, for libraries that
// take one rather than a pgx pool (jess/ledger). It keeps pgx inside this
// package: the daemon asks for a *sql.DB and never names the driver. The
// caller closes what it gets back.
func (s *Store) SQLDB() *sql.DB { return stdlib.OpenDBFromPool(s.pool) }

func (s *Store) Close() { s.pool.Close() }

// tx runs fn in a transaction, committing on nil and rolling back otherwise.
// The deferred rollback also covers a panic inside fn: it runs unconditionally,
// and after a successful commit it is a harmless no-op that returns
// pgx.ErrTxClosed.
func (s *Store) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// notify issues pg_notify on the autophage_events channel inside tx, so the
// wake-up is delivered exactly when the change commits.
func notify(ctx context.Context, tx pgx.Tx, payload string) error {
	_, err := tx.Exec(ctx, "select pg_notify('autophage_events', $1)", payload)
	return err
}

// toInterval converts a Duration to the wire form Postgres INTERVAL columns
// take on insert: pgx does not encode time.Duration into INTERVAL directly.
func toInterval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

// fromInterval converts a scanned INTERVAL back to a Duration. Intervals
// stored here never carry days or months, but days is included for safety.
func fromInterval(iv pgtype.Interval) time.Duration {
	return time.Duration(iv.Microseconds)*time.Microsecond + time.Duration(iv.Days)*24*time.Hour
}
