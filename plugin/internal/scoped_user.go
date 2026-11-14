// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"context"
	"strings"

	"github.com/pkg/errors"
	"github.com/simple-container-com/api/pkg/api"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"

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
//
// `Cluster` is an operator-provided hint matching the Atlas cluster
// name the sibling sc-plugin-mongodb resource publishes its exports
// under (e.g. "backup-smoke-1d-20261114"). It defaults to `Resource`
// when unset — sufficient for stacks that let sc-plugin-mongodb
// auto-name. Operators who set `clusterName:` on the Mongo resource
// (typical for `adopt: true`) must set this to the override value so
// the plugin can read `<cluster>-database` / `<cluster>-mongo-uri-options`
// from the parent stack's exports. Added 2026-11-14 when the live 1d
// smoke found params.ResourceOutputs is empty on consumer-side
// ComputeProcess and the plugin has to fall back to
// `sdk.NewStackReference` (the sc-plugin-mongodb precedent).
type BackupDatabase struct {
	Resource string
	Driver   string
	Cluster  string
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

// PopulateDatabasesFromRaw is the exported wrapper over populateDatabasesFromRaw.
// Used by the plugin's main.go Provision to extract atlasAdmin at parent
// provision time and export it to stack state for PreDestroy's scoped-user
// cleanup pass.
func PopulateDatabasesFromRaw(raw map[string]any) ([]BackupDatabase, *AtlasAdminRef, error) {
	return populateDatabasesFromRaw(raw)
}

// PopulateAtlasAdminFromRaw is the exported wrapper over
// populateAtlasAdminFromRaw. Called by main.go Provision to export
// atlasAdmin to parent state for PreDestroy's scoped-user cleanup pass.
func PopulateAtlasAdminFromRaw(raw map[string]any) (*AtlasAdminRef, error) {
	return populateAtlasAdminFromRaw(raw)
}

// populateAtlasAdminFromRaw extracts just the `atlasAdmin:` block from a
// Component's Config.Raw map. Returns nil + no error when atlasAdmin is
// absent (operator opted out of scoped-user minting).
//
// Slice 1d.7 splits atlasAdmin extraction out from the legacy
// `databases:` extraction: discovery is now from collector env vars,
// not from an operator-authored databases list. The `databases:` block
// is deprecated but tolerated for one release (populateDatabasesFromRaw
// stays available and PopulateDatabasesFromRaw's return is still called
// by main.go's Provision for the atlasAdmin half; the dbs slice is
// ignored).
func populateAtlasAdminFromRaw(raw map[string]any) (*AtlasAdminRef, error) {
	if raw == nil {
		return nil, nil
	}
	v, ok := raw["atlasAdmin"]
	if !ok || v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.Errorf("atlasAdmin: want map, got %T", v)
	}
	admin := &AtlasAdminRef{}
	if s, ok := m["privateKey"].(string); ok {
		admin.PrivateKey = s
	}
	if s, ok := m["publicKey"].(string); ok {
		admin.PublicKey = s
	}
	if s, ok := m["projectId"].(string); ok {
		admin.ProjectID = s
	}
	return admin, nil
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
			if s, ok := m["cluster"].(string); ok {
				db.Cluster = s
			}
			if db.Resource == "" || db.Driver == "" {
				return nil, nil, errors.Errorf("databases[%d]: resource + driver required (got resource=%q driver=%q)", i, db.Resource, db.Driver)
			}
			if db.Cluster == "" {
				db.Cluster = db.Resource
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

// pluginOutputOf returns the *api.PluginResourceOutput for a dep name,
// handling the (name missing) + (Ref wrong type) + (nil Ref) cases.
func pluginOutputOf(deps pApi.ResourcesOutputs, name string) *api.PluginResourceOutput {
	ref, ok := deps[name]
	if !ok || ref == nil {
		return nil
	}
	pro, ok := ref.Ref.(*api.PluginResourceOutput)
	if !ok || pro == nil {
		return nil
	}
	return pro
}

// stringOutputValue reads one published output by exact key and returns
// it as a string. Missing or non-string values return "" + false.
func stringOutputValue(pro *api.PluginResourceOutput, key string) (string, bool) {
	v, found := pro.Lookup(key)
	if !found {
		return "", false
	}
	s, ok := v.Value.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}
