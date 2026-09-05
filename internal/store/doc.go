// Package store is autophage's Postgres persistence: one transaction per
// aggregate change, the fact tables as the event log, NOTIFY as the wake-up.
// It is the only package that imports pgx and goose. Every table is the
// contracts document's, applied by the embedded migration.
package store
