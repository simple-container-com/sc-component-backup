// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	. "github.com/onsi/gomega"
)

// Three cases per design doc §P3 push-model operational contract:
//   1. Happy path — first attempt 200.
//   2. 5xx then 200 — retries succeed.
//   3. 500 all three attempts — error returned, names the component.
//
// Uses a local httptest.Server for the target; the retry timing is
// shortened by overriding the backoff schedule for tests (so the suite
// doesn't take 7+ seconds on the sad path).

func TestPushRegister_HappyPath(t *testing.T) {
	RegisterTestingT(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		Expect(r.Header.Get("Authorization")).To(HavePrefix("Bearer "))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	proc := NewProcessor(ProcessorConfig{HTTPClient: srv.Client()})
	err := proc.pushRegister(t.Context(), srv.URL, "fake-jwt", TenantRecord{StackName: "test"})
	Expect(err).ToNot(HaveOccurred())
	Expect(atomic.LoadInt32(&hits)).To(Equal(int32(1)))
}

func TestPushRegister_RetrySucceeds(t *testing.T) {
	RegisterTestingT(t)

	// Shrink backoff for tests — overwrite via package-level var.
	saved := backoffSeconds
	backoffSeconds = []int{0, 0, 0}
	defer func() { backoffSeconds = saved }()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	proc := NewProcessor(ProcessorConfig{HTTPClient: srv.Client()})
	err := proc.pushRegister(t.Context(), srv.URL, "fake-jwt", TenantRecord{StackName: "test"})
	Expect(err).ToNot(HaveOccurred())
	Expect(atomic.LoadInt32(&hits)).To(Equal(int32(2)))
}

func TestPushRegister_AllFail(t *testing.T) {
	RegisterTestingT(t)

	saved := backoffSeconds
	backoffSeconds = []int{0, 0, 0}
	defer func() { backoffSeconds = saved }()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	proc := NewProcessor(ProcessorConfig{HTTPClient: srv.Client()})
	err := proc.pushRegister(t.Context(), srv.URL, "fake-jwt", TenantRecord{StackName: "test"})
	Expect(err).To(HaveOccurred())
	Expect(err.Error()).To(ContainSubstring("all 3 attempts failed"))
	Expect(atomic.LoadInt32(&hits)).To(Equal(int32(3)))
}

func TestPushRegister_NonRetryable4xx(t *testing.T) {
	RegisterTestingT(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	proc := NewProcessor(ProcessorConfig{HTTPClient: srv.Client()})
	err := proc.pushRegister(t.Context(), srv.URL, "fake-jwt", TenantRecord{StackName: "test"})
	Expect(err).To(HaveOccurred())
	Expect(err.Error()).To(ContainSubstring("non-retryable http 403"))
	Expect(atomic.LoadInt32(&hits)).To(Equal(int32(1)), "403 must NOT retry (only 4xx-except-429 are non-retryable)")
}

func TestMintJWT(t *testing.T) {
	RegisterTestingT(t)

	tok, err := mintJWT("a-fake-signing-key-that-is-at-least-32-bytes-long", "my-consumer-stack", 60)
	Expect(err).ToNot(HaveOccurred())
	Expect(tok).ToNot(BeEmpty())
	// HS256 JWTs are 3 base64url segments separated by dots.
	segments := 0
	for _, c := range tok {
		if c == '.' {
			segments++
		}
	}
	Expect(segments).To(Equal(2))
}

func TestReadDeployedServiceURL(t *testing.T) {
	RegisterTestingT(t)

	// Shape 1: bare string.
	v, ok := ReadDeployedServiceURL([]byte(`{"_deployed_service":"https://backup.example.com"}`))
	Expect(ok).To(BeTrue())
	Expect(v).To(Equal("https://backup.example.com"))

	// Shape 2: {url: ...} map.
	v, ok = ReadDeployedServiceURL([]byte(`{"_deployed_service":{"url":"https://alt.example.com"}}`))
	Expect(ok).To(BeTrue())
	Expect(v).To(Equal("https://alt.example.com"))

	// Shape 3: PluginResourceOutput JSON — the CURRENT SC core
	// sidecar packs the aws-lambda template's Outputs here.
	// Scanner finds the `*-function-url` key by suffix.
	pro := `{"_deployed_service":{"PluginName":"aws","Urn":"urn:pulumi:...","Outputs":{"my-backup--staging-staging-function-url":{"Value":"https://xxxx.lambda-url.eu-central-1.on.aws","Secret":false},"my-backup--staging-staging-lambda-arn":{"Value":"arn:aws:lambda:...","Secret":false}}}}`
	v, ok = ReadDeployedServiceURL([]byte(pro))
	Expect(ok).To(BeTrue())
	Expect(v).To(Equal("https://xxxx.lambda-url.eu-central-1.on.aws"))

	// Shape 3 negative: PluginResourceOutput with no function-url
	// key — the sidecar's Outputs may hold only lambda-arn
	// (hypothetical; unlikely in practice but defensive).
	proNoURL := `{"_deployed_service":{"PluginName":"aws","Outputs":{"foo-lambda-arn":{"Value":"arn:aws:lambda:..."}}}}`
	v, ok = ReadDeployedServiceURL([]byte(proNoURL))
	Expect(ok).To(BeFalse())
	Expect(v).To(BeEmpty())

	// Empty inputs.
	v, ok = ReadDeployedServiceURL([]byte(`{}`))
	Expect(ok).To(BeFalse())
	Expect(v).To(BeEmpty())

	v, ok = ReadDeployedServiceURL(nil)
	Expect(ok).To(BeFalse())
	Expect(v).To(BeEmpty())
}

// TestParentFunctionURLKeys pins the candidate order for the
// parent-stack Function URL lookup.
//
// The component-scoped key MUST come first. Before sc-plugin-aws named
// aws-lambda resources per-Component, the template exported this under
// `<parentStack>-<env>-function-url` — one key for the whole stack. A
// parent stack hosting two Components therefore had both dispatches
// writing that one key, last writer winning, so preferring it would
// silently resolve one Component's service URL to the other's Lambda.
// No error, no log line: every consumer would push-register its tenant
// record into the wrong service.
//
// The legacy key stays as the second candidate so an operator on an
// older sc-plugin-aws still resolves.
func TestParentFunctionURLKeys(t *testing.T) {
	RegisterTestingT(t)

	keys := parentFunctionURLKeys("infra", "my-backup", "staging")
	Expect(keys).To(HaveLen(2))
	Expect(keys[0]).To(Equal("infra-my-backup-staging-function-url"))
	Expect(keys[1]).To(Equal("infra-staging-function-url"))

	// A second Component on the same parent stack must not share the
	// scoped key — that collision is the bug this ordering fixes.
	other := parentFunctionURLKeys("infra", "my-observatory", "staging")
	Expect(other[0]).NotTo(Equal(keys[0]))
	// ...while both still fall back to the same legacy key, which is
	// exactly why it cannot be tried first.
	Expect(other[1]).To(Equal(keys[1]))
}
