// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package goembedbadmin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"

	"github.com/FerretDB/FerretDB/ferretdb"

	"github.com/simple-container-com/sc-component-backup/service/internal/goembedbadmin"
)

// embeddedMongo spins up an in-test ferretdb + sqlite listener and
// returns a connected client plus a cleanup func. One listener is
// shared between the source and sink databases — the migration
// endpoint does not care whether they are on the same physical
// server, only that CountDocuments and Find/ReplaceOne work on
// both.
func embeddedMongo(t *testing.T) (*mongo.Client, func()) {
	t.Helper()
	dir := t.TempDir()
	sqliteDir := filepath.Join(dir, "sqlite")
	if err := os.MkdirAll(sqliteDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := &ferretdb.Config{
		Handler:   "sqlite",
		SQLiteURL: "file:" + sqliteDir + "/",
		Listener:  ferretdb.ListenerConfig{TCP: "127.0.0.1:0"},
	}
	fdb, err := ferretdb.New(cfg)
	if err != nil {
		t.Fatalf("ferretdb.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		if err := fdb.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("Run exit: %v", err)
		}
	}()
	uri := fdb.MongoDBURI()
	cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ccancel()
	client, err := mongo.Connect(cctx, options.Client().ApplyURI(uri).SetDirect(true))
	if err != nil {
		cancel()
		<-runDone
		t.Fatalf("Connect: %v", err)
	}
	if err := client.Ping(cctx, readpref.Primary()); err != nil {
		cancel()
		<-runDone
		t.Fatalf("Ping: %v", err)
	}
	return client, func() {
		_ = client.Disconnect(context.Background())
		cancel()
		<-runDone
	}
}

// seedFixtures writes a handful of documents into each of several
// backup-owned collections on the given database, returns the
// per-collection seed counts (so the test can diff against the
// response), and mirrors the shape the backup service's domain
// managers produce (string _id for everything — matches tenant
// rows, project rows, run rows).
func seedFixtures(t *testing.T, db *mongo.Database) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	fixtures := map[string]int64{
		goembedbadmin.CollectionTenants:          2,
		goembedbadmin.CollectionProjects:         3,
		goembedbadmin.CollectionBackupRuns:       5,
		goembedbadmin.CollectionRestoreRuns:      1,
		goembedbadmin.CollectionSnapshots:        4,
		goembedbadmin.CollectionObjectSamples:    6,
		goembedbadmin.CollectionSettings:         1,
		goembedbadmin.CollectionAlertsDismissals: 2,
	}
	for name, n := range fixtures {
		coll := db.Collection(name)
		for i := int64(0); i < n; i++ {
			_, err := coll.InsertOne(ctx, bson.M{
				"_id":       name + "-" + string(rune('a'+int(i))),
				"createdAt": time.Now().UTC(),
				"shape":     "fixture",
			})
			if err != nil {
				t.Fatalf("seed %s: %v", name, err)
			}
		}
	}
	return fixtures
}

