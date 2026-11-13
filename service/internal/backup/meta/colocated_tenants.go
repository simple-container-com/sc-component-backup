// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"context"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/retention"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

type colocTenants struct{ s *colocatedStore }

func tenantKey(stackName string) string {
	return metaPrefix + "tenants/" + retention.Enc(stackName) + ".json"
}

func (t colocTenants) Upsert(ctx context.Context, record tenant.Record) error {
	return t.s.putJSON(ctx, tenantKey(record.StackName), record)
}

func (t colocTenants) List(ctx context.Context) ([]tenant.Record, error) {
	rows, err := t.s.archive.List(ctx, metaPrefix+"tenants/")
	if err != nil {
		return nil, err
	}
	out := make([]tenant.Record, 0, len(rows))
	for _, obj := range rows {
		var rec tenant.Record
		if ok, err := t.s.getJSON(ctx, obj.Path, &rec); err != nil || !ok {
			continue
		}
		if rec.SoftDeleted {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

func (t colocTenants) Get(ctx context.Context, stackName string) (tenant.Record, bool, error) {
	var rec tenant.Record
	ok, err := t.s.getJSON(ctx, tenantKey(stackName), &rec)
	if err != nil || !ok {
		return tenant.Record{}, false, err
	}
	if rec.SoftDeleted {
		return tenant.Record{}, false, nil
	}
	return rec, true, nil
}

func (t colocTenants) SoftDelete(ctx context.Context, stackName string) error {
	var rec tenant.Record
	ok, err := t.s.getJSON(ctx, tenantKey(stackName), &rec)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	rec.SoftDeleted = true
	return t.s.putJSON(ctx, tenantKey(stackName), rec)
}

func (t colocTenants) Close(_ context.Context) error { return nil }
