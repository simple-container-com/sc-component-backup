// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Command backup is the microservice half of sc-component-backup — the
// service that gets dispatched into the operator's chosen template
// (lambda / compose / k8s) and receives tenant registrations from any
// consumer that `uses: [my-backup]`.
//
//   - Reads env: SERVICE_JWT_SECRET (HS256, 32+ bytes, operator-set via
//     the plugin's config.signingKey), BACKUP_OPERATOR_PASSWORD (SPA login,
//     operator-set via config.operatorPassword), ADMIN_MIGRATION_TOKEN
//     (bearer for the one-shot /admin/migrate-mongo-to-goembedb endpoint,
//     operator-set via config.migrationToken; empty = route not mounted),
//     STORAGE_* / METADATA_* (archive + metadata backend selection — see
//     config.go), PORT (default 8080).
//     All three operator-set secrets may arrive as `arn:aws:secretsmanager:*`
//     ARN strings — the plugin binds `${secret:…}` config values onto Lambda
//     env as per-function SecretsManager entries, and the Lambda runtime
//     does not dereference them. Resolution happens once at startup via
//     resolveAWSSecretARN; failure exits 2.
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

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	sdkservice "github.com/simple-container-com/go-aws-lambda-sdk/pkg/service"

	"github.com/simple-container-com/sc-component-backup/service/internal/api"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/alerts"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/meta"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/notify"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/goembedbadmin"
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
	// Resolve the ARN when SC core's `${secret:…}` wiring left an
	// `arn:aws:secretsmanager:…` string in the env. See the resolver's
	// doc comment for the full why; summary: the plugin mints JWTs with
	// the resolved signingKey, so this service MUST validate with the
	// resolved value too or every push-register is 401.
	{
		resolveCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		resolved, err := resolveAWSSecretARN(resolveCtx, "SERVICE_JWT_SECRET", jwtSecret)
		cancel()
		if err != nil {
			slog.Error("failed to resolve SERVICE_JWT_SECRET from SecretsManager", "err", err)
			os.Exit(2)
		}
		jwtSecret = resolved
	}
	if len(jwtSecret) < 32 {
		slog.Error("SERVICE_JWT_SECRET must be at least 32 bytes (HS256 floor)")
		os.Exit(2)
	}
	// Resolve BACKUP_OPERATOR_PASSWORD on the same ARN pattern. The SPA
	// login handler reads the operator password via `os.Getenv` out of
	// NewSessionConfigFromEnv — pushing the resolved value BACK into
	// the env keeps that reader untouched and keeps the responsibility
	// for resolution in one place (here, at startup). Empty env is
	// allowed at this layer; the login handler surfaces its own
	// "BACKUP_OPERATOR_PASSWORD not set on the server" 403 when a
	// login attempt lands on an empty value.
	//
	// Live-caught 2026-11-18 on the first fleet-roll of
	// backup.simple-forge.com: service compared against the ARN string
	// and every operator login failed with `wrong password` (the ARN
	// string is 115 chars; no human-typeable password matches).
	if pw := os.Getenv("BACKUP_OPERATOR_PASSWORD"); pw != "" {
		resolveCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		resolved, err := resolveAWSSecretARN(resolveCtx, "BACKUP_OPERATOR_PASSWORD", pw)
		cancel()
		if err != nil {
			slog.Error("failed to resolve BACKUP_OPERATOR_PASSWORD from SecretsManager", "err", err)
			os.Exit(2)
		}
		if err := os.Setenv("BACKUP_OPERATOR_PASSWORD", resolved); err != nil {
			slog.Error("failed to set resolved BACKUP_OPERATOR_PASSWORD env", "err", err)
			os.Exit(2)
		}
	}
	// Resolve ADMIN_MIGRATION_TOKEN on the same ARN pattern. The plugin's
	// ComponentSecretEnvMap binds `config.migrationToken` as a per-function
	// SecretsManager ARN (plaintext never lands in Lambda env); the gate at
	// the migration route compares the operator's bearer against this env
	// value — without resolution it compares against the ARN string and no
	// human-typeable token matches. Mirrors the SERVICE_JWT_SECRET +
	// BACKUP_OPERATOR_PASSWORD resolution above. Pre-resolve-and-write
	// keeps the gate's os.Getenv reader untouched.
	if t := os.Getenv("ADMIN_MIGRATION_TOKEN"); t != "" {
		resolveCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		resolved, err := resolveAWSSecretARN(resolveCtx, "ADMIN_MIGRATION_TOKEN", t)
		cancel()
		if err != nil {
			slog.Error("failed to resolve ADMIN_MIGRATION_TOKEN from SecretsManager", "err", err)
			os.Exit(2)
		}
		if err := os.Setenv("ADMIN_MIGRATION_TOKEN", resolved); err != nil {
			slog.Error("failed to set resolved ADMIN_MIGRATION_TOKEN env", "err", err)
			os.Exit(2)
		}
	}
	// Resolve NOTIFY_TOKEN on the same ARN pattern. The plugin's
	// ComponentSecretEnvMap binds `config.notifyToken` as a per-function
	// SecretsManager ARN; the notify publisher POSTs to the notifier with
	// this value as its Bearer. Without resolution the service would
	// present the SecretsManager ARN string instead of the token, and the
	// notifier would 401. Same shape as SERVICE_JWT_SECRET +
	// BACKUP_OPERATOR_PASSWORD + ADMIN_MIGRATION_TOKEN above.
	//
	// Caught 2026-11-24 by TestSecretEnvVarsAreResolvedByTheService — the
	// Schema invariant test added alongside this fix. Before the test
	// landed, the notify wiring (PR #33) had bound notifyToken as a
	// secret env var but no service-side resolver; a Lambda deploy with
	// `config.notifyToken: ${secret:…}` would have sent an ARN string as
	// its Bearer.
	if t := os.Getenv("NOTIFY_TOKEN"); t != "" {
		resolveCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		resolved, err := resolveAWSSecretARN(resolveCtx, "NOTIFY_TOKEN", t)
		cancel()
		if err != nil {
			slog.Error("failed to resolve NOTIFY_TOKEN from SecretsManager", "err", err)
			os.Exit(2)
		}
		if err := os.Setenv("NOTIFY_TOKEN", resolved); err != nil {
			slog.Error("failed to set resolved NOTIFY_TOKEN env", "err", err)
			os.Exit(2)
		}
	}
	port := envOr("PORT", "8080")

	slog.Info("starting sc-component-backup",
		"version", version, "commit", commit, "revision", revision, "port", port)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Slice 1e.4g — archive.Store from env (STORAGE_*).
	//
	// BLOB CARVE-OUT: the archive store carries the dump artefacts
	// (`.tar.gz`, `.sql.gz`, `.gz`) produced by the driver workers.
	// It stays on S3 / GCS / filesystem regardless of USE_GOEMBEDB.
	// Only metadata rows move through the goembedb path below.
	archiveStore, archiveLocation, err := buildArchiveStore(ctx)
	if err != nil {
		slog.Error("archive store init failed", "err", err)
		os.Exit(1)
	}

	// go-embedb adoption (design: forge docs/design/2026-11-22/
	// go-embedb-active-active-s3/). When USE_GOEMBEDB=1 the metadata
	// path is diverted onto an embedded ferretdb listener — the
	// existing METADATA_TYPE=mongo wiring (five domain stores, four
	// `mongo.Connect` calls) reuses the URI the embed advertises,
	// with `?directConnection=true` appended so each client skips
	// SDAM against the single-node replica set the embed exposes.
	//
	// migrationSourceDB captures the pre-cutover external Mongo for
	// the `/admin/migrate-mongo-to-goembedb` endpoint's source. nil
	// when either USE_GOEMBEDB is off or when the operator already
	// unset METADATA_URI post-cutover.
	useGoEmbedb := envOr("USE_GOEMBEDB", "") == "1"
	var migrationSourceDB *mongo.Database
	var migrationSinkDB *mongo.Database
	if useGoEmbedb {
		embed, berr := bootGoEmbed(slog.Default())
		if berr != nil {
			slog.Error("goembedb boot failed", "err", berr)
			os.Exit(1)
		}
		// Capture the external Mongo as the migration source when
		// configured. Non-fatal: a stack already running on goembedb
		// (post-cutover) may have had METADATA_URI unset by that point.
		if externalURI := os.Getenv("METADATA_URI"); externalURI != "" {
			srcCtx, srcCancel := context.WithTimeout(ctx, 10*time.Second)
			src, cerr := mongo.Connect(srcCtx, options.Client().ApplyURI(externalURI))
			if cerr != nil {
				slog.Warn("external METADATA_URI connect failed — migration source disabled", "err", cerr)
			} else if perr := src.Ping(srcCtx, nil); perr != nil {
				slog.Warn("external METADATA_URI ping failed — migration source disabled", "err", perr)
			} else {
				migrationSourceDB = src.Database(goembedbadmin.Database)
				slog.Info("external Mongo captured as migration source", "db", goembedbadmin.Database)
			}
			srcCancel()
		}
		// Rewrite the metadata env so buildMetaStore reaches the embed
		// through the standard mongo path. Direct-connection is forced
		// in the URI so each of the four backend `mongo.Connect` calls
		// pins to the loopback socket (see metaURIForGoEmbed doc).
		if err := os.Setenv("METADATA_TYPE", "mongo"); err != nil {
			slog.Error("set METADATA_TYPE for goembedb failed", "err", err)
			os.Exit(1)
		}
		if err := os.Setenv("METADATA_URI", metaURIForGoEmbed(embed.URI)); err != nil {
			slog.Error("set METADATA_URI for goembedb failed", "err", err)
			os.Exit(1)
		}
		migrationSinkDB = embed.Client.Database(goembedbadmin.Database)
		slog.Info("metadata backend: embedded go-embedb", "replSet", embed.ReplSet)
	}

	// Slice 1e.4b/4g — meta.Store umbrella (METADATA_*).
	metaStore, err := buildMetaStore(ctx, archiveStore)
	if err != nil {
		slog.Error("meta store init failed", "err", err)
		os.Exit(1)
	}
	defer metaStore.Close(context.Background())

	// Advertise the active metadata backend to Prometheus. Label is
	// "goembedb" when the embed is live, otherwise the operator's
	// chosen METADATA_TYPE (empty → the meta package's default — see
	// buildMetaStore for the resolution shape).
	goembedbadmin.SetStoreBackend(backendLabel(useGoEmbedb))

	sch := scheduler.New(scheduler.Config{}, metaStore.Tenants(), metaStore.Runs(), archiveStore, logger, nil)

	// Publisher for sc-component-notifier. NOTIFY_URL/NOTIFY_TOKEN are
	// landed by this component's plugin from the operator's `notify:`
	// block; both absent means notifications are off and NewEmitter
	// returns a no-op.
	//
	// A construction error is FATAL on purpose. It only happens when
	// the wiring is half-done — a URL with no token (every event would
	// 401), or a URL still holding an unresolved `$sc:…` placeholder —
	// and both would otherwise surface for the first time at the moment
	// an operator was relying on being paged. A deploy that fails here
	// is strictly better than a service that runs with its alerting
	// silently dead.
	eventPublisher, err := notify.NewEmitter(notify.EmitterConfig{
		BaseURL: os.Getenv("NOTIFY_URL"),
		Token:   os.Getenv("NOTIFY_TOKEN"),
		SelfURL: os.Getenv("BACKUP_AGENT_URL"),
		Log:     logger,
	})
	if err != nil {
		slog.Error("notify publisher init failed", "err", err)
		os.Exit(1)
	}
	sch.SetPublisher(eventPublisher)

	// Reconcile stuck "running" rows at startup. A Lambda that OOM'd
	// mid-run (or was replaced for any other reason) leaves the row
	// stuck forever because the in-process UpdateBackup call never
	// fires — the SPA then shows a permanent spinner. Cutoff is 2 ×
	// BackupTimeout so an honest slow run never gets reclassified.
	// Best-effort: failures log + continue.
	if n := scheduler.ReconcileStuckRuns(context.Background(), metaStore.Runs(), logger, time.Now().UTC(), 0); n > 0 {
		logger.Info("startup watchdog reclassified stuck runs", "count", n)
	}

	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(middleware.RealIP)
	router.Use(middleware.Recoverer)
	router.Use(requestLogger)

	// Slice 1e.2 — SPA-facing handlers. Every handler reaches its
	// narrow store through the meta.Store's accessors (slice 1e.4b).
	sessionCfg := api.NewSessionConfigFromEnv([]byte(jwtSecret))
	// So the SPA header + a post-deploy smoke can confirm WHICH build
	// is serving. The same value /healthz below publishes; this wire
	// is latent to handler tests (every internal/api test builds its
	// own SessionConfig), so version_wire_test.go asserts it live.
	sessionCfg.Version = version
	metaCfg := &api.MetaConfig{Version: version, Commit: commit}
	overviewCfg := &api.OverviewConfig{
		Tenants:         metaStore.Tenants(),
		Runs:            metaStore.Runs(),
		StorageLocation: archiveLocation,
	}
	projectsCfg := &api.ProjectsConfig{
		Tenants:  metaStore.Tenants(),
		Runs:     metaStore.Runs(),
		Projects: metaStore.Projects(),
	}
	testConnCfg := &api.TestConnectionConfig{Projects: metaStore.Projects(), Timeout: 10 * time.Second}
	cancelCfg := &api.CancelConfig{Runs: metaStore.Runs()}
	statsCfg := &api.StatsConfig{Snapshots: metaStore.Snapshots(), Runs: metaStore.Runs()}
	downloadCfg := &api.DownloadConfig{Archive: archiveStore}
	archivesCfg := &api.ArchivesConfig{
		Tenants:  metaStore.Tenants(),
		Runs:     metaStore.Runs(),
		Projects: metaStore.Projects(),
	}
	// Alerts feature 2 (SPA+API gap top-to-bottom roll, 2026-11-18):
	// dismissal store persists per-id dismissals + a before-cutoff
	// high-water mark. Synthesizer walks recent failed/cancelled runs
	// across tenants. The Overview handler filters alerts through the
	// store state before rendering.
	//
	// In-memory impl for v1 — dismissals don't survive a Lambda cold
	// start. MongoDismissals ships alongside for when
	// BACKUP_PROJECTS_MODE=mongo is enabled (same lifecycle as the
	// other Mongo stores — follow-up to wire the switch).
	alertsDismissals := alerts.NewInMemoryDismissals()
	alertsCfg := &api.AlertsConfig{Dismissals: alertsDismissals}
	overviewCfg.AlertsDismissals = alertsDismissals
	overviewCfg.AlertsSynthesizer = alerts.Synthesizer{}

	// Feature 3 (SPA+API gap top-to-bottom, 2026-11-18): Telegram
	// notifications. Settings store persists bot token + chat id;
	// handlers validate against Telegram's Bot API + ship a test
	// message. In-memory for v1 — token survives Lambda warm reuse
	// but not cold starts. MongoStore ships alongside for the
	// BACKUP_PROJECTS_MODE=mongo switch (follow-up).
	notifyStore := notify.NewInMemoryStore()
	telegramClient := notify.NewTelegramClient()
	settingsCfg := &api.SettingsConfig{
		Store:           notifyStore,
		Telegram:        telegramClient,
		StorageLocation: archiveLocation,
		MaxParallel:     1,
	}

	// Unauthenticated endpoints — healthz + the session probe + login/logout.
	// /healthz now carries the build stamp (the only probe reachable
	// without credentials — see api.Healthz comment).
	router.Get("/healthz", api.Healthz(version))
	router.Get("/api/v1/session", sessionCfg.Session)
	router.Post("/api/v1/login", sessionCfg.Login)
	router.Post("/api/v1/logout", sessionCfg.Logout)

	// JWT-authenticated endpoints.
	router.Group(func(r chi.Router) {
		r.Use(api.JWTMiddleware([]byte(jwtSecret)))

		// Core tenant / run surface (predates the SPA; plugin-auth path
		// stays intact — register uses op=register claim).
		r.Post("/api/v1/tenants/register", api.TenantsRegister(metaStore.Tenants()))
		r.Get("/api/v1/tenants", api.TenantsList(metaStore.Tenants()))
		r.Delete("/api/v1/tenants/{name}", api.TenantsDelete(api.TenantsDeleteConfig{
			Tenants: metaStore.Tenants(),
			Runs:    metaStore.Runs(),
			Archive: archiveStore,
		}).ServeHTTP)
		r.Get("/api/v1/backups", api.NewBackupsListHandler(metaStore.Runs()).ServeHTTP)
		r.Post("/api/v1/tenants/{stack}/backup-now", api.NewBackupNowHandler(api.BackupNowConfig{
			Sch:          sch,
			Tenants:      metaStore.Tenants(),
			Runs:         metaStore.Runs(),
			MintWorker:   api.MintWorkerJWT([]byte(jwtSecret)),
			AgentURL:     strings.TrimRight(os.Getenv("BACKUP_AGENT_URL"), "/"),
			PathTemplate: "", // scheduler default; shared string
		}).ServeHTTP)
		// Internal callback — the Fargate worker POSTs here on exit
		// with status + size + s3 path + error. Service-token authed
		// (JWTMiddleware above) with the op="worker" JWT minted by
		// backup-now's MintWorker when it fired RunTask.
		r.Post("/api/v1/internal/runs/{id}/complete", api.InternalRunComplete(api.InternalRunCompleteConfig{
			Runs: metaStore.Runs(),
		}).ServeHTTP)
		r.Post("/api/v1/tenants/{stack}/restore", api.NewRestoreHandler(sch, metaStore.Runs()).ServeHTTP)
		r.Get("/api/v1/restores/{id}", api.NewRestoreStatusHandler(metaStore.Runs()).ServeHTTP)

		// Scheduler tick — EventBridge fires this every minute when
		// deployTo: lambda (design doc §5.3). Walks tenants, fires ECS
		// Fargate one-shot tasks per due tenant. ?force=true on an
		// operator-driven call runs every tenant regardless of
		// schedule. Nil Ecs here → handler builds its own client from
		// the AWS SDK default config chain at first-tick time.
		// Runs is what makes the due-check real: without it every tenant
		// reads as never-backed-up and fires on every tick.
		// Same dispatch wiring as Run Now above — deliberately, because
		// the two go through one path now. MintWorker + AgentURL are
		// what let the Fargate worker report completion; without them
		// no run is ever recorded succeeded, every tenant stays due
		// forever, and a 30-minute tick dumps the whole fleet 48 times
		// a day.
		r.Post("/api/v1/scheduler/tick", api.NewSchedulerTickHandler(api.SchedulerTickConfig{
			Tenants:      metaStore.Tenants(),
			Runs:         metaStore.Runs(),
			Projects:     metaStore.Projects(),
			MintWorker:   api.MintWorkerJWT([]byte(jwtSecret)),
			AgentURL:     strings.TrimRight(os.Getenv("BACKUP_AGENT_URL"), "/"),
			PathTemplate: "", // scheduler default; shared string
		}).ServeHTTP)

		// SPA-facing read-only surface (slice 1e.2 2026-11-13). Mutating
		// project CRUD + settings + stats endpoints stay TODO (slice 1e.3).
		r.Get("/api/v1/meta", metaCfg.Meta)
		r.Get("/api/v1/overview", overviewCfg.Overview)
		r.Get("/api/v1/projects/{name}", projectsCfg.Project)
		r.Get("/api/v1/projects/{name}/runs", projectsCfg.RunsForProject)

		// Slice 1e.3 — project CRUD + setSecret.
		r.Post("/api/v1/projects", projectsCfg.CreateProject)
		r.Put("/api/v1/projects/{name}", projectsCfg.UpdateProject)
		r.Delete("/api/v1/projects/{name}", projectsCfg.DeleteProject)
		r.Put("/api/v1/projects/{name}/secret", projectsCfg.SetSecret)

		// Slice 1e.3 — stats endpoints. Data flows from
		// metaStore.Snapshots() through the stats aggregator and out
		// as JSON matching the SPA's ProjectStats/ObjectStats/
		// ObjectHistory TypeScript types. Snapshot ingestion from the
		// scheduler is a follow-up slice — in the meantime a clean
		// deploy answers these routes with empty series.
		r.Get("/api/v1/projects/{name}/stats", statsCfg.ProjectStats)
		r.Get("/api/v1/projects/{name}/stats/objects", statsCfg.ObjectStats)
		r.Get("/api/v1/projects/{name}/stats/object-history", statsCfg.ObjectHistory)

		// Slice 1e.3 — test-connection + cancel. test-connection wraps
		// the Driver.Test shape (service/internal/backup/driver) so the
		// SPA's NewProject form can validate a URI before committing;
		// cancel marks the newest running BackupRun as cancelled
		// (worker-side signal plumbing is a 1e.4 follow-up).
		r.Post("/api/v1/test-connection", testConnCfg.TestConnection)
		r.Post("/api/v1/projects/{name}/cancel", cancelCfg.Cancel)

		// SPA SPA+API gap feature 1 (2026-11-18): archive inventory +
		// presigned-URL download for the SPA's ArchivesTab.
		//   - `GET /api/v1/projects/{name}/archives` groups the tenant's
		//     successful backup runs by target database + attaches
		//     retention keep-reasons so the operator sees what's
		//     protecting each archive.
		//   - `GET /api/v1/download?key=X` 302-redirects to a signed
		//     GET URL on the archive store (short TTL). Browser follows
		//     the redirect and downloads directly from S3 / FSStore —
		//     the Lambda doesn't stream the body, so there's no 6 MB
		//     API Gateway cap and no double egress.
		// Both sit behind the JWT middleware (operator session).
		r.Get("/api/v1/projects/{name}/archives", archivesCfg.Archives)
		r.Get("/api/v1/download", downloadCfg.Download)

		// SPA+API gap feature 2 (2026-11-18): alerts dismissal. The
		// read side lives on /api/v1/overview — alerts are derived
		// from recent runs, not stored. These two routes record the
		// operator's dismissal state (per-id + a "clear all" cutoff
		// high-water mark) so the Overview filter can hide them.
		r.Post("/api/v1/alerts/{id}/dismiss", alertsCfg.DismissOne)
		r.Post("/api/v1/alerts/dismiss-all", alertsCfg.DismissAll)

		// SPA+API gap feature 3 (2026-11-18): Telegram notifications
		// + settings read. The storage block in AppSettings mirrors
		// the archive location; the notify.telegram slot carries
		// chat_id + a token_set flag (never the token itself).
		r.Get("/api/v1/settings", settingsCfg.Settings)
		r.Post("/api/v1/settings/test-notify", settingsCfg.TestNotify)
		r.Post("/api/v1/settings/telegram/check", settingsCfg.TelegramCheck)
		r.Put("/api/v1/settings/telegram", settingsCfg.TelegramSave)
		r.Delete("/api/v1/settings/telegram", settingsCfg.TelegramDisable)
	})

	// Prometheus text-exposition. Unauthenticated: it exposes no
	// tenant-scoped data, only process-level counters + the
	// backup_store_backend{backend="..."} gauge an operator alerts
	// on after flipping USE_GOEMBEDB. No `/api/` prefix so it is NOT
	// behind the JWT middleware.
	router.Handle("/metrics", goembedbadmin.MetricsHandler())

	// Migration endpoint. Disabled unless ADMIN_MIGRATION_TOKEN is set
	// — the endpoint is a one-off operator tool for the external-Mongo
	// → goembedb cutover, NOT a steady-state route. Also silent when
	// USE_GOEMBEDB is off (no sink to write to).
	//
	// Path name matches the pre-cutover shape of THIS service (the
	// backup service already ran against external Mongo before this
	// adoption — see internal/backup/meta/meta.go), so the semantic
	// is "copy the external Mongo database into the embedded one".
	//
	// The env is already resolved (ARN → plaintext) at startup above,
	// so this reader sees the bearer the operator set via
	// config.migrationToken — not the SecretsManager ARN. Dropping
	// `migrationToken` from the server.yaml (not just rotating it)
	// tears the route down: an empty value here skips the route-
	// registration branch entirely.
	if adminToken := os.Getenv("ADMIN_MIGRATION_TOKEN"); adminToken != "" && migrationSinkDB != nil {
		migration := &goembedbadmin.MigrationConfig{
			Source:     migrationSourceDB, // may be nil → endpoint returns a 500 that names what is missing
			Sink:       migrationSinkDB,
			AdminToken: adminToken,
		}
		router.Post("/admin/migrate-mongo-to-goembedb", migration.Migrate)
		slog.Info("migration endpoint wired", "path", "/admin/migrate-mongo-to-goembedb",
			"source", migrationSourceDB != nil, "sink", true)

		// Second migration endpoint — colocated S3 → go-embedb. The
		// mongo endpoint above is for cross-fleet imports (operator
		// already runs METADATA_TYPE=mongo against an external DB); a
		// staging deploy with `metadata: colocated` would be orphaned by
		// it (empty external DB → verified=true against zero → every
		// tenant disappears on the next tick). This sibling endpoint
		// walks the live colocated meta.Store via its own accessors so
		// a `_meta/` key-layout drift between the writer and the
		// migration reader is unreachable by construction.
		//
		// Source is built fresh here (the service's own meta.Store may
		// be the goembedb-backed one post-USE_GOEMBEDB=1; the migration
		// needs to see the PRE-cutover colocated keyspace, which is
		// just the archive store + TypeColocated). Non-fatal: a build
		// failure logs + skips the endpoint rather than refusing to
		// boot.
		colocCtx, colocCancel := context.WithTimeout(ctx, 10*time.Second)
		colocSource, cerr := meta.NewFromConfig(colocCtx, meta.Config{Type: meta.TypeColocated}, archiveStore)
		colocCancel()
		if cerr != nil {
			slog.Warn("colocated migration source build failed — endpoint not mounted", "err", cerr)
		} else {
			colocMigration := &goembedbadmin.ColocatedMigrationConfig{
				Source:     colocSource,
				Sink:       migrationSinkDB,
				AdminToken: adminToken,
			}
			router.Post("/admin/migrate-colocated-to-goembedb", colocMigration.Migrate)
			slog.Info("colocated migration endpoint wired", "path", "/admin/migrate-colocated-to-goembedb",
				"source", true, "sink", true)
		}
	}

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

// backendLabel maps the boot-time backend selection to the
// Prometheus gauge label. "goembedb" wins over any METADATA_TYPE
// override since the embed is the actual listener the mongo client
// talks to; otherwise the operator-set METADATA_TYPE is echoed. An
// empty METADATA_TYPE resolves inside the meta package based on the
// archive backend (filesystem→sqlite, object-store→colocated); we
// don't re-resolve here — the gauge value "default" names that
// implicit-default path so an operator sees a distinct label from an
// explicit "colocated" / "sqlite" choice.
func backendLabel(useGoEmbedb bool) string {
	if useGoEmbedb {
		return "goembedb"
	}
	if t := strings.ToLower(os.Getenv("METADATA_TYPE")); t != "" {
		return t
	}
	return "default"
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

// maskMongoURI redacts the password from a mongodb://user:password@host
// form so a startup log can show the connection target without the
// credential. Called by worker.go against its BACKUP_TENANT_URI env.
func maskMongoURI(uri string) string {
	if idx := indexAt(uri); idx > 0 {
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
