// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"context"
	"sort"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/retention"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/stats"
)

type colocSnapshots struct{ s *colocatedStore }

func snapshotKey(project, database string, ts time.Time) string {
	return metaPrefix + "snapshots/" + retention.Enc(project) + "/" +
		retention.Enc(database) + "/" + retention.FormatTS(ts) + ".json"
}

func samplesKey(project, database string, ts time.Time) string {
	return metaPrefix + "samples/" + retention.Enc(project) + "/" +
		retention.Enc(database) + "/" + retention.FormatTS(ts) + ".json"
}

func (p colocSnapshots) PutDbSnapshot(ctx context.Context, snap stats.DbSnapshot) error {
	return p.s.putJSON(ctx, snapshotKey(snap.Project, snap.Database, snap.Ts), snap)
}

func (p colocSnapshots) PutObjectSamples(ctx context.Context, project, database, key string, snapshotID int64, samples []stats.ObjectSample) error {
	ts := timeFromKey(key)
	if ts.IsZero() {
		return p.s.putJSON(ctx,
			metaPrefix+"samples/"+retention.Enc(project)+"/"+retention.Enc(database)+"/id-"+itoaInt64(snapshotID)+".json",
			samples)
	}
	return p.s.putJSON(ctx, samplesKey(project, database, ts), samples)
}

func (p colocSnapshots) SnapshotsForProject(ctx context.Context, project string, since time.Time) ([]stats.DbSnapshot, error) {
	prefix := metaPrefix + "snapshots/" + retention.Enc(project) + "/"
	rows, err := p.s.archive.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	out := make([]stats.DbSnapshot, 0, len(rows))
	for _, obj := range rows {
		var snap stats.DbSnapshot
		if ok, err := p.s.getJSON(ctx, obj.Path, &snap); err != nil || !ok {
			continue
		}
		if !since.IsZero() && snap.Ts.Before(since) {
			continue
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ts.After(out[j].Ts) })
	return out, nil
}

func (p colocSnapshots) ObjectSamplesForProject(ctx context.Context, project, database string, since time.Time) ([]stats.ObjectSample, error) {
	prefix := metaPrefix + "samples/" + retention.Enc(project) + "/" + retention.Enc(database) + "/"
	rows, err := p.s.archive.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path > rows[j].Path })
	out := []stats.ObjectSample{}
	for _, obj := range rows {
		if !since.IsZero() {
			ts := timeFromKey(obj.Path)
			if !ts.IsZero() && ts.Before(since) {
				continue
			}
		}
		var batch []stats.ObjectSample
		if ok, err := p.s.getJSON(ctx, obj.Path, &batch); err != nil || !ok {
			continue
		}
		out = append(out, batch...)
	}
	return out, nil
}

func (p colocSnapshots) RunResultsForProject(ctx context.Context, project string, since time.Time) ([]stats.DbResult, error) {
	runs, err := (colocRuns{p.s}).ListBackupsByTenant(ctx, project)
	if err != nil {
		return nil, err
	}
	return scheduler.ProjectDbResults(runs, since), nil
}

func (p colocSnapshots) Close(_ context.Context) error { return nil }

func timeFromKey(key string) time.Time {
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == '/' {
			base := key[i+1:]
			if len(base) > len(".json") && base[len(base)-len(".json"):] == ".json" {
				base = base[:len(base)-len(".json")]
			}
			t, err := time.Parse(retention.TimestampFormat, base)
			if err == nil {
				return t
			}
			return time.Time{}
		}
	}
	return time.Time{}
}

func itoaInt64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
