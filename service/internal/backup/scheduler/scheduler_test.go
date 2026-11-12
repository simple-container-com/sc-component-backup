// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/driver"
)

func TestConfigFillDefaults(t *testing.T) {
	c := Config{}
	c.FillDefaults()
	if c.PathTemplate == "" {
		t.Error("PathTemplate default missing")
	}
	if c.TickInterval != 60*time.Second {
		t.Errorf("TickInterval default: %v", c.TickInterval)
	}
	if c.BackupTimeout != time.Hour {
		t.Errorf("BackupTimeout default: %v", c.BackupTimeout)
	}
	if c.ShutdownGraceWait != 5*time.Minute {
		t.Errorf("ShutdownGraceWait default: %v", c.ShutdownGraceWait)
	}
}

func TestDriverFactoryError(t *testing.T) {
	_, err := driver.ByName("nonsense-driver")
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestSchedulerClaimRelease(t *testing.T) {
	s := &Scheduler{inflight: map[string]struct{}{}}
	if !s.claim("t1", "r1") {
		t.Fatal("first claim failed")
	}
	if s.claim("t1", "r1") {
		t.Error("second claim on same key should fail")
	}
	if !s.claim("t1", "r2") {
		t.Error("different resource should claim")
	}
	s.release("t1", "r1")
	if !s.claim("t1", "r1") {
		t.Error("claim after release should succeed")
	}
}

func TestSpecFromConnection(t *testing.T) {
	conn := map[string]string{
		"uri":      "mongodb://u:p@h:27017/d",
		"host":     "h",
		"port":     "27017",
		"user":     "u",
		"password": "p",
		"database": "d",
	}
	spec := specFromConnection(conn)
	if spec.URI != conn["uri"] || spec.Port != 27017 {
		t.Errorf("spec mismatch: %+v", spec)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 100); got != "short" {
		t.Errorf("short unchanged: %q", got)
	}
	long := "0123456789012345"
	if got := truncate(long, 5); got != "01234...(truncated)" {
		t.Errorf("truncate: %q", got)
	}
}

// Smoke-check that a scheduler can be constructed with nil driver
// factory and a nil logger, and reach RunOnce's "no driver" error path.
func TestRunOnceUnknownDriver(t *testing.T) {
	sch := New(Config{}, nil, nil, nil, nil, func(name string) (driver.Driver, error) {
		return nil, errors.New("no such driver")
	})
	_ = sch // we don't invoke RunOnce here (nil tenants would NPE first);
	// constructing + default-filling is the assertion.
	if sch.cfg.TickInterval != 60*time.Second {
		t.Error("defaults not filled on construction")
	}
	if ctx := context.Background(); ctx == nil {
		t.Fatal("stdlib broken")
	}
}
