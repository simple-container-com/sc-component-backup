// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Honest notes carried forward from sibling sc-component-observatory's
// `fix/s3-list-pagination` session (same fix landed as
// `sc-component-observatory#33`):
//
//   - `len(result) == 1001` alone does NOT pin pagination. A loop that
//     re-requests page 1 forever against a fake that ignores the
//     token returns 2000 objects and passes a count-only assertion.
//     Hence the fake REFUSES unknown tokens, and the test asserts the
//     second call's token equals the one page 1 handed back.
//   - A fake that selects pages by CALL COUNT (not by token) passes a
//     loop that sends the wrong token on page 2 — the fake would
//     still return page 2 because it is call #2. Hence page selection
//     is TOKEN-DRIVEN here.
//   - `Prefix` sent only on page 1 (and nil on page 2) makes page 2
//     list the whole bucket — a superset of what the caller asked
//     for. Every count assertion stays green against a real S3 whose
//     bucket has extra keys outside the prefix. Hence the explicit
//     prefix-stability assertion below.
//   - A later-page error silently dropped as "success with partial
//     first page" is the EXACT failure mode that trapped the migration:
//     a short list that reads as success is how `verified: true`
//     against `sinkRows >= s3Count` on a clipped source arose. Hence
//     the ErrorOnLaterPage test asserts BOTH `err != nil` AND
//     `got == nil`.
//
// Each test below carries a one-line comment naming which of the four
// hazards it closes, so a reader deleting one knows which hole it
// re-opens.

// pagingListAPI is a ListObjectsV2 fake that behaves like S3 does: at
// most pageSize keys per call, IsTruncated + NextContinuationToken
// while more remain, and — the part that matters — it REFUSES a
// continuation token it did not mint. A fake that ignores the token
// cannot tell a correct pagination loop from one that re-requests
// page 1 forever.
type pagingListAPI struct {
	objs     []types.Object
	pageSize int

	// calls records the ContinuationToken of every request, in order.
	// nil for the first page; the string value afterwards.
	calls []*string

	// prefixes records the Prefix of every request, in order. Lets a
	// test assert the prefix is sent on EVERY page, not just the first.
	prefixes []string

	// issued maps a minted token to the offset it stands for, so an
	// unrecognised token is an error rather than a silent restart.
	issued map[string]int
}

func (p *pagingListAPI) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	p.calls = append(p.calls, in.ContinuationToken)
	p.prefixes = append(p.prefixes, aws.ToString(in.Prefix))

	offset := 0
	if tok := aws.ToString(in.ContinuationToken); tok != "" {
		off, ok := p.issued[tok]
		if !ok {
			return nil, fmt.Errorf("fake s3: unknown continuation token %q", tok)
		}
		offset = off
	}

	end := offset + p.pageSize
	if end > len(p.objs) {
		end = len(p.objs)
	}
	out := &s3.ListObjectsV2Output{Contents: p.objs[offset:end]}
	if end < len(p.objs) {
		if p.issued == nil {
			p.issued = map[string]int{}
		}
		tok := fmt.Sprintf("tok-%d", end)
		p.issued[tok] = end
		out.IsTruncated = aws.Bool(true)
		out.NextContinuationToken = aws.String(tok)
	} else {
		out.IsTruncated = aws.Bool(false)
	}
	return out, nil
}

// fixtureObjects builds N synthetic objects with zero-padded keys so
// lexical order == numeric order (the way real S3 keys sort and the
// way the migration iterates).
func fixtureObjects(n int) []types.Object {
	objs := make([]types.Object, 0, n)
	base := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		size := int64(i)
		lm := base.Add(time.Duration(i) * time.Second)
		objs = append(objs, types.Object{
			Key:          aws.String(fmt.Sprintf("_meta/backup_runs/%05d.json", i)),
			Size:         &size,
			LastModified: &lm,
		})
	}
	return objs
}

