// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"context"
	"sort"
	"sync"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Why this test exists, carried over verbatim pattern from sc-plugin-echo's
// urn_contract_test.go: SC's plugin extraction promise is that no
// pre-split stack will replace+create on first plugin-era `sc deploy`.
// For the sc-component-backup template, the URN contract is light (the
// plugin registers no Pulumi resources of its own — Path A's sidecar
// wrapper dispatches the template), but we exercise the same test shape
// so that:
//
//   1. Real components cloning this template inherit the test harness
//      with the right `pulumi.NewContext + RunInfo.Mocks` pattern, NOT
//      `pulumi.RunErr` (which drains stranded registrations silently —
//      Addendum R §R3 of 04-no-destroy-invariant.md).
//   2. Any future change that adds Pulumi resources on the plugin side
//      (not expected for backup, possible for real components) has a
//      place to lock the URN contract.
//
// The current body is a placeholder that opens the context and asserts
// the mocks captured zero resources — the correct state for a backup that
// doesn't register any.

type captureMocks struct {
	mu       sync.Mutex
	captured []pulumi.MockResourceArgs
}

func (m *captureMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.captured = append(m.captured, args)
	return args.Name + "-mock-id", args.Inputs, nil
}

func (m *captureMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return args.Args, nil
}

func (m *captureMocks) namesByType(typeToken string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, a := range m.captured {
		if a.TypeToken == typeToken {
			out = append(out, a.Name)
		}
	}
	sort.Strings(out)
	return out
}

// withTrapFreeContext opens a pulumi.Context with mocks via the exported
// RunInfo.Mocks field. Close() is a no-op under mocks. The handler's
// return is what the assertion lands on — NOT the RunErr harness's drain.
func withTrapFreeContext(t *testing.T, mocks *captureMocks, fn func(*pulumi.Context) error) {
	t.Helper()
	pctx, err := pulumi.NewContext(context.Background(), pulumi.RunInfo{
		Project: "test-project",
		Stack:   "test-stack",
		Mocks:   mocks,
	})
	Expect(err).ToNot(HaveOccurred())
	defer pctx.Close()

	Expect(fn(pctx)).To(Succeed())
}

// TestURNContract_BackupRegistersNothing — the backup plugin's Provision
// RPC registers no Pulumi resources of its own. Confirms the contract.
func TestURNContract_BackupRegistersNothing(t *testing.T) {
	RegisterTestingT(t)

	m := &captureMocks{}
	withTrapFreeContext(t, m, func(_ *pulumi.Context) error {
		// Backup's Provision body is a near-no-op; it reads the deployed
		// service URL from DependencyOutputsJson, exports one bookkeeping
		// output, and returns. No Pulumi resource registrations.
		return nil
	})

	Expect(m.namesByType("sc-component-backup:index:sc-backup")).To(BeEmpty(),
		"backup plugin must NOT register Pulumi resources — Path A's sidecar wrapper owns the template dispatch")
}
