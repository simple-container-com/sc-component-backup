// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"net/http"
)

// MetaResponse mirrors the SPA's Meta type
// (service/internal/ui/src/api/types.ts:19). Returned by the meta
// handler so the SPA can render the top-bar version + populate the
// engine picker on the NewProject page.
type MetaResponse struct {
	Version string          `json:"version"`
	Build   *MetaBuild      `json:"build,omitempty"`
	Engines []MetaEngineRow `json:"engines"`
}

type MetaBuild struct {
	Commit     string  `json:"commit"`
	DeployedAt *string `json:"deployed_at"`
}

type MetaEngineRow struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	URIExample      string   `json:"uri_example"`
	DumpArgs        []string `json:"dump_args"`
	SupportsGlobals bool     `json:"supports_globals"`
}

// metaEngines is the canonical engine catalog surfaced to the SPA.
// Matches the Driver names + what each engine's reference port
// (sc-component-backup fb53643 / c50309b) actually supports.
var metaEngines = []MetaEngineRow{
	{
		ID:              "mongodb",
		Label:           "MongoDB",
		URIExample:      "mongodb+srv://user:pass@cluster.example.net/?readPreference=secondary",
		DumpArgs:        []string{},
		SupportsGlobals: false,
	},
	{
		ID:              "postgres",
		Label:           "PostgreSQL",
		URIExample:      "postgres://user:pass@host:5432/dbname?sslmode=require",
		DumpArgs:        []string{"--no-owner", "--no-acl"},
		SupportsGlobals: true,
	},
	{
		ID:              "mariadb",
		Label:           "MySQL / MariaDB",
		URIExample:      "mysql://user:pass@host:3306/dbname",
		DumpArgs:        []string{"--single-transaction", "--quick"},
		SupportsGlobals: false,
	},
}

// MetaConfig carries the deploy-time values a Meta response needs.
// Populated from cmd/backup/main.go's build-ldflags constants.
type MetaConfig struct {
	Version    string
	Commit     string
	DeployedAt string // RFC-3339; empty if unset
}

// Meta returns GET /api/v1/meta — the static-ish metadata the SPA
// shows in its top-bar + NewProject engine picker. Authenticated route.
func (c *MetaConfig) Meta(w http.ResponseWriter, _ *http.Request) {
	resp := MetaResponse{
		Version: c.Version,
		Engines: metaEngines,
	}
	if c.Commit != "" || c.DeployedAt != "" {
		b := &MetaBuild{Commit: c.Commit}
		if c.DeployedAt != "" {
			d := c.DeployedAt
			b.DeployedAt = &d
		}
		resp.Build = b
	}
	writeJSON(w, http.StatusOK, resp)
}
