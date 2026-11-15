// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"context"
	"encoding/json"

	"github.com/pkg/errors"
	"github.com/simple-container-com/api/pkg/api"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
	scPlugin "github.com/simple-container-com/api/pkg/plugin"
)

// Scoped-user provisioning via cross-plugin peer-invoke
// (sc-components-plugin-peer-invoke, api v2026.11.63+). This plugin
// stays driver-agnostic — it never calls Atlas REST (or any other
// DB admin API) directly. The database-owning plugin advertises
// capabilities it then handles with ITS OWN credentials:
//
//   - sc-plugin-mongodb: mint-scoped-user, delete-scoped-user.
//     Requires sc-plugin-mongodb >= v2026.11.15-5705452.
//   - future sc-plugin-postgres: same capability names, different
//     plugin owner. Same contract.
//
// Driver → plugin name convention: the driver string the mongodb
// plugin stamps on MONGO_URI (via collector.AddSecretEnvVariableIfNotExist)
// matches the plugin's SC name. "mongodb" → the mongodb plugin.
// Registered in `driverPlugins` below; keep in sync with each
// database plugin's advertised SchemaResponse.plugin_name.

// driverPlugins maps a discovered driver name to the SC plugin name
// that owns scoped-user minting for it. Centralised here so new
// database plugins land as a one-line addition — not scattered
// capability-call sites.
var driverPlugins = map[string]string{
	"mongodb": "mongodb",
	// Future:
	// "postgres": "postgres",
	// "mysql":    "mysql",
}

// PluginForDriver returns the SC plugin name that owns scoped-user
// minting for the given driver string. Returns ok=false for an
// unmapped driver — caller should surface a clear error naming the
// unmapped driver.
func PluginForDriver(driver string) (string, bool) {
	p, ok := driverPlugins[driver]
	return p, ok
}

// MintScopedUserResult is the deserialised reply from a successful
// peer-invoke mint call. Shape matches every database plugin's
// `mint-scoped-user` capability contract (currently only mongodb
// implements; future postgres/mysql follow the same shape).
type MintScopedUserResult struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// URI carries the scoped user's connection URI — the database
	// plugin weaves the fresh creds into the cluster's host string.
	URI string `json:"uri"`
}

// MintScopedUser asks the driver's owning plugin to create a scoped
// readWrite user. Returns the minted username/password/URI the caller
// embeds in the tenant.Record.Resources entry.
//
// The call fans out via SC core's broker — the owning plugin reads
// its OWN admin credential (operator-configured on the mongodb/
// postgres/mysql resource) and never crosses it back onto the wire.
// This is the whole architectural point: backup is generic, each
// database plugin keeps ownership of its own creds.
func MintScopedUser(ctx context.Context, peer *scPlugin.PeerClient, driver, cluster, database, consumerStack string) (*MintScopedUserResult, error) {
	pluginName, ok := PluginForDriver(driver)
	if !ok {
		return nil, errors.Errorf("MintScopedUser: no plugin wired for driver %q (expected one of %v) — backup plugin needs the driver → plugin map entry", driver, knownDrivers())
	}
	if cluster == "" || database == "" || consumerStack == "" {
		return nil, errors.Errorf("MintScopedUser: cluster/database/consumerStack all required (got %q, %q, %q)", cluster, database, consumerStack)
	}
	payload, err := json.Marshal(map[string]any{
		"cluster":       cluster,
		"database":      database,
		"roleName":      "readWrite",
		"consumerStack": consumerStack,
	})
	if err != nil {
		return nil, errors.Wrap(err, "MintScopedUser: marshal payload")
	}
	reply, err := peer.PluginInvoke(ctx, pluginName, "mint-scoped-user", payload)
	if err != nil {
		return nil, errors.Wrapf(err, "MintScopedUser: peer-invoke %s/mint-scoped-user (cluster=%q, database=%q)", pluginName, cluster, database)
	}
	var result MintScopedUserResult
	if err := json.Unmarshal(reply, &result); err != nil {
		return nil, errors.Wrap(err, "MintScopedUser: decode reply")
	}
	if result.Username == "" || result.Password == "" {
		return nil, errors.New("MintScopedUser: reply missing username or password")
	}
	return &result, nil
}

// DeleteScopedUser asks the driver's owning plugin to remove a
// previously-minted scoped user. Idempotent — a missing user is a
// no-op (the owning plugin's handler swallows 404).
//
// Called from PreDestroy. On parent-stack destroy the owning plugin's
// credential cache may be empty (its ComputeProcess doesn't fire on
// destroy) — the call will fail with a cache-miss error from the
// owning plugin. PreDestroy swallows all errors; orphan users on
// isolated-backup-destroy are a documented limitation until
// mongodb-exports-creds-for-predestroy-peer-invoke follow-up lands.
func DeleteScopedUser(ctx context.Context, peer *scPlugin.PeerClient, driver, cluster, username string) error {
	pluginName, ok := PluginForDriver(driver)
	if !ok {
		return errors.Errorf("DeleteScopedUser: no plugin wired for driver %q", driver)
	}
	if cluster == "" || username == "" {
		return errors.Errorf("DeleteScopedUser: cluster/username required (got %q, %q)", cluster, username)
	}
	payload, err := json.Marshal(map[string]any{
		"cluster":  cluster,
		"username": username,
	})
	if err != nil {
		return errors.Wrap(err, "DeleteScopedUser: marshal payload")
	}
	if _, err := peer.PluginInvoke(ctx, pluginName, "delete-scoped-user", payload); err != nil {
		return errors.Wrapf(err, "DeleteScopedUser: peer-invoke %s/delete-scoped-user (cluster=%q, username=%q)", pluginName, cluster, username)
	}
	return nil
}

// pluginOutputOf returns the *api.PluginResourceOutput for a dep name,
// handling the (name missing) + (Ref wrong type) + (nil Ref) cases.
// Used by compute.go's readServiceURL to read sibling-published
// outputs from the consumer's ResourceOutputs map.
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

// stringOutputValue reads one published output by exact key and
// returns it as a string. Missing or non-string values return "" +
// false.
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

// knownDrivers returns a deterministic sorted list of driver strings
// the backup plugin currently routes. Used in error messages so the
// operator / plugin author sees the exact set.
func knownDrivers() []string {
	keys := make([]string, 0, len(driverPlugins))
	for k := range driverPlugins {
		keys = append(keys, k)
	}
	// Deterministic for readable error text; not performance-critical.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}
