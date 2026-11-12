// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package storage

import (
	"fmt"
	"strings"
	"time"
)

// RenderPath substitutes known placeholders in a path template.
//
// Supported placeholders:
//   - {tenant}       → sanitised tenantStack (slash-safe)
//   - {resource}     → sanitised resource name
//   - {yyyy} {mm} {dd} {hh} {mi} {ss} → zero-padded UTC time parts
//   - {ts-rfc3339}   → full RFC3339 timestamp, colons → hyphens
//     (S3 keys with colons work but are a nuisance in curl / shell)
//   - {uuid}         → the provided runID (caller passes the BackupRun.ID)
//
// Unknown placeholders are left intact — operators can embed literal
// braces in a path by double-escaping; not v1-worth.
//
// Example:
//
//	template = "forge-{tenant}/{yyyy}/{mm}/{dd}/{resource}-{ts-rfc3339}.archive.gz"
//	result   = "forge-smoke-client/2026/11/11/mongodb-2026-11-11T09-30-15Z.archive.gz"
func RenderPath(template, tenantStack, resourceName, runID string, now time.Time) string {
	now = now.UTC()
	r := strings.NewReplacer(
		"{tenant}", safeSegment(tenantStack),
		"{resource}", safeSegment(resourceName),
		"{yyyy}", fmt.Sprintf("%04d", now.Year()),
		"{mm}", fmt.Sprintf("%02d", now.Month()),
		"{dd}", fmt.Sprintf("%02d", now.Day()),
		"{hh}", fmt.Sprintf("%02d", now.Hour()),
		"{mi}", fmt.Sprintf("%02d", now.Minute()),
		"{ss}", fmt.Sprintf("%02d", now.Second()),
		"{ts-rfc3339}", strings.ReplaceAll(now.Format(time.RFC3339), ":", "-"),
		"{uuid}", safeSegment(runID),
	)
	return r.Replace(template)
}

// safeSegment strips characters a filesystem / S3 key shouldn't carry
// in a single segment — slashes and control chars. Leaves hyphens,
// underscores, dots untouched.
func safeSegment(s string) string {
	b := strings.Builder{}
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '/' || r == '\\':
			b.WriteByte('-')
		case r < 0x20 || r == 0x7f:
			// drop control chars
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
