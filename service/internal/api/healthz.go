// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"encoding/json"
	"net/http"
)

// Healthz returns 200 with a JSON body carrying the build stamp.
// Unauthenticated per sc-components-platform design doc §P3 — the smoke
// needs a probe that doesn't depend on the JWT path, and k8s / ECS /
// Lambda health checks typically don't carry a Bearer.
//
// The build stamp exists here because a pin bump was unverifiable from
// outside: the version is logged once at boot and reading that log
// needs AWS credentials the operator smoking a deploy usually does
// not have — so "the provision went green" was the only evidence that
// a new image was serving, which is evidence that the apply ran, not
// that this process is the build you merged. The /healthz surface is
// the only probe a post-deploy smoke can reach with no credentials at
// all, so the signal has to live here. A CalVer `vYYYY.MM.DD-<short-
// sha>` is not a credential — the short SHA is already in every
// release/tag/image-ref — so it is written ungated.
//
// Mirrors sc-component-notifier#6's `api.Healthz(version)` design.
func Healthz(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"version": version,
		})
	}
}