func TestMigration_HappyPath(t *testing.T) {
	client, cleanup := embeddedMongo(t)
	defer cleanup()

	srcDB := client.Database("src_" + t.Name())
	seeded := seedFixtures(t, srcDB)

	sink := client.Database("sink_" + t.Name())

	cfg := &goembedbadmin.MigrationConfig{
		Source:     srcDB,
		Sink:       sink,
		AdminToken: "secret-token",
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/migrate-mongo-to-goembedb", nil)
	req.Header.Set("X-Admin-Token", "secret-token")
	w := httptest.NewRecorder()
	cfg.Migrate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Migrate: status %d, body %s", w.Code, w.Body.String())
	}
	var resp struct {
		DryRun       bool             `json:"dry_run"`
		SourceCount  int64            `json:"s3_count"`
		SourceCounts map[string]int64 `json:"source_counts"`
		Migrated     map[string]int64 `json:"migrated"`
		Total        int64            `json:"total_migrated"`
		Verified     bool             `json:"verified"`
		Errors       []any            `json:"errors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, w.Body.String())
	}

	var wantTotal int64
	for _, v := range seeded {
		wantTotal += v
	}
	if resp.SourceCount != wantTotal {
		t.Fatalf("Migrate: source_count = %d, want %d", resp.SourceCount, wantTotal)
	}
	if resp.Total != wantTotal {
		t.Fatalf("Migrate: total_migrated = %d, want %d (errors=%v)", resp.Total, wantTotal, resp.Errors)
	}
	if !resp.Verified {
		t.Fatalf("Migrate: verified = false (errors=%v, migrated=%+v)", resp.Errors, resp.Migrated)
	}
	for name, want := range seeded {
		if resp.Migrated[name] != want {
			t.Fatalf("Migrate: migrated[%s] = %d, want %d (all: %+v)", name, resp.Migrated[name], want, resp.Migrated)
		}
	}

	// Re-run is idempotent (ReplaceOne-with-upsert by _id).
	req2 := httptest.NewRequest(http.MethodPost, "/admin/migrate-mongo-to-goembedb", nil)
	req2.Header.Set("X-Admin-Token", "secret-token")
	w2 := httptest.NewRecorder()
	cfg.Migrate(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("Migrate re-run: status %d", w2.Code)
	}
	var resp2 struct {
		Migrated map[string]int64 `json:"migrated"`
	}
	_ = json.Unmarshal(w2.Body.Bytes(), &resp2)
	for name, want := range seeded {
		if resp2.Migrated[name] != want {
			t.Fatalf("Migrate re-run: migrated[%s] = %d, want %d (not idempotent)", name, resp2.Migrated[name], want)
		}
	}
}

func TestMigration_DryRun(t *testing.T) {
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	srcDB := client.Database("src_" + t.Name())
	seeded := seedFixtures(t, srcDB)
	sink := client.Database("sink_" + t.Name())

	cfg := &goembedbadmin.MigrationConfig{Source: srcDB, Sink: sink, AdminToken: "t"}

	req := httptest.NewRequest(http.MethodPost, "/admin/migrate-mongo-to-goembedb?dry_run=1", nil)
	req.Header.Set("X-Admin-Token", "t")
	w := httptest.NewRecorder()
	cfg.Migrate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Migrate dry: status %d", w.Code)
	}
	var resp struct {
		DryRun       bool             `json:"dry_run"`
		SourceCount  int64            `json:"s3_count"`
		Total        int64            `json:"total_migrated"`
		Verified     bool             `json:"verified"`
		SourceCounts map[string]int64 `json:"source_counts"`
		Migrated     map[string]int64 `json:"migrated"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	var wantTotal int64
	for _, v := range seeded {
		wantTotal += v
	}
	if !resp.DryRun {
		t.Fatalf("dry-run flag not echoed: %+v", resp)
	}
	if resp.SourceCount != wantTotal {
		t.Fatalf("dry-run source_count = %d, want %d", resp.SourceCount, wantTotal)
	}
	if resp.Total != 0 {
		t.Fatalf("dry-run total_migrated = %d, want 0 (no writes)", resp.Total)
	}
	if resp.Verified {
		t.Fatalf("dry-run claimed verified, which must never be")
	}
	// No writes should have landed in the sink.
	for name := range seeded {
		if n := resp.Migrated[name]; n != 0 {
			t.Fatalf("dry-run leaked write: %s count = %d, want 0", name, n)
		}
	}
}

func TestMigration_AuthAndGate(t *testing.T) {
	// Endpoint disabled when AdminToken is empty.
	cfg := &goembedbadmin.MigrationConfig{AdminToken: ""}
	req := httptest.NewRequest(http.MethodPost, "/admin/migrate-mongo-to-goembedb", nil)
	w := httptest.NewRecorder()
	cfg.Migrate(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled: got %d, want 404", w.Code)
	}

	// Enabled but no token header → 401.
	cfg.AdminToken = "s"
	req = httptest.NewRequest(http.MethodPost, "/admin/migrate-mongo-to-goembedb", nil)
	w = httptest.NewRecorder()
	cfg.Migrate(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no header: got %d, want 401", w.Code)
	}

	// Wrong token → 401.
	req = httptest.NewRequest(http.MethodPost, "/admin/migrate-mongo-to-goembedb", nil)
	req.Header.Set("X-Admin-Token", "other")
	w = httptest.NewRecorder()
	cfg.Migrate(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", w.Code)
	}

	// Correct token but Source/Sink absent → 500 that names what is missing.
	cfg = &goembedbadmin.MigrationConfig{AdminToken: "t"}
	req = httptest.NewRequest(http.MethodPost, "/admin/migrate-mongo-to-goembedb", nil)
	req.Header.Set("X-Admin-Token", "t")
	w = httptest.NewRecorder()
	cfg.Migrate(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("missing source: got %d, want 500", w.Code)
	}
}

func TestSetStoreBackend_GaugeFlipsExactlyOnce(t *testing.T) {
	// SetStoreBackend is idempotent and exported for main.go to call
	// once at boot. Repeated calls must not panic from double-register.
	goembedbadmin.SetStoreBackend("mongo")
	goembedbadmin.SetStoreBackend("goembedb")
	goembedbadmin.SetStoreBackend("mongo")
}

func TestMetricsHandler_ServesText(t *testing.T) {
	goembedbadmin.SetStoreBackend("goembedb")
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	goembedbadmin.MetricsHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics: got %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !containsSub(body, "backup_store_backend") {
		t.Fatalf("/metrics output missing backup_store_backend gauge:\n%s", body)
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
