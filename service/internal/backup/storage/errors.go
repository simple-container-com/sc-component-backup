// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package storage

import "errors"

// stdErrorsAs isolates the stdlib import for the one call site in s3.go.
func stdErrorsAs(err error, target any) bool {
	return errors.As(err, target)
}
