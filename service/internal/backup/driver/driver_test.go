// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package driver

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestByName(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantErr  bool
	}{
		{"mongodb", "mongodb", false},
		{"mongo", "mongodb", false},
		{"MongoDB", "mongodb", false},
		{"  postgres  ", "postgres", false},
		{"postgresql", "postgres", false},
		{"mysql", "mysql", false},
		{"redis", "", true},
		{"", "", true},
		{"nonsense", "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			d, err := ByName(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want err, got driver %q", d.Name())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if d.Name() != c.wantName {
				t.Fatalf("want %q, got %q", c.wantName, d.Name())
			}
		})
	}
}

func TestStubsReturnErrDriverUnsupported(t *testing.T) {
	ctx := context.Background()
	spec := ConnectionSpec{URI: "x"}
	for _, d := range []Driver{&PostgresDriver{}, &MySQLDriver{}} {
		t.Run(d.Name(), func(t *testing.T) {
			if err := d.Dump(ctx, spec, io.Discard); !errors.Is(err, ErrDriverUnsupported) {
				t.Errorf("Dump: want ErrDriverUnsupported, got %v", err)
			}
			if err := d.Restore(ctx, spec, strings.NewReader("")); !errors.Is(err, ErrDriverUnsupported) {
				t.Errorf("Restore: want ErrDriverUnsupported, got %v", err)
			}
		})
	}
}

func TestTailStderr(t *testing.T) {
	// short input returned as-is (trimmed)
	if got := tailStderr([]byte("  hello  ")); got != "hello" {
		t.Errorf("short: got %q", got)
	}
	// long input truncated with leading "..."
	long := strings.Repeat("a", 500)
	got := tailStderr([]byte(long))
	if !strings.HasPrefix(got, "...") {
		t.Errorf("long: expected leading ..., got %q", got[:10])
	}
	if len(got) != 3+256 {
		t.Errorf("long: expected 259 chars, got %d", len(got))
	}
}

func TestMongoDBDriver_EmptyURI(t *testing.T) {
	d := &MongoDBDriver{}
	ctx := context.Background()
	err := d.Dump(ctx, ConnectionSpec{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "URI is empty") {
		t.Errorf("want URI-empty error, got %v", err)
	}
}
