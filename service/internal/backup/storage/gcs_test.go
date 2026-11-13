// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Integration tests gate on BACKUP_GCS_TEST_BUCKET. An operator with a
// throwaway GCS bucket + working ADC can run them; CI stays green
// without those preconditions.
//
// For authenticated tests, point BACKUP_GCS_TEST_BUCKET at a bucket
// your ADC has objects.create / get / list / delete on. If you also
// set BACKUP_GCS_TEST_CREDENTIALS_FILE, Presign will sign locally
// (preferred for a reproducible integration run).

func gcsTestBucket(t *testing.T) (*GCSStore, string, bool) {
	t.Helper()
	bucket := os.Getenv("BACKUP_GCS_TEST_BUCKET")
	if bucket == "" {
		return nil, "", false
	}
	cfg := GCSConfig{
		Bucket:          bucket,
		CredentialsFile: os.Getenv("BACKUP_GCS_TEST_CREDENTIALS_FILE"),
	}
	s, err := NewGCSStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewGCSStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, bucket, true
}

func TestGCSStore_PutGetRoundTrip(t *testing.T) {
	s, _, ok := gcsTestBucket(t)
	if !ok {
		t.Skip("integration — needs BACKUP_GCS_TEST_BUCKET")
	}
	key := "sc-component-backup/test/" + t.Name()
	n, err := s.Put(context.Background(), key, strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != 5 {
		t.Errorf("Put size: got %d, want 5", n)
	}
	rc, err := s.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != "hello" {
		t.Errorf("Get body: %q", body)
	}
	_ = s.Delete(context.Background(), key)
}

func TestGCSStore_GetMissingReturnsErrNotFound(t *testing.T) {
	s, _, ok := gcsTestBucket(t)
	if !ok {
		t.Skip("integration — needs BACKUP_GCS_TEST_BUCKET")
	}
	_, err := s.Get(context.Background(), "sc-component-backup/test/does-not-exist-"+t.Name())
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing: want ErrNotFound, got %v", err)
	}
}

func TestGCSStore_ListByPrefix(t *testing.T) {
	s, _, ok := gcsTestBucket(t)
	if !ok {
		t.Skip("integration — needs BACKUP_GCS_TEST_BUCKET")
	}
	prefix := "sc-component-backup/test/" + t.Name() + "/"
	keys := []string{prefix + "a.txt", prefix + "b.txt", prefix + "c.txt"}
	for _, k := range keys {
		_, _ = s.Put(context.Background(), k, strings.NewReader(k))
	}
	defer func() {
		for _, k := range keys {
			_ = s.Delete(context.Background(), k)
		}
	}()
	rows, err := s.List(context.Background(), prefix)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != len(keys) {
		t.Errorf("List: got %d rows, want %d", len(rows), len(keys))
	}
}

func TestGCSStore_Delete(t *testing.T) {
	s, _, ok := gcsTestBucket(t)
	if !ok {
		t.Skip("integration — needs BACKUP_GCS_TEST_BUCKET")
	}
	key := "sc-component-backup/test/" + t.Name()
	_, _ = s.Put(context.Background(), key, strings.NewReader("x"))
	if err := s.Delete(context.Background(), key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(context.Background(), key); !errors.Is(err, ErrNotFound) {
		t.Errorf("Second Delete: want ErrNotFound, got %v", err)
	}
}

func TestGCSStore_PresignRoundTrip(t *testing.T) {
	s, _, ok := gcsTestBucket(t)
	if !ok {
		t.Skip("integration — needs BACKUP_GCS_TEST_BUCKET")
	}
	if s.presignUnavailable != nil {
		t.Skipf("Presign unavailable: %v (set BACKUP_GCS_TEST_CREDENTIALS_FILE)", s.presignUnavailable)
	}
	key := "sc-component-backup/test/" + t.Name()
	payload := "presign-body"
	_, _ = s.Put(context.Background(), key, strings.NewReader(payload))
	defer s.Delete(context.Background(), key)

	url, err := s.Presign(context.Background(), key, 1*time.Minute)
	if err != nil {
		t.Fatalf("Presign: %v", err)
	}
	if !strings.HasPrefix(url, "https://") {
		t.Fatalf("Presign URL shape: %q", url)
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET presigned URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET presigned URL: status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != payload {
		t.Errorf("Presign GET body: %q, want %q", body, payload)
	}
}

func TestNewGCSStore_RefusesEmptyBucket(t *testing.T) {
	_, err := NewGCSStore(context.Background(), GCSConfig{})
	if err == nil {
		t.Fatal("expected refusal on empty Bucket")
	}
	if !strings.Contains(err.Error(), "Bucket is required") {
		t.Errorf("error wording: %v", err)
	}
}

// Presign on a non-SA credentials path returns the typed reason — not
// a nil error with a broken URL. This test runs without a real GCS
// bucket by stubbing presignUnavailable directly on the struct.
func TestGCSStore_PresignRefusesWithoutSigner(t *testing.T) {
	s := &GCSStore{
		cfg:                GCSConfig{Bucket: "x"},
		presignUnavailable: errors.New("test — no signer"),
	}
	_, err := s.Presign(context.Background(), "k", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "test — no signer") {
		t.Errorf("Presign: want signer-unavailable, got %v", err)
	}
}
