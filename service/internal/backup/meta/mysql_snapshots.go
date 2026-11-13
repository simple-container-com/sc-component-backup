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

// mysqlSnapshotStore mirrors sqliteSnapshotStore. `database` is a
// MySQL reserved word, so every reference is backticked. MySQL has no
// `RETURNING id` — use LastInsertId after INSERT.
type mysqlSnapshotStore struct {
	db   *sql.DB
	runs *mysqlRunStore
}

func (s *mysqlSnapshotStore) PutDbSnapshot(ctx context.Context, snap stats.DbSnapshot) error {
	raw, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("mysql snapshots put: marshal: %w", err)
	}
	tsStr := snap.Ts.UTC().Format(time.RFC3339Nano)
	if snap.ID != 0 {
		const q = "INSERT INTO db_snapshots(id, project, `database`, ts, data) VALUES (?, ?, ?, ?, ?) " +
			"ON DUPLICATE KEY UPDATE project=VALUES(project), `database`=VALUES(`database`), ts=VALUES(ts), data=VALUES(data)"
		if _, err := s.db.ExecContext(ctx, q, snap.ID, snap.Project, snap.Database, tsStr, string(raw)); err != nil {
			return fmt.Errorf("mysql snapshots put by-id: %w", err)
		}
		return nil
	}
	res, err := s.db.ExecContext(
		ctx,
		"INSERT INTO db_snapshots(project, `database`, ts, data) VALUES (?, ?, ?, ?)",
		snap.Project, snap.Database, tsStr, string(raw),
	)
	if err != nil {
		return fmt.Errorf("mysql snapshots put autoinc: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("mysql snapshots put last-insert-id: %w", err)
	}
	snap.ID = id
	raw, err = json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("mysql snapshots put rewrap: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE db_snapshots SET data = ? WHERE id = ?`, string(raw), id); err != nil {
		return fmt.Errorf("mysql snapshots put rewrite: %w", err)
	}
	return nil
}

func (s *mysqlSnapshotStore) PutObjectSamples(ctx context.Context, project, database, _ string, snapshotID int64, samples []stats.ObjectSample) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mysql samples put: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(
		ctx,
		"DELETE FROM object_samples WHERE snapshot_id = ? AND project = ? AND `database` = ?",
		snapshotID, project, database,
	); err != nil {
		return fmt.Errorf("mysql samples put clear: %w", err)
	}
	if len(samples) == 0 {
		return tx.Commit()
	}
	var tsStr string
	if err := tx.QueryRowContext(
		ctx,
		`SELECT ts FROM db_snapshots WHERE id = ?`, snapshotID,
	).Scan(&tsStr); err != nil {
		return fmt.Errorf("mysql samples put snap-ts: %w", err)
	}
	for _, sample := range samples {
		sample.SnapshotID = snapshotID
		raw, err := json.Marshal(sample)
		if err != nil {
			return fmt.Errorf("mysql samples put marshal: %w", err)
		}
		if _, err := tx.ExecContext(
			ctx,
			"INSERT INTO object_samples(project, `database`, snapshot_id, ts, data) VALUES (?, ?, ?, ?, ?)",
			project, database, snapshotID, tsStr, string(raw),
		); err != nil {
			return fmt.Errorf("mysql samples put insert: %w", err)
		}
	}
	return tx.Commit()
}

func (s *mysqlSnapshotStore) SnapshotsForProject(ctx context.Context, project string, since time.Time) ([]stats.DbSnapshot, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if since.IsZero() {
		rows, err = s.db.QueryContext(ctx,
			`SELECT data FROM db_snapshots WHERE project = ? ORDER BY ts DESC`, project)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT data FROM db_snapshots WHERE project = ? AND ts >= ? ORDER BY ts DESC`,
			project, since.UTC().Format(time.RFC3339Nano))
	}
	if err != nil {
		return nil, fmt.Errorf("mysql snapshots list: %w", err)
	}
	defer rows.Close()
	var out []stats.DbSnapshot
	for rows.Next() {
		var raw sql.RawBytes
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("mysql snapshots list scan: %w", err)
		}
		var snap stats.DbSnapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			return nil, fmt.Errorf("mysql snapshots list unmarshal: %w", err)
		}
		out = append(out, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql snapshots list rows: %w", err)
	}
	return out, nil
}

func (s *mysqlSnapshotStore) ObjectSamplesForProject(ctx context.Context, project, database string, since time.Time) ([]stats.ObjectSample, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if since.IsZero() {
		rows, err = s.db.QueryContext(ctx,
			"SELECT data FROM object_samples WHERE project = ? AND `database` = ? ORDER BY ts DESC",
			project, database)
	} else {
		rows, err = s.db.QueryContext(ctx,
			"SELECT data FROM object_samples WHERE project = ? AND `database` = ? AND ts >= ? ORDER BY ts DESC",
			project, database, since.UTC().Format(time.RFC3339Nano))
	}
	if err != nil {
		return nil, fmt.Errorf("mysql samples list: %w", err)
	}
	defer rows.Close()
	var out []stats.ObjectSample
	for rows.Next() {
		var raw sql.RawBytes
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("mysql samples list scan: %w", err)
		}
		var sample stats.ObjectSample
		if err := json.Unmarshal(raw, &sample); err != nil {
			return nil, fmt.Errorf("mysql samples list unmarshal: %w", err)
		}
		out = append(out, sample)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql samples list rows: %w", err)
	}
	return out, nil
}

func (s *mysqlSnapshotStore) RunResultsForProject(ctx context.Context, project string, since time.Time) ([]stats.DbResult, error) {
	runs, err := s.runs.ListBackupsByTenant(ctx, project)
	if err != nil {
		return nil, err
	}
	return scheduler.ProjectDbResults(runs, since), nil
}

func (s *mysqlSnapshotStore) Close(_ context.Context) error { return nil }
