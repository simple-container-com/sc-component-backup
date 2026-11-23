// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package goembedbadmin

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// storeBackend is a labelled gauge that advertises which metadata
// backend sc-component-backup is actively using.
//
// One gauge per backend label, 1 for the active one and 0 for the
// others. Operator alerting pattern:
//
//	# A deploy flipped USE_GOEMBEDB=1 but writes are still landing on
//	# the external cluster — indicates a stale env the deploy did not pick up.
//	backup_store_backend{backend="mongo"} == 1 AND <expected>
//
// The gauge is set once at boot in main.go, after the metadata
// backend is chosen.
//
// Backend labels:
//
//   - "goembedb"  — embedded ferretdb (USE_GOEMBEDB=1)
//   - "mongo"     — external MongoDB (METADATA_TYPE=mongo)
//   - "postgres"  — PostgreSQL
//   - "mysql"     — MySQL / MariaDB
//   - "sqlite"    — local SQLite
//   - "colocated" — JSON-under-prefix on top of the archive store
var (
	storeBackend = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "backup_store_backend",
		Help: "Which metadata backend sc-component-backup is actively using (1 = active, 0 = inactive).",
	}, []string{"backend"})

	backendRegistered sync.Once

	// Known labels. The migration gauge sets the live one to 1 and
	// clears every other known label to 0 so a Prometheus scrape
	// after a cutover shows exactly one `== 1` sample.
	knownBackends = []string{"goembedb", "mongo", "postgres", "mysql", "sqlite", "colocated"}
)

// SetStoreBackend marks `name` as the active backend and clears the
// others. Idempotent — safe to call from boot even if another
// goroutine already recorded it.
func SetStoreBackend(name string) {
	backendRegistered.Do(func() {
		// Register lazily — the service built without this file
		// would still link cleanly, and prometheus.MustRegister
		// panics on a duplicate name if a test happens to reuse the
		// default registerer. The sync.Once bounds that.
		prometheus.MustRegister(storeBackend)
	})
	for _, b := range knownBackends {
		if b == name {
			storeBackend.WithLabelValues(b).Set(1)
		} else {
			storeBackend.WithLabelValues(b).Set(0)
		}
	}
}

// MetricsHandler is the Prometheus text-exposition endpoint.
//
// Unauthenticated: it exposes no tenant-scoped data, only the active
// backend + whatever prometheus/client_golang's default collectors
// (Go runtime, process) register. Matches the fleet pattern — the
// aigateway and go-embedb smoke both expose /metrics without auth.
func MetricsHandler() http.Handler {
	return promhttp.Handler()
}
