// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pkg/errors"

	"github.com/simple-container-com/sc-component-backup/plugin/internal/atlas"
)

// Slice 1d scoped-user provisioning (sc-components-platform design doc §5.2).
//
// When a consumer `uses: [the-backup, the-mongo]`, sc-plugin-backup's
// ComputeProcess calls Atlas REST (with the operator-supplied admin
// credential from `config.atlasAdmin:`) to mint `backup-<consumer-stack>`
// with `readWrite` on the consumer's specific DB. The scoped user is
// deleted when the consumer's `sc destroy` fires PreDestroy on the
// backup resource.

// BackupDatabase mirrors the api.BackupDatabase shape without taking a
// dependency on the api/ module from the plugin's compute path (which
// stays plugin-bus-shape). populateDatabasesFromRaw extracts it from
// the ComponentRaw map.
type BackupDatabase struct {
	Resource string
	Driver   string
}

// AtlasAdminRef mirrors api.BackupAtlasAdmin similarly.
type AtlasAdminRef struct {
	PrivateKey string
	PublicKey  string
	ProjectID  string
}

// ScopedUserPolicy is what ComputeProcess builds from the component config
// + the consumer's dependency outputs before calling Atlas REST.
type ScopedUserPolicy struct {
	// Username is the scoped user to create ("backup-<consumer-stack>").
	Username string
	// Database is the target DB to grant readWrite on (the consumer's own
	// database, read from the consumer's dependency outputs).
	Database string
	// AtlasAdmin is the admin creds to call CreateDatabaseUser with.
	AtlasAdmin AtlasAdminRef
}

// ProvisionScopedMongoUser creates (or re-creates) a scoped Atlas user
// per the policy + returns the generated password so the caller can
// embed it in the tenant record pushed to the backup service.
//
// Idempotency: if Atlas returns 409 (user already exists), we delete +
// retry once. The scoped user is owned by this backup component for this
// consumer — nothing else should be minting the same name.
func ProvisionScopedMongoUser(ctx context.Context, pol ScopedUserPolicy) (password string, err error) {
	if pol.Username == "" || pol.Database == "" {
		return "", errors.Errorf("ProvisionScopedMongoUser: Username and Database are required (got %q, %q)", pol.Username, pol.Database)
	}
	if pol.AtlasAdmin.PrivateKey == "" || pol.AtlasAdmin.PublicKey == "" || pol.AtlasAdmin.ProjectID == "" {
		return "", errors.Errorf("ProvisionScopedMongoUser: AtlasAdmin.{PrivateKey,PublicKey,ProjectID} all required")
	}

	cl := atlas.NewClient(pol.AtlasAdmin.PublicKey, pol.AtlasAdmin.PrivateKey)
	user := atlas.DatabaseUser{
		Username: pol.Username,
		Roles: []atlas.Role{
			{DatabaseName: pol.Database, RoleName: "readWrite"},
		},
	}
	created, err := cl.CreateDatabaseUser(ctx, pol.AtlasAdmin.ProjectID, user)
	if err == nil {
		return created.Password, nil
	}
	// 409 = already exists → delete + retry once for idempotency.
	if !strings.Contains(err.Error(), "HTTP 409") {
		return "", err
	}
	if delErr := cl.DeleteDatabaseUser(ctx, pol.AtlasAdmin.ProjectID, pol.Username, "admin"); delErr != nil {
		return "", errors.Wrapf(err, "409 on create and delete-for-retry also failed: %v", delErr)
	}
	created, err = cl.CreateDatabaseUser(ctx, pol.AtlasAdmin.ProjectID, user)
	if err != nil {
		return "", errors.Wrap(err, "retry-after-409 failed")
	}
	return created.Password, nil
}

// DeleteScopedMongoUser removes a scoped user from Atlas. Idempotent
// (404 is nil). Called from PreDestroy on the consumer's teardown.
func DeleteScopedMongoUser(ctx context.Context, admin AtlasAdminRef, username string) error {
	if admin.PrivateKey == "" || admin.PublicKey == "" || admin.ProjectID == "" {
		return errors.Errorf("DeleteScopedMongoUser: AtlasAdmin.{PrivateKey,PublicKey,ProjectID} all required")
	}
	cl := atlas.NewClient(admin.PublicKey, admin.PrivateKey)
	return cl.DeleteDatabaseUser(ctx, admin.ProjectID, username, "admin")
}

