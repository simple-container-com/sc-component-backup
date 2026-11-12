// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"testing"

	. "github.com/onsi/gomega"
	scPlugin "github.com/simple-container-com/api/pkg/plugin"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
)

// PreDestroy for the backup is a fire-and-forget log + return. A real
// component's test would assert on the cleanup side effects — for backup,
// we just prove the hook is invokable with the Phase 1e v2 request shape.
func TestPreDestroy_FireAndForget(t *testing.T) {
	RegisterTestingT(t)

	pub := scPlugin.NewPublisher(0)
	req := &scpluginv1.PreDestroyRequest{
		Header:       &scpluginv1.RequestHeader{StackName: "smoke-backup", Environment: "smoke"},
		ResourceType: "sc-backup",
	}

	err := PreDestroy(t.Context(), pub, req)
	Expect(err).ToNot(HaveOccurred())
}
