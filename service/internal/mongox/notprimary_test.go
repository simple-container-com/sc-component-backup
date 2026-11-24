// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package mongox

import (
	"errors"
	"fmt"
	"testing"

	"go.mongodb.org/mongo-driver/mongo"
)

// errNotWritablePrimary mirrors the shape go-embedb's lease gate puts on
// the wire: a command error carrying code 10107 and the `primary` hint.
// Built from the real driver type so errors.As behaves as in production.
//
// The 10107 here is deliberately a LITERAL, not codeNotWritablePrimary.
// It is a Mongo wire-spec constant owned by go-embedb's
// handlererrors.ErrNotWritablePrimary, not by this package. Building the
// fixture from the package's own const made the first version of this
// test self-consistent and therefore useless: flipping the const to
// 10108 moved the fixture and the assertion together and the suite
// still passed.
func errNotWritablePrimary() error {
	return mongo.CommandError{
		Code:    10107,
		Name:    "NotWritablePrimary",
		Message: "not master - please connect to primary slot 127.0.0.1:0",
	}
}

// TestCodeMatchesWireSpec pins the constant to the wire value
// independently of every fixture above.
func TestCodeMatchesWireSpec(t *testing.T) {
	t.Parallel()

	if codeNotWritablePrimary != 10107 {
		t.Fatalf("codeNotWritablePrimary = %d, want 10107 (Mongo wire spec; "+
			"must match go-embedb handlererrors.ErrNotWritablePrimary)",
			codeNotWritablePrimary)
	}
}

func TestIsNotWritablePrimary(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"bare 10107", errNotWritablePrimary(), true},
		{
			"wrapped 10107",
			fmt.Errorf("mongo index create: %w", errNotWritablePrimary()),
			true,
		},
		{
			"doubly wrapped 10107",
			fmt.Errorf("meta mongo: tenant store: %w",
				fmt.Errorf("mongo index create: %w", errNotWritablePrimary())),
			true,
		},
		{
			// The adjacent code must NOT be swallowed: a duplicate-key
			// failure on a unique index is a real schema conflict.
			"duplicate key 11000",
			mongo.CommandError{Code: 11000, Name: "DuplicateKey"},
			false,
		},
		{
			"unrelated code 26",
			mongo.CommandError{Code: 26, Name: "NamespaceNotFound"},
			false,
		},
		{"plain error", errors.New("connection refused"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := IsNotWritablePrimary(tc.err); got != tc.want {
				t.Fatalf("IsNotWritablePrimary(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestTolerateNotWritablePrimary(t *testing.T) {
	t.Parallel()

	t.Run("swallows 10107 so a non-owner replica can boot", func(t *testing.T) {
		t.Parallel()

		in := fmt.Errorf("mongo index create: %w", errNotWritablePrimary())
		if got := TolerateNotWritablePrimary("tenants.stackName", in); got != nil {
			t.Fatalf("got %v, want nil — a non-lease-owner must still boot", got)
		}
	})

	t.Run("passes every other error through unchanged", func(t *testing.T) {
		t.Parallel()

		in := errors.New("no reachable servers")
		got := TolerateNotWritablePrimary("tenants.stackName", in)
		if !errors.Is(got, in) {
			t.Fatalf("got %v, want the original error %v", got, in)
		}
	})

	t.Run("nil stays nil", func(t *testing.T) {
		t.Parallel()

		if got := TolerateNotWritablePrimary("tenants.stackName", nil); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})
}
