package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/guygrigsby/autophage/internal/resolution"
)

// EnrollRepository inserts or re-enrolls: installation id and default
// branch are refreshed and a removal row, if any, is deleted.
func (s *Store) EnrollRepository(ctx context.Context, r resolution.Repository) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into repositories (full_name, installation_id, default_branch, enrolled_at)
			values ($1, $2, $3, $4)
			on conflict (full_name) do update set installation_id = excluded.installation_id, default_branch = excluded.default_branch`,
			r.FullName, r.InstallationID, r.DefaultBranch, r.EnrolledAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `delete from repository_removals where repository = $1`, r.FullName); err != nil {
			return err
		}
		return notify(ctx, tx, "repository:"+r.FullName)
	})
}

// RemoveRepository records the removal. Unknown repositories are ignored.
func (s *Store) RemoveRepository(ctx context.Context, fullName string, at time.Time) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into repository_removals (repository, removed_at)
			select full_name, $2 from repositories where full_name = $1
			on conflict (repository) do nothing`, fullName, at); err != nil {
			return err
		}
		return notify(ctx, tx, "repository:"+fullName)
	})
}

func (s *Store) GetRepository(ctx context.Context, fullName string) (resolution.Repository, error) {
	var r resolution.Repository
	var removedAt *time.Time
	err := s.pool.QueryRow(ctx, `select r.full_name, r.installation_id, r.default_branch, r.enrolled_at, x.removed_at
		from repositories r left join repository_removals x on x.repository = r.full_name
		where r.full_name = $1`, fullName).Scan(&r.FullName, &r.InstallationID, &r.DefaultBranch, &r.EnrolledAt, &removedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return resolution.Repository{}, ErrNotFound
	}
	if err != nil {
		return resolution.Repository{}, err
	}
	if removedAt != nil {
		r.Removed = &resolution.Removal{RemovedAt: *removedAt}
	}
	return r, nil
}

// RepositoriesNeedingLabel lists enrolled repositories with no label setup.
func (s *Store) RepositoriesNeedingLabel(ctx context.Context) ([]resolution.Repository, error) {
	rows, err := s.pool.Query(ctx, `select r.full_name, r.installation_id, r.default_branch, r.enrolled_at
		from repositories r
		left join repository_removals x on x.repository = r.full_name
		left join repository_label_setups l on l.repository = r.full_name
		where x.repository is null and l.repository is null
		order by r.enrolled_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []resolution.Repository
	for rows.Next() {
		var r resolution.Repository
		if err := rows.Scan(&r.FullName, &r.InstallationID, &r.DefaultBranch, &r.EnrolledAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) RecordLabelSetup(ctx context.Context, fullName, label string) error {
	_, err := s.pool.Exec(ctx, `insert into repository_label_setups (repository, label) values ($1, $2)
		on conflict (repository) do nothing`, fullName, label)
	return err
}
