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

func (MySQLDriver) ListDatabases(_ context.Context, _ ConnectionSpec) ([]string, error) {
	return nil, ErrDriverUnsupported
}

func (MySQLDriver) Inventory(_ context.Context, _ ConnectionSpec, _ string) ([]CollectionStat, error) {
	return nil, ErrDriverUnsupported
}

func (MySQLDriver) Test(_ context.Context, _ ConnectionSpec) (TestResult, error) {
	return TestResult{Error: ErrDriverUnsupported.Error()}, nil
}

func (MySQLDriver) RestoreCommand() string {
	return `{cat} | mysql --host=<host> --user=<user> --password=<pw> <database>`
}
