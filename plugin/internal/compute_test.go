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

	v, ok := ReadDeployedServiceURL([]byte(`{"_deployed_service":"https://backup.example.com"}`))
	Expect(ok).To(BeTrue())
	Expect(v).To(Equal("https://backup.example.com"))

	v, ok = ReadDeployedServiceURL([]byte(`{}`))
	Expect(ok).To(BeFalse())
	Expect(v).To(BeEmpty())

	v, ok = ReadDeployedServiceURL(nil)
	Expect(ok).To(BeFalse())
	Expect(v).To(BeEmpty())
}