// ScopedUsername returns the deterministic name for a (component, consumer)
// scoped user. Atlas usernames are limited in character set; the consumer
// stack name may contain hyphens/underscores already — which Atlas accepts.
// Cap at 64 chars (Atlas limit).
func ScopedUsername(consumerStack string) string {
	name := "backup-" + consumerStack
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// populateDatabasesFromRaw extracts the `databases:` + `atlasAdmin:` from
// a Component's Config.Raw map. Returns nil + empty list if absent (no
// slice 1d matching to do).
func populateDatabasesFromRaw(raw map[string]any) ([]BackupDatabase, *AtlasAdminRef, error) {
	if raw == nil {
		return nil, nil, nil
	}
	var dbs []BackupDatabase
	if v, ok := raw["databases"]; ok && v != nil {
		rawList, ok := v.([]any)
		if !ok {
			return nil, nil, errors.Errorf("databases: want list, got %T", v)
		}
		for i, item := range rawList {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, nil, errors.Errorf("databases[%d]: want map, got %T", i, item)
			}
			db := BackupDatabase{}
			if s, ok := m["resource"].(string); ok {
				db.Resource = s
			}
			if s, ok := m["driver"].(string); ok {
				db.Driver = s
			}
			if db.Resource == "" || db.Driver == "" {
				return nil, nil, errors.Errorf("databases[%d]: resource + driver required (got resource=%q driver=%q)", i, db.Resource, db.Driver)
			}
			dbs = append(dbs, db)
		}
	}
	var admin *AtlasAdminRef
	if v, ok := raw["atlasAdmin"]; ok && v != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, nil, errors.Errorf("atlasAdmin: want map, got %T", v)
		}
		admin = &AtlasAdminRef{}
		if s, ok := m["privateKey"].(string); ok {
			admin.PrivateKey = s
		}
		if s, ok := m["publicKey"].(string); ok {
			admin.PublicKey = s
		}
		if s, ok := m["projectId"].(string); ok {
			admin.ProjectID = s
		}
	}
	// Validate the conjunction: any mongodb entry needs admin.
	for _, db := range dbs {
		if db.Driver == "mongodb" && admin == nil {
			return nil, nil, errors.Errorf("databases[%q].driver = mongodb but atlasAdmin not set", db.Resource)
		}
	}
	return dbs, admin, nil
}

// MatchConsumerToDatabase looks at the consumer's dependency_outputs_json
// payload (the map SC core assembles from the consumer's `uses:` list) +
// returns the database name + which component Database entry matched.
//
// The depOutputs shape is whatever SC core assembles at ComputeProcess
// time from the consumer's sibling resource outputs. For a consumer with
// `uses: [the-mongo]` on an mongodb-atlas resource named `the-mongo`:
//
//	depOutputs["the-mongo"] = {
//	    "mongo-uri": "mongodb+srv://...",
//	    "database": "<consumer-db>",      // consumer-side override OR default-from-stack
//	    ...
//	}
//
// This matcher is intentionally lenient — the shape is still stabilising
// across plugins (see the index-sc-deploy-secrets memory on canonical
// `connection.*`). We look for any key that looks like a database name.
func MatchConsumerToDatabase(depOutputsJSON []byte, databases []BackupDatabase) (resourceName, databaseName string, matched *BackupDatabase, _ error) {
	if len(depOutputsJSON) == 0 || len(databases) == 0 {
		return "", "", nil, nil
	}
	var deps map[string]any
	if err := json.Unmarshal(depOutputsJSON, &deps); err != nil {
		return "", "", nil, fmt.Errorf("parse dependency_outputs_json: %w", err)
	}
	for i := range databases {
		db := &databases[i]
		raw, ok := deps[db.Resource]
		if !ok {
			continue
		}
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// Look for a database-name key on the sibling's output.
		for _, key := range []string{"database", "dbName", "databaseName"} {
			if v, ok := m[key].(string); ok && v != "" {
				return db.Resource, v, db, nil
			}
		}
	}
	return "", "", nil, nil
}
