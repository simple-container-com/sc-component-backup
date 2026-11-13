// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/driver"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
)

// POST /api/v1/test-connection — the SPA's NewProject form hits this
// before the operator commits the project config, so a bad URI surfaces
// as a form-field error, not as a run-time failure after the project
// is created.
//
// Request shape mirrors the SPA's TypeScript `TestConnectionBody`
// (service/internal/ui/src/api/client.ts:202):
//
//	{ engine: "mongodb"|"postgres"|"mariadb", uri: string }
//	OR
//	{ project: "<name>" }   // use the stored project.SecretURI
//
// Response: `{ok, server_version?, client_version?, databases?,
// warnings?, error?}` — the SPA's `TestConnectionResult` shape.
//
// Lives in its own file so the Driver + project.Store dependencies are
// scoped tight; projects_handler.go stays about CRUD.

// TestConnectionConfig carries the handler's deps.
type TestConnectionConfig struct {
	Projects      project.Store
	DriverFactory func(name string) (driver.Driver, error) // defaults to driver.ByName when nil
	// Timeout caps a test — overrides the driver's internal timeout
	// for the SPA's responsiveness (operators type + submit; a stuck
	// test would hold the request indefinitely otherwise).
	Timeout time.Duration
}

type testConnectionBody struct {
	Engine  string `json:"engine,omitempty"`
	URI     string `json:"uri,omitempty"`
	Project string `json:"project,omitempty"`
}

type testConnectionResp struct {
	OK            bool     `json:"ok"`
	ServerVersion string   `json:"server_version,omitempty"`
	ClientVersion string   `json:"client_version,omitempty"`
	Databases     []string `json:"databases,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// TestConnection handles POST /api/v1/test-connection.
func (c *TestConnectionConfig) TestConnection(w http.ResponseWriter, r *http.Request) {
	var body testConnectionBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed body: " + err.Error()})
		return
	}
	engine, uri, err := c.resolveInput(r.Context(), body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	drv, err := c.driverFor(engine)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "field": "engine"})
		return
	}
	ctx := r.Context()
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	res, err := drv.Test(ctx, driver.ConnectionSpec{URI: uri})
	if err != nil {
		writeJSON(w, http.StatusOK, testConnectionResp{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, testConnectionResp{
		OK:            res.OK,
		ServerVersion: res.ServerVersion,
		ClientVersion: res.ClientVersion,
		Databases:     res.Databases,
		Warnings:      res.Warnings,
		Error:         res.Error,
	})
}

// resolveInput extracts (engine, uri) from the body — either both
// fields on the engine/uri path, or looking up the stored config +
// secret URI on the project path.
func (c *TestConnectionConfig) resolveInput(ctx context.Context, body testConnectionBody) (string, string, error) {
	if body.Project != "" {
		if c.Projects == nil {
			return "", "", errors.New("project.Store not wired — pass engine+uri explicitly")
		}
		cfg, ok, err := c.Projects.Get(ctx, body.Project)
		if err != nil {
			return "", "", fmt.Errorf("project lookup: %w", err)
		}
		if !ok {
			return "", "", fmt.Errorf("project %q not found", body.Project)
		}
		if cfg.SecretURI == "" {
			return "", "", fmt.Errorf("project %q has no stored secret URI — set it first via PUT /secret or pass uri explicitly", body.Project)
		}
		return cfg.Engine, cfg.SecretURI, nil
	}
	if body.Engine == "" || body.URI == "" {
		return "", "", errors.New("engine and uri are both required (or pass project)")
	}
	return body.Engine, body.URI, nil
}

func (c *TestConnectionConfig) driverFor(name string) (driver.Driver, error) {
	if c.DriverFactory != nil {
		return c.DriverFactory(name)
	}
	return driver.ByName(name)
}
