// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"testing"
)

// TestErrString covers the trivial nil/non-nil format helper. Keeps vet
// happy + proves the helper doesn't panic on nil.
func TestErrString(t *testing.T) {
	if got := errString(nil); got != "" {
		t.Errorf("errString(nil) = %q, want \"\"", got)
	}
	err := errSentinel{}
	if got := errString(err); got != "sentinel" {
		t.Errorf("errString(err) = %q, want sentinel", got)
	}
}

type errSentinel struct{}

func (errSentinel) Error() string { return "sentinel" }

// Note on worker-mode integration testing:
//
// runWorker() uses os.Exit on missing env + os.Exit on dump/upload
// failure, so a subprocess invocation is the right shape to prove the
// branch end-to-end. The actual dump behaviour is tested at the driver
// level (service/internal/backup/driver/*_test.go) and the upload at
// the storage level (service/internal/backup/storage/s3_test.go). The
// worker just wires them together — verified at smoke time.
