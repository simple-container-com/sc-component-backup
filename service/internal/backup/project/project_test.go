// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package project

import (
	"context"
	"testing"
	"time"
)

func TestInMemoryStore_CreateGetUpdateDelete(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryStore()

	cfg := Config{
		Name:              "demo",
		Engine:            "mongodb",
		Databases:         []string{"db1"},
		Interval:          "1h",
		Timeout:           "1h",
		Retention:         Retention{Last: 7, Daily: 7},
		Retries:           3,
		ShrinkWarnPercent: 50,
		DumpArgs:          []string{"--quiet"},
		Globals:           false,
	}

	// Create → get fresh revision + timestamps.
	created, err := s.Create(ctx, cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Revision == "" {
		t.Fatal("Create should mint a revision")
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatal("Create should stamp timestamps")
	}

	// Get → sees the row.
	got, ok, err := s.Get(ctx, "demo")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Revision != created.Revision {
		t.Errorf("Get revision drift: %q vs %q", got.Revision, created.Revision)
	}

	// Create again → ErrConflict.
	if _, err := s.Create(ctx, cfg); err != ErrConflict {
		t.Errorf("Create duplicate: want ErrConflict, got %v", err)
	}

	// Update with stale revision → ErrRevisionMismatch.
	stale := created
	stale.Revision = "wrong"
	stale.Interval = "6h"
	if _, err := s.Update(ctx, stale); err != ErrRevisionMismatch {
		t.Errorf("Update with stale revision: want ErrRevisionMismatch, got %v", err)
	}

	// Update with correct revision → bumped revision + preserved CreatedAt.
	fresh := created
	fresh.Interval = "6h"
	updated, err := s.Update(ctx, fresh)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Revision == created.Revision {
		t.Error("Update should mint a NEW revision")
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("Update should preserve CreatedAt: %v vs %v", updated.CreatedAt, created.CreatedAt)
	}
	if updated.Interval != "6h" {
		t.Errorf("Update should apply new field: %q", updated.Interval)
	}

	// SetSecret → row carries the URI; revision unchanged.
	if err := s.SetSecret(ctx, "demo", "mongodb://user:pass@h/"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	withSecret, _, _ := s.Get(ctx, "demo")
	if withSecret.SecretURI != "mongodb://user:pass@h/" {
		t.Errorf("SetSecret didn't persist")
	}
	if withSecret.Revision != updated.Revision {
		t.Errorf("SetSecret should not bump revision; got %q vs %q", withSecret.Revision, updated.Revision)
	}

	// SoftDelete with stale revision → ErrRevisionMismatch.
	if err := s.SoftDelete(ctx, "demo", "wrong"); err != ErrRevisionMismatch {
		t.Errorf("SoftDelete stale: want ErrRevisionMismatch, got %v", err)
	}

	// SoftDelete with correct revision → Get now returns ok=false.
	if err := s.SoftDelete(ctx, "demo", updated.Revision); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	_, ok, _ = s.Get(ctx, "demo")
	if ok {
		t.Error("SoftDelete didn't hide the row from Get")
	}

	// Create again after SoftDelete → succeeds (resurrection).
	cfg2 := cfg
	cfg2.Interval = "30m"
	res, err := s.Create(ctx, cfg2)
	if err != nil {
		t.Fatalf("Create after SoftDelete: %v", err)
	}
	if res.Revision == updated.Revision {
		t.Error("Resurrected create should mint a new revision")
	}
}

func TestInMemoryStore_List(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryStore()

	for _, n := range []string{"a", "b", "c"} {
		_, err := s.Create(ctx, Config{Name: n, Engine: "mongodb"})
		if err != nil {
			t.Fatalf("Create %s: %v", n, err)
		}
	}
	// Soft-delete the middle one.
	b, _, _ := s.Get(ctx, "b")
	_ = s.SoftDelete(ctx, "b", b.Revision)

	rows, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("List: got %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.SoftDeleted {
			t.Errorf("List returned soft-deleted row %q", r.Name)
		}
	}
}

func TestNewRevision_Unique(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 1000; i++ {
		r := NewRevision()
		if _, dup := seen[r]; dup {
			t.Fatalf("NewRevision collision at iteration %d: %q", i, r)
		}
		seen[r] = struct{}{}
	}
}

func TestInMemoryStore_GetSoftDeletedReturnsFalse(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryStore()
	created, _ := s.Create(ctx, Config{Name: "x", Engine: "mongodb"})
	_ = s.SoftDelete(ctx, "x", created.Revision)
	_, ok, _ := s.Get(ctx, "x")
	if ok {
		t.Fatal("Get should return ok=false for soft-deleted row")
	}
	// Second SoftDelete should be ErrNotFound (not ErrRevisionMismatch).
	if err := s.SoftDelete(ctx, "x", created.Revision); err != ErrNotFound {
		t.Errorf("Double SoftDelete: want ErrNotFound, got %v", err)
	}
}

// Belt-and-suspenders: ensure the timestamps are actually UTC.
func TestInMemoryStore_TimestampsUTC(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryStore()
	cfg, _ := s.Create(ctx, Config{Name: "x", Engine: "mongodb"})
	if _, offset := cfg.CreatedAt.Zone(); offset != 0 {
		t.Errorf("CreatedAt should be UTC; offset=%d", offset)
	}
	// And now > createdAt (unless the test is somehow travelling in time).
	if cfg.CreatedAt.After(time.Now().UTC()) {
		t.Error("CreatedAt is in the future")
	}
}
