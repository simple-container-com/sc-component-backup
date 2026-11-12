// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package driver

import (
	"context"
	"io"
)

// MySQLDriver — stub until the first real MySQL smoke lands. Shape is:
// `mysqldump --single-transaction --quick --routines --events
// --triggers` streamed to stdout; restore is `mysql` reading stdin.
type MySQLDriver struct{}

func (MySQLDriver) Name() string { return "mysql" }

func (MySQLDriver) Dump(_ context.Context, _ ConnectionSpec, _ io.Writer) error {
	return ErrDriverUnsupported
}

func (MySQLDriver) Restore(_ context.Context, _ ConnectionSpec, _ io.Reader) error {
	return ErrDriverUnsupported
}
