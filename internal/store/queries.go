package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// CaseFilter narrows ListCases. After is the cursor a previous page returned.
type CaseFilter struct {
	State      string
	Repository string
	Limit      int
	After      string
}

// CaseRow is one line of the operator listing.
type CaseRow struct {
	ID                string
	Repository        string
	Number            int
	State             string
	RequesterLogin    string
	RequesterTrust    string
	ReceivedAt        time.Time
	LatestOutcomeKind string
}

// ListCases pages cases newest first by (received_at, id). The cursor is
// "<received_at RFC3339Nano>|<id>" of the last row.
func (s *Store) ListCases(ctx context.Context, f CaseFilter) ([]CaseRow, string, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 500 {
		f.Limit = 500
	}
	var where []string
	var args []any
	add := func(clause string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.State != "" {
		add("c.state = $%d", f.State)
	}
	if f.Repository != "" {
		add("c.repository = $%d", f.Repository)
	}
	if f.After != "" {
		ts, id, ok := strings.Cut(f.After, "|")
		at, err := time.Parse(time.RFC3339Nano, ts)
		if !ok || err != nil {
			return nil, "", fmt.Errorf("bad cursor")
		}
		args = append(args, at, id)
		where = append(where, fmt.Sprintf("(c.received_at, c.id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, f.Limit+1)
	q := `select c.id, c.repository, c.number, c.state, c.requester_login, c.requester_trust, c.received_at,
			coalesce((select o.kind from attempts a join attempt_outcomes o on o.attempt_id = a.id where a.case_id = c.id order by a.ordinal desc limit 1), 'none')
		from cases c`
	if len(where) > 0 {
		q += " where " + strings.Join(where, " and ")
	}
	q += fmt.Sprintf(" order by c.received_at desc, c.id desc limit $%d", len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []CaseRow
	for rows.Next() {
		var r CaseRow
		if err := rows.Scan(&r.ID, &r.Repository, &r.Number, &r.State, &r.RequesterLogin, &r.RequesterTrust, &r.ReceivedAt, &r.LatestOutcomeKind); err != nil {
			return nil, "", err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > f.Limit {
		out = out[:f.Limit]
		last := out[len(out)-1]
		next = last.ReceivedAt.UTC().Format(time.RFC3339Nano) + "|" + last.ID
	}
	return out, next, nil
}

// QueuedCases lists Queued cases in enrolled repositories, oldest queueing
// first, for the Scheduler.
func (s *Store) QueuedCases(ctx context.Context) ([]CaseKey, error) {
	return s.keys(ctx, `select c.repository, c.number from cases c
		left join repository_removals x on x.repository = c.repository
		join lateral (select occurred_at from case_transitions t where t.case_id = c.id and t.to_state = 'queued' order by t.id desc limit 1) q on true
		where c.state = 'queued' and x.repository is null
		order by q.occurred_at, c.id`)
}

// ReceivedWithoutTriage lists trusted cases the triage service has not sized.
func (s *Store) ReceivedWithoutTriage(ctx context.Context) ([]CaseKey, error) {
	return s.keys(ctx, `select c.repository, c.number from cases c
		left join case_triages t on t.case_id = c.id
		where c.state = 'received' and t.case_id is null order by c.received_at, c.id`)
}

func (s *Store) keys(ctx context.Context, q string) ([]CaseKey, error) {
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CaseKey
	for rows.Next() {
		var k CaseKey
		if err := rows.Scan(&k.Repository, &k.Number); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// OpenAttempt is a running attempt as the status endpoint and the restart
// recovery see it.
type OpenAttempt struct {
	AttemptID  string
	Repository string
	Number     int
	Ordinal    int
	StartedAt  time.Time
}

func (s *Store) OpenAttempts(ctx context.Context) ([]OpenAttempt, error) {
	rows, err := s.pool.Query(ctx, `select a.id, c.repository, c.number, a.ordinal, a.started_at
		from attempts a join cases c on c.id = a.case_id
		left join attempt_outcomes o on o.attempt_id = a.id
		where o.attempt_id is null order by a.started_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpenAttempt
	for rows.Next() {
		var a OpenAttempt
		if err := rows.Scan(&a.AttemptID, &a.Repository, &a.Number, &a.Ordinal, &a.StartedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) CountByState(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `select state, count(*) from cases group by state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}
