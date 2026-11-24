// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package storage

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// s3ListBucketResult is the subset of the ListObjectsV2 XML payload
// the pagination test needs. Keeping it local to the test keeps the
// production path free of a test-only type.
type s3ListBucketResult struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	KeyCount              int            `xml:"KeyCount"`
	MaxKeys               int            `xml:"MaxKeys"`
	IsTruncated           bool           `xml:"IsTruncated"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	Contents              []s3ListObject `xml:"Contents"`
}

type s3ListObject struct {
	Key          string `xml:"Key"`
	Size         int64  `xml:"Size"`
	LastModified string `xml:"LastModified"`
}

// TestS3Store_List_PaginatesPast1000Keys regresses the live-caught
// 2026-11-24 defect: the pre-fix `S3Store.List` issued a single
// `ListObjectsV2` and returned its 1000-key page as the complete
// listing. This test stands up an httptest S3 shim that serves
// EXACTLY 1001 synthetic objects across two pages and asserts:
//
//  1. `List` returns 1001 objects (not 1000 — a 1000 here means the
//     bug is back; see s3.go's method-level comment).
//  2. The shim observed EXACTLY TWO `ListObjectsV2` requests — a
//     one-call loop + implicit retry would read as success on
//     assertion (1) alone.
//  3. The SECOND request's `continuation-token` query value equals
//     `page2-token` (the token the first response handed back).
//     Catches a loop that re-requests page 1 (continuation-token nil
//     or empty) or echoes the wrong token.
//
// The 1001 count is deliberate — 10 or 100 objects never cross the
// AWS page boundary and prove nothing. Sibling sc-component-observatory
// is landing the same test shape on the identical fix.
func TestS3Store_List_PaginatesPast1000Keys(t *testing.T) {
	var (
		callCount       atomic.Int32
		secondCallToken atomic.Value // string; populated on the second call
	)
	secondCallToken.Store("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") != "2" {
			http.Error(w, "unexpected request: not ListObjectsV2", http.StatusBadRequest)
			return
		}
		n := callCount.Add(1)
		token := r.URL.Query().Get("continuation-token")

		result := s3ListBucketResult{
			Name:    "bkt",
			Prefix:  r.URL.Query().Get("prefix"),
			MaxKeys: 1000,
		}
		switch n {
		case 1:
			// Page 1: 1000 objects + a NextContinuationToken.
			for i := 0; i < 1000; i++ {
				result.Contents = append(result.Contents, s3ListObject{
					Key:          fmt.Sprintf("obj-%04d", i),
					Size:         1,
					LastModified: "2026-11-24T00:00:00.000Z",
				})
			}
			result.KeyCount = 1000
			result.IsTruncated = true
			result.NextContinuationToken = "page2-token"
		case 2:
			// Record what the client actually sent so the test can
			// assert on it from the outside.
			secondCallToken.Store(token)
			// Page 2: 1 object, IsTruncated=false, no next token.
			result.Contents = append(result.Contents, s3ListObject{
				Key:          "obj-1000",
				Size:         1,
				LastModified: "2026-11-24T00:00:00.000Z",
			})
			result.KeyCount = 1
			result.IsTruncated = false
		default:
			// A third call means the loop is re-requesting pages —
			// turn the extra round-trip into a loud failure body so
			// the test's call-count assertion catches it.
			http.Error(w, fmt.Sprintf("unexpected ListObjectsV2 call #%d (token=%q)", n, token), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/xml")
		if err := xml.NewEncoder(w).Encode(result); err != nil {
			t.Errorf("encode xml: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	s, err := NewS3Store(context.Background(), S3Config{
		Bucket:       "bkt",
		Region:       "us-east-1",
		EndpointURL:  srv.URL,
		AccessKey:    "test-ak",
		SecretKey:    "test-sk",
		UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}

	objs, err := s.List(context.Background(), "_meta/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got, want := len(objs), 1001; got != want {
		t.Errorf("List len: got %d, want %d (a 1000 here means the single-page bug is back — see s3.go method comment)", got, want)
	}
	if got, want := callCount.Load(), int32(2); got != want {
		t.Errorf("ListObjectsV2 call count: got %d, want %d (one call = unpaginated bug; three+ = loop re-requests pages)", got, want)
	}
	if got, want := secondCallToken.Load().(string), "page2-token"; got != want {
		t.Errorf("second call continuation-token: got %q, want %q (a nil/empty token or wrong token means the loop re-requests page 1)", got, want)
	}
}
