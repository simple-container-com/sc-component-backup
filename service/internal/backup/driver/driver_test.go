// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package driver

import (
	"context"
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

// A child that exits non-zero having printed nothing must say so. The
// 2026-11-24 mongodump failure logged `stderr tail: ` with nothing
// after the colon, which reads like a truncated log line.
func TestTailStderrNamesTheEmptyCase(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t \n"} {
		if got := tailStderr([]byte(in)); got != "(no stderr output)" {
			t.Errorf("tailStderr(%q) = %q, want %q", in, got, "(no stderr output)")
		}
	}
}

// mongodump runs without --quiet so its failure line is captured, which
// means stderr also carries progress output: the buffer must stay bounded
// by the size of the tail, not grow with the size of the dump.
func TestTailBufferKeepsOnlyTheTail(t *testing.T) {
	b := &tailBuffer{max: 8}
	for i := 0; i < 100; i++ {
		if n, err := b.Write([]byte("0123456789")); n != 10 || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if len(b.Bytes()) != 8 {
		t.Fatalf("retained %d bytes, want 8 (buffer grew with input)", len(b.Bytes()))
	}
	if got := string(b.Bytes()); got != "23456789" {
		t.Errorf("retained %q, want the LAST 8 bytes %q", got, "23456789")
	}
}

// A short write must not be padded or dropped — the tail of a child that
// printed less than max is the whole of its output.
func TestTailBufferShortWrite(t *testing.T) {
	b := &tailBuffer{max: 4096}
	_, _ = b.Write([]byte("Failed: auth error"))
	if got := string(b.Bytes()); got != "Failed: auth error" {
		t.Errorf("got %q", got)
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
