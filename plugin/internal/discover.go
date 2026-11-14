// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
)

// Slice 1d.7: auto-discover databases from the consumer's sibling
// compute processors. Operator feedback 2026-11-14:
//
//   "we WANT to have this dynamically defined by every service deployed.
//    Every service must 'register' its database in the backup component
//    dynamically, without any explicit config needed."
//
// The sc-plugin-mongodb ComputeProcessor already emits `MONGO_URI` +
// `MONGO_DATABASE` (+ MONGO_USER + MONGO_PASSWORD) into the consumer's
// env via `collector.AddEnvVariableIfNotExist` / AddSecretEnvVariableIfNotExist.
// When the backup plugin's ComputeProcess fires on the SAME consumer
// deploy, those entries are already in the collector's accumulated
// state — the backup plugin reads them to discover what DBs the
// consumer actually uses.
//
// Replaces the operator-authored `databases: [{resource: ..., driver:
// ..., cluster: ...}]` block from slice 1d.1/1d.3/1d.4. That block is
// now unnecessary surface; operators' server.yaml drops it entirely.

// discoveredDB is one database the consumer deploys into. The URI +
// Database values are the PRE-ARN-substitution ones the collector
// holds at compute-process time (service's ARN resolver only matters
// at Lambda startup; here we have the real string).
type discoveredDB struct {
	driver   string
	uri      string // mongodb+srv://user:pass@host/?...
	database string
	user     string // prior sibling's compute-processor-minted user (not the scoped one)
}

// discoverDatabasesFromCollector walks the consumer's accumulated env
// vars (both plain + secret) and returns one `discoveredDB` per
// database the consumer uses.
//
// Today: only mongodb (`MONGO_URI` + `MONGO_DATABASE` + optional
// `MONGO_USER`). Postgres/MySQL entries land when sc-plugin-aws'
// RDS compute processors emit their own canonical `POSTGRES_URI` /
// `MYSQL_URI` env.
//
// **Ordering caveat**: this only sees env vars emitted by sibling
// compute processors that ran BEFORE the backup plugin's. SC core
// processes `uses:` in the operator's declaration order, so
// `uses: [the-mongo, my-backup]` works and `uses: [my-backup,
// the-mongo]` discovers nothing. Documented in the backup
// component's README; will go away if SC core starts sorting
// Component-type processors last (api follow-up).
func discoverDatabasesFromCollector(collector pApi.ComputeContextCollector) []discoveredDB {
	// Build a flat name→value map from both plain + secret env lists.
	// The collector's own AddEnv* methods enforce no-overwrite, so
	// each name has at most one value here.
	byName := make(map[string]string, 16)
	for _, e := range collector.EnvVariables() {
		byName[e.Name] = e.Value
	}
	for _, e := range collector.SecretEnvVariables() {
		byName[e.Name] = e.Value
	}

	var out []discoveredDB
	if uri := byName["MONGO_URI"]; uri != "" {
		out = append(out, discoveredDB{
			driver:   "mongodb",
			uri:      uri,
			database: byName["MONGO_DATABASE"],
			user:     byName["MONGO_USER"],
		})
	}
	// Follow-ups when the fleet has them:
	//   if uri := byName["POSTGRES_URI"]; uri != "" { ... }
	//   if uri := byName["MYSQL_URI"]; uri != "" { ... }
	return out
}
