// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package driver

import (
	"context"
	"io"
)

// PostgresDriver — stub until the first real Postgres smoke lands.
// Shape is: `pg_dump --format=custom --compress=9 --dbname=<URI>` +
// `pg_restore --clean --if-exists --dbname=<URI>`. Defer impl until a
// real Postgres smoke is on the roadmap.
type PostgresDriver struct{}

func (PostgresDriver) Name() string { return "postgres" }

func (PostgresDriver) Dump(_ context.Context, _ ConnectionSpec, _ io.Writer) error {
	return ErrDriverUnsupported
}

func (PostgresDriver) Restore(_ context.Context, _ ConnectionSpec, _ io.Reader) error {
	return ErrDriverUnsupported
}
