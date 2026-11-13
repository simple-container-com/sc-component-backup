// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Command backup is the microservice half of sc-component-backup — the
// service that gets dispatched into the operator's chosen template
// (lambda / compose / k8s) and receives tenant registrations from any
// consumer that `uses: [my-backup]`.
//
//   - Reads env: SERVICE_JWT_SECRET (HS256, 32+ bytes, operator-set via
//     the plugin's config.signingKey), MONGO_URI, PORT (default 8080).
//   - HTTP router:
//     GET  /healthz                         — unauthenticated, 200
//     POST /api/v1/tenants/register         — JWT-auth, upsert row
//     GET  /api/v1/tenants                  — JWT-auth, list for SPA
//     GET  /*                               — embedded SPA
//   - Graceful shutdown on SIGTERM.
//
// Auth model: the operator sets config.signingKey in server.yaml;
// Path A's dispatch injects it as SERVICE_JWT_SECRET env on this
// container. Same key signs the plugin's per-consumer JWTs at
// ComputeProcess time. This is sc-component-backup's own key — not an
// alias of any larger fleet's shared secret.
package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	sdkservice "github.com/simple-container-com/go-aws-lambda-sdk/pkg/service"

	"github.com/simple-container-com/sc-component-backup/service/internal/api"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/storage"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
	"github.com/simple-container-com/sc-component-backup/service/internal/ui"
)

// Set at build time via ldflags: -X main.version=... etc.
var (
	version  = "dev"
	commit   = "dev"
	revision = "dev"
)

