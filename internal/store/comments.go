package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Comment is an outbox row with the case it lands on.
type Comment struct {
	ID         string
	CaseID     string
	Repository string
	Number     int
	Body       string
	CreatedAt  time.Time
}

// EnqueueTriageComment queues the rationale comment for a case's triage.
// A triage that already has a comment is left alone.
func (s *Store) EnqueueTriageComment(ctx context.Context, caseID, body string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from case_triage_comments where case_id = $1)`, caseID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		var id string
		if err := tx.QueryRow(ctx, `insert into github_comments (case_id, body) values ($1, $2) returning id`, caseID, body).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into case_triage_comments (case_id, comment_id) values ($1, $2)`, caseID, id); err != nil {
			return err
		}
		return notify(ctx, tx, "comment:"+id)
	})
}

// EnqueueOutcomeComment queues the summary comment for an attempt's outcome.
func (s *Store) EnqueueOutcomeComment(ctx context.Context, attemptID, body string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from attempt_outcome_comments where attempt_id = $1)`, attemptID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		var id string
		if err := tx.QueryRow(ctx, `insert into github_comments (case_id, body) select case_id, $2 from attempts where id = $1 returning id`, attemptID, body).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into attempt_outcome_comments (attempt_id, comment_id) values ($1, $2)`, attemptID, id); err != nil {
			return err
		}
		return notify(ctx, tx, "comment:"+id)
	})
}

// UnpostedComments lists outbox rows with no post, oldest first, for
// enrolled repositories only. The App has no access to a repository it was
// removed from, so posting there fails every sweep for ever.
func (s *Store) UnpostedComments(ctx context.Context) ([]Comment, error) {
	rows, err := s.pool.Query(ctx, `select g.id, g.case_id, c.repository, c.number, g.body, g.created_at
		from github_comments g join cases c on c.id = g.case_id
		left join github_comment_posts p on p.comment_id = g.id
		left join repository_removals x on x.repository = c.repository
		where p.comment_id is null and x.repository is null order by g.created_at, g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		var m Comment
		if err := rows.Scan(&m.ID, &m.CaseID, &m.Repository, &m.Number, &m.Body, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RecordCommentPost marks a queued comment as landed on GitHub.
func (s *Store) RecordCommentPost(ctx context.Context, commentID string, githubCommentID int64) error {
	_, err := s.pool.Exec(ctx, `insert into github_comment_posts (comment_id, github_comment_id) values ($1, $2) on conflict (comment_id) do nothing`, commentID, githubCommentID)
	return err
}

// TriagesNeedingComment lists cases parked by a Large triage with no comment
// queued yet, in enrolled repositories only.
func (s *Store) TriagesNeedingComment(ctx context.Context) ([]CaseKey, error) {
	return s.keys(ctx, `select c.repository, c.number from cases c
		join case_triages t on t.case_id = c.id
		left join case_triage_comments m on m.case_id = c.id
		left join repository_removals x on x.repository = c.repository
		where t.size = 'large' and m.case_id is null and x.repository is null
		order by t.triaged_at`)
}

// OutcomesNeedingComment lists attempts whose outcome is reported as a
// comment (exhausted, failed, operator stop) and has none queued yet, in
// enrolled repositories only.
func (s *Store) OutcomesNeedingComment(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `select o.attempt_id from attempt_outcomes o
		join attempts a on a.id = o.attempt_id
		join cases c on c.id = a.case_id
		left join attempt_outcome_aborts b on b.attempt_id = o.attempt_id
		left join attempt_outcome_comments m on m.attempt_id = o.attempt_id
		left join repository_removals x on x.repository = c.repository
		where m.attempt_id is null and x.repository is null
			and (o.kind in ('budget_exhausted', 'failed') or b.reason = 'operator_stop')
		order by o.ended_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