// TestS3Store_List_PaginatesPastPageCap is the regression test for the
// unpaginated-list bug: a single ListObjectsV2 returns at most 1000
// keys, and the pre-fix List read exactly one page and dropped the
// rest without erroring.
//
// 1001 is the only interesting fixture size — it is the first count a
// one-page lister gets wrong. 10 or 100 never cross the boundary and
// prove nothing.
//
// Closes hazards (see file header): count-passes-with-loop (via the
// token assertion); call-count-tied-fake (via the token-driven fake).
func TestS3Store_List_PaginatesPastPageCap(t *testing.T) {
	const total = 1001
	api := &pagingListAPI{objs: fixtureObjects(total), pageSize: 1000}

	got, err := listAllObjects(context.Background(), api, "bkt", "_meta/backup_runs/")
	if err != nil {
		t.Fatalf("listAllObjects: %v", err)
	}

	if len(got) != total {
		t.Fatalf("got %d objects, want %d (a 1000 here means the listing was truncated at the page cap)", len(got), total)
	}
	// Spot-check both ends: the first page's head and the key that
	// only exists on page 2.
	if got[0].Path != "_meta/backup_runs/00000.json" {
		t.Errorf("first key = %q, want _meta/backup_runs/00000.json", got[0].Path)
	}
	if got[total-1].Path != "_meta/backup_runs/01000.json" {
		t.Errorf("last key = %q, want _meta/backup_runs/01000.json", got[total-1].Path)
	}
	if got[total-1].Size != int64(total-1) {
		t.Errorf("last size = %d, want %d", got[total-1].Size, total-1)
	}
	if got[total-1].ModifiedAt.IsZero() {
		t.Error("last ModifiedAt is zero; LastModified was not copied")
	}

	// Two calls, and the second one must carry the token the FIRST
	// call handed back — not nil, not empty, not a token of its own
	// invention. A loop that re-requests page 1 returns 2000 objects
	// and would pass a count-only assertion.
	if len(api.calls) != 2 {
		t.Fatalf("made %d ListObjectsV2 calls, want 2 (one call = unpaginated bug; three+ = loop re-requests pages)", len(api.calls))
	}
	if api.calls[0] != nil {
		t.Errorf("first call sent ContinuationToken %q, want nil", aws.ToString(api.calls[0]))
	}
	if tok := aws.ToString(api.calls[1]); tok != "tok-1000" {
		t.Errorf("second call sent ContinuationToken %q, want the token page 1 returned (tok-1000)", tok)
	}
}

// TestS3Store_List_SinglePageDoesNotLoop guards the other direction:
// an untruncated response must end the loop. Trusting the presence of
// NextContinuationToken instead of IsTruncated, or looping while the
// token is non-nil, spins forever against a real bucket that echoes a
// stray token on a terminal page.
func TestS3Store_List_SinglePageDoesNotLoop(t *testing.T) {
	api := &pagingListAPI{objs: fixtureObjects(3), pageSize: 1000}

	got, err := listAllObjects(context.Background(), api, "bkt", "")
	if err != nil {
		t.Fatalf("listAllObjects: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d objects, want 3", len(got))
	}
	if len(api.calls) != 1 {
		t.Fatalf("made %d ListObjectsV2 calls, want 1", len(api.calls))
	}
}

// TestS3Store_List_ErrorOnLaterPage — a failure on page 2 must
// surface, not return the partial first page as if it were the whole
// bucket.
//
// A short list that reads as success is exactly how the migration's
// `verified: true` on `sinkRows >= s3Count` against a clipped source
// arose. Asserting both `err != nil` AND `got == nil` catches a
// `return result, nil` on a later-page error — a bug the compiler
// does not care about and a count-only assertion does not catch.
func TestS3Store_List_ErrorOnLaterPage(t *testing.T) {
	api := &failOnSecondPage{pagingListAPI{objs: fixtureObjects(1001), pageSize: 1000}}

	got, err := listAllObjects(context.Background(), api, "bkt", "_meta/backup_runs/")
	if err == nil {
		t.Fatalf("want error from page 2, got %d objects and nil error", len(got))
	}
	if got != nil {
		t.Errorf("want nil result alongside the error, got %d objects", len(got))
	}
}

type failOnSecondPage struct{ pagingListAPI }

func (f *failOnSecondPage) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if in.ContinuationToken != nil {
		return nil, errors.New("boom")
	}
	return f.pagingListAPI.ListObjectsV2(ctx, in, optFns...)
}

// TestS3Store_List_TrimsLeadingSlash pins the prefix normalisation
// that moved into listAllObjects with the pagination loop. A caller
// passes "/x" and the backend must see "x".
func TestS3Store_List_TrimsLeadingSlash(t *testing.T) {
	api := &pagingListAPI{objs: fixtureObjects(1), pageSize: 1000}

	if _, err := listAllObjects(context.Background(), api, "bkt", "/_meta/backup_runs/"); err != nil {
		t.Fatalf("listAllObjects: %v", err)
	}
	if len(api.prefixes) != 1 {
		t.Fatalf("made %d calls, want 1", len(api.prefixes))
	}
	if api.prefixes[0] != "_meta/backup_runs/" {
		t.Errorf("sent Prefix %q, want _meta/backup_runs/ (leading slash not trimmed)", api.prefixes[0])
	}
}

// TestS3Store_List_PaginatedPrefixIsStable — every page must carry
// the same (trimmed) prefix. Sending it only on the first request, or
// mutating it per-page, would make page 2 list the whole bucket — a
// SUPERSET of the caller's request that passes a count-only assertion
// but migrates keys outside the intended namespace.
func TestS3Store_List_PaginatedPrefixIsStable(t *testing.T) {
	api := &pagingListAPI{objs: fixtureObjects(1001), pageSize: 1000}

	if _, err := listAllObjects(context.Background(), api, "bkt", "/_meta/backup_runs/"); err != nil {
		t.Fatalf("listAllObjects: %v", err)
	}
	if len(api.prefixes) != 2 {
		t.Fatalf("made %d calls, want 2", len(api.prefixes))
	}
	for i, p := range api.prefixes {
		if p != "_meta/backup_runs/" {
			t.Errorf("call %d sent Prefix %q, want _meta/backup_runs/", i, p)
		}
	}
}