func main() {
	// One-shot worker mode — design doc §5.3. When the Lambda's
	// scheduler tick calls ecs.RunTask, it sets BACKUP_MODE=worker +
	// the per-tenant env below. The container runs dump → S3 → exits.
	// Otherwise (default) runs the long-lived HTTP service + Lambda
	// bridge below.
	if mode := strings.ToLower(os.Getenv("BACKUP_MODE")); mode == "worker" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		os.Exit(runWorker(ctx))
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	jwtSecret := mustGetEnv("SERVICE_JWT_SECRET")
	if len(jwtSecret) < 32 {
		slog.Error("SERVICE_JWT_SECRET must be at least 32 bytes (HS256 floor)")
		os.Exit(2)
	}
	mongoURI := mustGetEnv("MONGO_URI")
	port := envOr("PORT", "8080")

	slog.Info("starting sc-component-backup",
		"version", version, "commit", commit, "revision", revision,
		"port", port, "mongo_uri_masked", maskMongoURI(mongoURI))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	store, err := tenant.NewMongoStore(ctx, mongoURI)
	if err != nil {
		slog.Error("mongo connect failed", "err", err)
		os.Exit(1)
	}
	defer store.Close(context.Background())

	// S3 store. Endpoint comes from env (MinIO in smoke; empty → AWS).
	s3Store, err := storage.NewS3Store(ctx, storage.S3Config{
		Bucket:       envOr("S3_BUCKET", "sc-backup-smoke"),
		Region:       envOr("S3_REGION", "us-east-1"),
		EndpointURL:  envOr("S3_ENDPOINT_URL", ""),
		AccessKey:    envOr("S3_ACCESS_KEY", ""),
		SecretKey:    envOr("S3_SECRET_KEY", ""),
		UsePathStyle: envOr("S3_USE_PATH_STYLE", "") != "",
	})
	if err != nil {
		slog.Error("s3 store init failed", "err", err)
		os.Exit(1)
	}
	if envOr("S3_ENSURE_BUCKET", "") != "" {
		if err := s3Store.EnsureBucket(ctx); err != nil {
			slog.Error("s3 ensure-bucket failed", "err", err)
			os.Exit(1)
		}
	}

	// In-memory run store for the smoke (no Mongo collection for runs
	// in v1; rebuild from state on restart is a follow-up).
	runs := scheduler.NewInMemoryRunStore()
	sch := scheduler.New(scheduler.Config{}, store, runs, s3Store, logger, nil)

	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(middleware.RealIP)
	router.Use(middleware.Recoverer)
	router.Use(requestLogger)

	// Slice 1e.2 — SPA-facing handlers.
	sessionCfg := api.NewSessionConfigFromEnv([]byte(jwtSecret))
	metaCfg := &api.MetaConfig{Version: version, Commit: commit}
	overviewCfg := &api.OverviewConfig{
		Tenants:         store,
		Runs:            runs,
		StorageLocation: fmt.Sprintf("s3://%s/", envOr("S3_BUCKET", "sc-backup-smoke")),
	}
	projectsCfg := &api.ProjectsConfig{Tenants: store, Runs: runs}

	// Unauthenticated endpoints — healthz + the session probe + login/logout.
	router.Get("/healthz", api.Healthz)
	router.Get("/api/v1/session", sessionCfg.Session)
	router.Post("/api/v1/login", sessionCfg.Login)
	router.Post("/api/v1/logout", sessionCfg.Logout)

	// JWT-authenticated endpoints.
	router.Group(func(r chi.Router) {
		r.Use(api.JWTMiddleware([]byte(jwtSecret)))

		// Core tenant / run surface (predates the SPA; plugin-auth path
		// stays intact — register uses op=register claim).
		r.Post("/api/v1/tenants/register", api.TenantsRegister(store))
		r.Get("/api/v1/tenants", api.TenantsList(store))
		r.Get("/api/v1/backups", api.NewBackupsListHandler(runs).ServeHTTP)
		r.Post("/api/v1/tenants/{stack}/backup-now", api.NewBackupNowHandler(sch, store).ServeHTTP)
		r.Post("/api/v1/tenants/{stack}/restore", api.NewRestoreHandler(sch, runs).ServeHTTP)
		r.Get("/api/v1/restores/{id}", api.NewRestoreStatusHandler(runs).ServeHTTP)

		// Scheduler tick — EventBridge fires this every minute when
		// deployTo: lambda (design doc §5.3). Walks tenants, fires ECS
		// Fargate one-shot tasks per due tenant. ?force=true on an
		// operator-driven call runs every tenant regardless of
		// schedule. Nil Ecs here → handler builds its own client from
		// the AWS SDK default config chain at first-tick time.
		r.Post("/api/v1/scheduler/tick", api.NewSchedulerTickHandler(api.SchedulerTickConfig{Tenants: store}).ServeHTTP)

		// SPA-facing read-only surface (slice 1e.2 2026-11-13). Mutating
		// project CRUD + settings + stats endpoints stay TODO (slice 1e.3).
		r.Get("/api/v1/meta", metaCfg.Meta)
		r.Get("/api/v1/overview", overviewCfg.Overview)
		r.Get("/api/v1/projects/{name}", projectsCfg.Project)
		r.Get("/api/v1/projects/{name}/runs", projectsCfg.RunsForProject)
	})

	// Embedded SPA.
	spaFS, err := fs.Sub(ui.BuildFS, "dist")
	if err != nil {
		slog.Warn("SPA dist not embedded (build not run?) — serving fallback", "err", err)
	} else {
		router.Handle("/*", http.FileServer(http.FS(spaFS)))
	}

	// Bridge via go-aws-lambda-sdk's sdkservice. Under Lambda
	// (AWS_LAMBDA_FUNCTION_NAME env set), it joins the Lambda runtime API
	// and routes events through its-felix/aws-lambda-go-url-adapter
	// (the vanilla handler path). Outside Lambda — LOCAL_DEBUG=true or
	// just missing the env — it falls back to net/http on :PORT.
	slog.Info("starting service (sdkservice bridge)", "port", port)
	svc, err := sdkservice.New(ctx, sdkservice.WithVanillaHandler(router))
	if err != nil {
		slog.Error("sdkservice init failed", "err", fmt.Sprintf("%v", err))
		os.Exit(1)
	}
	if err := svc.Start(); err != nil {
		slog.Error("sdkservice exited", "err", fmt.Sprintf("%v", err))
		os.Exit(1)
	}
	slog.Info("exited cleanly")
}

func mustGetEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		slog.Error("required env var missing", "name", name)
		os.Exit(2)
	}
	return v
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// maskMongoURI redacts the password from a mongodb://user:password@host form
// so a startup log can show the connection target without the credential.
func maskMongoURI(uri string) string {
	if idx := indexAt(uri); idx > 0 {
		// Walk back from `@` to the `:` that starts the password.
		for i := idx - 1; i >= 0; i-- {
			if uri[i] == ':' {
				return uri[:i+1] + "***" + uri[idx:]
			}
		}
	}
	return uri
}

func indexAt(s string) int {
	for i, c := range s {
		if c == '@' {
			return i
		}
	}
	return -1
}

// requestLogger is a minimal structured-log middleware. Not production-grade
// (no latency bucketing, no sampling) — fits the backup's scale.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.Info(
			"http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"latency_ms", fmt.Sprintf("%.2f", float64(time.Since(start).Microseconds())/1000.0),
		)
	})
}
