// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package storage — S3-backed object store for backup archives.
// Implements the Store interface; MinIO is the test backend, AWS S3
// is the production backend, both drive the same code (`endpointURL`
// swap at construction).
package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Store is the backup archive store. Implementations:
//
//   - S3Store       — production, also drives MinIO / R2 / DO Spaces /
//     Yandex Object Storage via EndpointURL.
//   - FSStore       — local filesystem (VM mount / k8s PV / ECS + EFS).
//   - GCSStore      — Google Cloud Storage (skeleton — full impl is
//     slice 1e.4f).
//   - InMemoryStore — unit-test backend (see mem.go).
//
// Design: forge docs/design/2026-11-13/sc-component-backup-pluggable-storage.
type Store interface {
	Put(ctx context.Context, path string, body io.Reader) (sizeBytes int64, err error)
	Get(ctx context.Context, path string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]StoredObject, error)
	Delete(ctx context.Context, path string) error
	// Presign mints a time-limited URL for `path`. For S3 + GCS the URL
	// is a full signed https:// URL. For FSStore (no external URL) the
	// return is a RELATIVE path like "/api/v1/files/<jwt>" that the
	// HTTP layer rewrites against the request's host header.
	Presign(ctx context.Context, path string, expires time.Duration) (string, error)
}

// ErrNotFound is returned by Store.Get when the key doesn't exist. Lets
// callers (e.g. the colocated metadata store) distinguish "missing" from
// "backend failure" without matching on wrapped error strings.
var ErrNotFound = &notFoundError{}

type notFoundError struct{}

func (*notFoundError) Error() string { return "storage: object not found" }

// StoredObject is a listing result.
type StoredObject struct {
	Path       string
	Size       int64
	ModifiedAt time.Time
}

// S3Config bundles the knobs. EndpointURL is empty for AWS; set for
// MinIO or any other S3-compatible gateway.
type S3Config struct {
	Bucket      string
	Region      string
	EndpointURL string // empty → AWS. For MinIO: "http://minio:9000"
	AccessKey   string
	SecretKey   string
	// UsePathStyle forces path-style addressing (bucket in the URL path
	// rather than the hostname). Required for MinIO; harmless on AWS.
	UsePathStyle bool
}

// S3Store is the Store backed by aws-sdk-go-v2.
type S3Store struct {
	cfg    S3Config
	client *s3.Client
}

// NewS3Store builds an S3Store ready for use. Errors out at
// construction time rather than deferring to first call, so a
// mis-configured service fails `/healthz`-visibly at boot.
func NewS3Store(ctx context.Context, cfg S3Config) (*S3Store, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("storage.NewS3Store: Bucket is required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	loadOpts := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.Region),
	}
	if cfg.AccessKey != "" {
		loadOpts = append(loadOpts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("storage.NewS3Store: load aws config: %w", err)
	}
	clientOpts := []func(*s3.Options){}
	if cfg.EndpointURL != "" {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.EndpointURL)
		})
	}
	if cfg.UsePathStyle {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.UsePathStyle = true
		})
	}
	client := s3.NewFromConfig(awsCfg, clientOpts...)
	return &S3Store{cfg: cfg, client: client}, nil
}

// Put streams body to s3://<bucket>/<path>. aws-sdk-go-v2 refuses a
// non-seekable Body over plain HTTP ("unseekable stream is not supported
// without TLS and trailing checksum") — a security-first default that
// bites a local MinIO/SeaweedFS smoke. For a caller that passes a
// *bytes.Reader (the buffer-then-upload shape v1 uses), pass it through
// directly so the SDK sees io.ReadSeeker; size comes from Len().
// For any other io.Reader, wrap in a countingReader (SDK will error out
// on the aforementioned trailing-checksum path over plain HTTP — a
// future streaming-dump path either enables TLS, or buffers upstream).
func (s *S3Store) Put(ctx context.Context, path string, body io.Reader) (int64, error) {
	path = strings.TrimPrefix(path, "/")
	// Seekable → PutObject with declared Content-Length (in-process path:
	// `bytes.NewReader(buf.Bytes())` in scheduler.RunOnce).
	if br, ok := body.(*bytes.Reader); ok {
		declaredSize := int64(br.Len())
		_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(s.cfg.Bucket),
			Key:    aws.String(path),
			Body:   br,
		})
		if err != nil {
			return 0, fmt.Errorf("s3 put %q: %w", path, err)
		}
		return declaredSize, nil
	}
	// Unknown-length stream (Fargate worker piping mongodump stdout) —
	// S3 PutObject refuses with "411 MissingContentLength" because the
	// SDK can't buffer the whole body before sending. The multipart
	// Uploader handles unknown-length streams natively: it splits the
	// Reader into parts as it reads, uploads each part, and completes
	// the multipart upload on EOF. No Content-Length required.
	cr := &countingReader{r: body}
	uploader := manager.NewUploader(s.client, func(u *manager.Uploader) {
		// 10 MiB parts strike a balance between request count (low
		// enough for small DBs to finish in one or two parts) and
		// memory (each in-flight part buffers up to PartSize bytes
		// in the Uploader). Default concurrency (5) is fine.
		u.PartSize = 10 * 1024 * 1024
	})
	_, err := uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(path),
		Body:   cr,
	})
	if err != nil {
		return 0, fmt.Errorf("s3 put %q: %w", path, err)
	}
	return cr.n, nil
}

