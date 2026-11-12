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
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Store is the backup archive store. Implementations: S3Store (prod +
// smoke via MinIO). An in-memory one for unit tests lives alongside.
type Store interface {
	Put(ctx context.Context, path string, body io.Reader) (sizeBytes int64, err error)
	Get(ctx context.Context, path string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]StoredObject, error)
	Delete(ctx context.Context, path string) error
}

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
	var ioBody io.Reader = body
	var declaredSize int64 = -1
	if br, ok := body.(*bytes.Reader); ok {
		declaredSize = int64(br.Len())
		ioBody = br // SDK will see io.ReadSeeker + Content-Length
	} else {
		ioBody = &countingReader{r: body}
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(path),
		Body:   ioBody,
	})
	if err != nil {
		return 0, fmt.Errorf("s3 put %q: %w", path, err)
	}
	if declaredSize >= 0 {
		return declaredSize, nil
	}
	return ioBody.(*countingReader).n, nil
}

func (s *S3Store) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	path = strings.TrimPrefix(path, "/")
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(path),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 get %q: %w", path, err)
	}
	return out.Body, nil
}

func (s *S3Store) List(ctx context.Context, prefix string) ([]StoredObject, error) {
	prefix = strings.TrimPrefix(prefix, "/")
	out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.cfg.Bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 list %q: %w", prefix, err)
	}
	result := make([]StoredObject, 0, len(out.Contents))
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
	return result, nil
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
