// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package ui embeds the built React SPA so the entire dashboard ships
// inside the sc-component-backup service binary.
package ui

import "embed"

// BuildFS holds the compiled SPA assets. The directive is `all:` so
// vite's hashed filenames (which start with `.`-prefixed segments
// after some plugins) aren't silently dropped by the default embed
// rules.
//
// Verbatim pattern from forge-notifier/internal/ui/ui.go — the `all:`
// prefix is non-negotiable (bit the fleet multiple times when authors
// used bare `dist`).
//
//go:embed all:dist
var BuildFS embed.FS