func (s *S3Store) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	path = strings.TrimPrefix(path, "/")
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(path),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if errorsAs(err, &nsk) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("s3 get %q: %w", path, err)
	}
	return out.Body, nil
}

// Presign returns a GET URL valid for `expires`. Uses the standard
// AWS signature v4 PresignGetObject flow — works against AWS S3,
// MinIO, R2, DO Spaces, Yandex Object Storage uniformly.
func (s *S3Store) Presign(ctx context.Context, path string, expires time.Duration) (string, error) {
	path = strings.TrimPrefix(path, "/")
	ps := s3.NewPresignClient(s.client)
	req, err := ps.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(path),
	}, s3.WithPresignExpires(expires))
	if err != nil {
		return "", fmt.Errorf("s3 presign %q: %w", path, err)
	}
	return req.URL, nil
}

// List enumerates every object under prefix, following
// ListObjectsV2's ContinuationToken until IsTruncated is false.
//
// LIVE-CAUGHT 2026-11-24 on sibling sc-component-observatory's
// dry-run colocated→goembedb migration: it answered
// `s3_count: 1000` — exactly AWS's default page cap. The earlier
// shape issued ONE ListObjectsV2 call and returned `out.Contents`
// as the complete listing; `out.IsTruncated` was never read and
// `NextContinuationToken` was never threaded, so any prefix with
// ≥1001 objects silently truncated to its first page. The same
// defect was byte-identical here (meta.colocatedStore walks `_meta/`
// via this `List`), and the ColocatedMigrationConfig in sibling
// `goembedbadmin/colocated_source.go` derived both `SourceCount`
// AND `MigratedCount` from that truncated page — so a cutover on a
// deployment past the 1000-key boundary reported `verified: true`
// against `truncated >= truncated`, the operator flipped
// `USE_GOEMBEDB=1`, and the scheduler's next tick found orphaned
// tenants on the far side.
//
// Independent of go-embedb, the same `List` answers the SPA
// list-runs query and the scheduler's "has this tenant been backed
// up recently?" probe — the loss of visibility applies there too.
//
// Shape kept visibly identical to the fix sc-components is landing
// in observatory so the next reader sees the trap once, in the same
// spot, in both repos.
func (s *S3Store) List(ctx context.Context, prefix string) ([]StoredObject, error) {
	prefix = strings.TrimPrefix(prefix, "/")
	var (
		result            []StoredObject
		continuationToken *string
	)
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.cfg.Bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return nil, fmt.Errorf("s3 list %q: %w", prefix, err)
		}
		for _, o := range out.Contents {
			so := StoredObject{Path: aws.ToString(o.Key)}
			if o.Size != nil {
				so.Size = *o.Size
			}
			if o.LastModified != nil {
				so.ModifiedAt = *o.LastModified
			}
			result = append(result, so)
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			return result, nil
		}
		next := aws.ToString(out.NextContinuationToken)
		if next == "" {
			// Defensive: a `truncated=true` reply with no token means
			// the backend cut pagination mid-walk — surface rather
			// than loop forever on the same page.
			return nil, fmt.Errorf("s3 list %q: truncated response without NextContinuationToken", prefix)
		}
		continuationToken = aws.String(next)
	}
}

func (s *S3Store) Delete(ctx context.Context, path string) error {
	path = strings.TrimPrefix(path, "/")
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(path),
	})
	if err != nil {
		return fmt.Errorf("s3 delete %q: %w", path, err)
	}
	return nil
}

// EnsureBucket creates the bucket if it doesn't exist. Used by the
// smoke rig to idempotently set up MinIO; NOT used in production
// (operator owns the bucket via SC's s3-bucket resource).
func (s *S3Store) EnsureBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.cfg.Bucket)})
	if err == nil {
		return nil
	}
	_, err = s.client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(s.cfg.Bucket),
	})
	if err != nil {
		var already *types.BucketAlreadyOwnedByYou
		if ok := errorsAs(err, &already); ok {
			return nil
		}
		return fmt.Errorf("s3 ensure-bucket %q: %w", s.cfg.Bucket, err)
	}
	return nil
}

// countingReader wraps an io.Reader to track bytes passed through.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// errorsAs is a thin wrapper around errors.As with explicit ptr-to-ptr
// shape, kept here to avoid an import of errors just for one call.
func errorsAs(err error, target any) bool {
	// Simple nil-check; AWS SDK v2 returns typed errors.
	return err != nil && target != nil && asImpl(err, target)
}

// asImpl delegates to the stdlib; separated so the (nil, nil) guard
// above stays readable.
func asImpl(err error, target any) bool { return stdErrorsAs(err, target) }
