// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import "net/http"

// Healthz returns 200 with a one-line body. Unauthenticated per
// sc-components-platform design doc §P3 — the smoke needs a probe that
// doesn't depend on the JWT path, and k8s / ECS / Lambda health checks
// typically don't carry a Bearer.
func Healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
