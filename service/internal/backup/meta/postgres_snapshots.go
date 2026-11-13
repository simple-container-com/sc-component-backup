// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/stats"
)

// postgresSnapshotStore mirrors sqliteSnapshotStore. BIGSERIAL id,
// JSONB data columns. PG's `RETURNING id` makes the autoassign path
// one roundtrip instead of INSERT + LastInsertId.
type postgresSnapshotStore struct {
	db   *sql.DB
	runs *postgresRunStore
}

func (s *postgresSnapshotStore) PutDbSnapshot(ctx context.Context, snap stats.DbSnapshot) error {
	raw, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("postgres snapshots put: marshal: %w", err)
	}
	tsStr := snap.Ts.UTC().Format(time.RFC3339Nano)
	if snap.ID != 0 {
		const q = `
INSERT INTO db_snapshots(id, project, database, ts, data)
VALUES ($1, $2, $3, $4, $5::jsonb)
ON CONFLICT(id) DO UPDATE SET
    project=EXCLUDED.project,
    database=EXCLUDED.database,
    ts=EXCLUDED.ts,
    data=EXCLUDED.data
`
		if _, err := s.db.ExecContext(ctx, q, snap.ID, snap.Project, snap.Database, tsStr, string(raw)); err != nil {
			return fmt.Errorf("postgres snapshots put by-id: %w", err)
		}
		return nil
	}
	var newID int64
	err = s.db.QueryRowContext(
		ctx,
		`INSERT INTO db_snapshots(project, database, ts, data) VALUES ($1, $2, $3, $4::jsonb) RETURNING id`,
		snap.Project, snap.Database, tsStr, string(raw),
	).Scan(&newID)
	if err != nil {
		return fmt.Errorf("postgres snapshots put autoinc: %w", err)
	}
	snap.ID = newID
	raw, err = json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("postgres snapshots put rewrap: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE db_snapshots SET data = $1::jsonb WHERE id = $2`, string(raw), newID); err != nil {
		return fmt.Errorf("postgres snapshots put rewrite: %w", err)
	}
	return nil
}

func (s *postgresSnapshotStore) PutObjectSamples(ctx context.Context, project, database, _ string, snapshotID int64, samples []stats.ObjectSample) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres samples put: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(
		ctx,
		`DELETE FROM object_samples WHERE snapshot_id = $1 AND project = $2 AND database = $3`,
		snapshotID, project, database,
	); err != nil {
		return fmt.Errorf("postgres samples put clear: %w", err)
	}
	if len(samples) == 0 {
		return tx.Commit()
	}
	var tsStr string
	if err := tx.QueryRowContext(
		ctx,
		`SELECT ts FROM db_snapshots WHERE id = $1`, snapshotID,
	).Scan(&tsStr); err != nil {
		return fmt.Errorf("postgres samples put snap-ts: %w", err)
	}
	for _, sample := range samples {
		sample.SnapshotID = snapshotID
		raw, err := json.Marshal(sample)
		if err != nil {
			return fmt.Errorf("postgres samples put marshal: %w", err)
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO object_samples(project, database, snapshot_id, ts, data) VALUES ($1, $2, $3, $4, $5::jsonb)`,
			project, database, snapshotID, tsStr, string(raw),
		); err != nil {
			return fmt.Errorf("postgres samples put insert: %w", err)
		}
	}
	return tx.Commit()
}

func (s *postgresSnapshotStore) SnapshotsForProject(ctx context.Context, project string, since time.Time) ([]stats.DbSnapshot, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if since.IsZero() {
		rows, err = s.db.QueryContext(ctx,
			`SELECT data::text FROM db_snapshots WHERE project = $1 ORDER BY ts DESC`, project)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT data::text FROM db_snapshots WHERE project = $1 AND ts >= $2 ORDER BY ts DESC`,
			project, since.UTC().Format(time.RFC3339Nano))
	}
	if err != nil {
		return nil, fmt.Errorf("postgres snapshots list: %w", err)
	}
	defer rows.Close()
	var out []stats.DbSnapshot
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("postgres snapshots list scan: %w", err)
		}
		var snap stats.DbSnapshot
		if err := json.Unmarshal([]byte(raw), &snap); err != nil {
			return nil, fmt.Errorf("postgres snapshots list unmarshal: %w", err)
		}
		out = append(out, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres snapshots list rows: %w", err)
	}
	return out, nil
}

func (s *postgresSnapshotStore) ObjectSamplesForProject(ctx context.Context, project, database string, since time.Time) ([]stats.ObjectSample, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if since.IsZero() {
		rows, err = s.db.QueryContext(ctx,
			`SELECT data::text FROM object_samples WHERE project = $1 AND database = $2 ORDER BY ts DESC`,
			project, database)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT data::text FROM object_samples WHERE project = $1 AND database = $2 AND ts >= $3 ORDER BY ts DESC`,
			project, database, since.UTC().Format(time.RFC3339Nano))
	}
	if err != nil {
		return nil, fmt.Errorf("postgres samples list: %w", err)
	}
	defer rows.Close()
	var out []stats.ObjectSample
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("postgres samples list scan: %w", err)
		}
		var sample stats.ObjectSample
		if err := json.Unmarshal([]byte(raw), &sample); err != nil {
			return nil, fmt.Errorf("postgres samples list unmarshal: %w", err)
		}
		out = append(out, sample)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres samples list rows: %w", err)
	}
	return out, nil
}

func (s *postgresSnapshotStore) RunResultsForProject(ctx context.Context, project string, since time.Time) ([]stats.DbResult, error) {
	runs, err := s.runs.ListBackupsByTenant(ctx, project)
	if err != nil {
		return nil, err
	}
	return scheduler.ProjectDbResults(runs, since), nil
}

func (s *postgresSnapshotStore) Close(_ context.Context) error { return nil }
