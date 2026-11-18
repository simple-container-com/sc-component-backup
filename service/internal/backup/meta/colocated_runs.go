// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/retention"
)

type colocRuns struct{ s *colocatedStore }

func backupRunKey(r backup.BackupRun) string {
	return metaPrefix + "runs/" + retention.Enc(r.TenantStack) + "/" +
		retention.FormatTS(r.StartedAt) + "-" + retention.Enc(r.ID) + ".json"
}

func restoreRunKey(r backup.RestoreRun) string {
	return metaPrefix + "restores/" + retention.Enc(r.TenantStack) + "/" +
		retention.FormatTS(r.StartedAt) + "-" + retention.Enc(r.ID) + ".json"
}

func (r colocRuns) PutBackup(ctx context.Context, run backup.BackupRun) error {
	return r.s.putJSON(ctx, backupRunKey(run), run)
}

func (r colocRuns) UpdateBackup(ctx context.Context, run backup.BackupRun) error {
	return r.PutBackup(ctx, run)
}

func (r colocRuns) GetBackup(ctx context.Context, id string) (backup.BackupRun, error) {
	key, ok, err := r.findRunKey(ctx, metaPrefix+"runs/", id)
	if err != nil {
		return backup.BackupRun{}, err
	}
	if !ok {
		return backup.BackupRun{}, fmt.Errorf("backup run %q not found", id)
	}
	var out backup.BackupRun
	if _, err := r.s.getJSON(ctx, key, &out); err != nil {
		return backup.BackupRun{}, err
	}
	return out, nil
}

func (r colocRuns) ListBackupsByTenant(ctx context.Context, tenantStack string) ([]backup.BackupRun, error) {
	prefix := metaPrefix + "runs/" + retention.Enc(tenantStack) + "/"
	rows, err := r.s.archive.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	out := make([]backup.BackupRun, 0, len(rows))
	for _, obj := range rows {
		var run backup.BackupRun
		if ok, err := r.s.getJSON(ctx, obj.Path, &run); err != nil || !ok {
			continue
		}
		out = append(out, run)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

// DeleteBackupsByTenant removes every run JSON under the tenant's
// runs prefix. Returns the count removed. Idempotent — a missing
// prefix returns 0 with no error. Caller is responsible for
// separately removing the archive objects themselves; these are the
// retention metadata rows, not the archives.
func (r colocRuns) DeleteBackupsByTenant(ctx context.Context, tenantStack string) (int, error) {
	prefix := metaPrefix + "runs/" + retention.Enc(tenantStack) + "/"
	rows, err := r.s.archive.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, obj := range rows {
		if err := r.s.archive.Delete(ctx, obj.Path); err != nil {
			continue
		}
		n++
	}
	return n, nil
}

// ListAllBackups walks every tenant's runs prefix and merges. S3 LIST
// is O(#rows) per tenant and there is no server-side cross-prefix sort;
// we sort client-side after reading. For cross-tenant /history on a
// colocated-meta deployment this is fine while total row count stays
// in the low thousands — the Mongo-backed store is preferred for
// scale. Unlike ListBackupsByTenant, we call `List` with just the
// runs root prefix so we visit every tenant folder in one shot.
func (r colocRuns) ListAllBackups(ctx context.Context, limit int) ([]backup.BackupRun, error) {
	rows, err := r.s.archive.List(ctx, metaPrefix+"runs/")
	if err != nil {
		return nil, err
	}
	out := make([]backup.BackupRun, 0, len(rows))
	for _, obj := range rows {
		var run backup.BackupRun
		if ok, err := r.s.getJSON(ctx, obj.Path, &run); err != nil || !ok {
			continue
		}
		out = append(out, run)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r colocRuns) PutRestore(ctx context.Context, run backup.RestoreRun) error {
	return r.s.putJSON(ctx, restoreRunKey(run), run)
}

func (r colocRuns) UpdateRestore(ctx context.Context, run backup.RestoreRun) error {
	return r.PutRestore(ctx, run)
}

func (r colocRuns) GetRestore(ctx context.Context, id string) (backup.RestoreRun, error) {
	key, ok, err := r.findRunKey(ctx, metaPrefix+"restores/", id)
	if err != nil {
		return backup.RestoreRun{}, err
	}
	if !ok {
		return backup.RestoreRun{}, fmt.Errorf("restore run %q not found", id)
	}
	var out backup.RestoreRun
	if _, err := r.s.getJSON(ctx, key, &out); err != nil {
		return backup.RestoreRun{}, err
	}
	return out, nil
}

func (r colocRuns) findRunKey(ctx context.Context, prefix, id string) (string, bool, error) {
	rows, err := r.s.archive.List(ctx, prefix)
	if err != nil {
		return "", false, err
	}
	suffix := "-" + retention.Enc(id) + ".json"
	for _, obj := range rows {
		if strings.HasSuffix(obj.Path, suffix) {
			return obj.Path, true, nil
		}
	}
	return "", false, nil
}
