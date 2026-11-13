// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package storage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// GCSStore is the Google Cloud Storage archive backend. Slice 1e.4f.
//
// Auth model:
//
//   - Default: Application Default Credentials (ADC). Works on GKE /
//     GCE / Cloud Run via Workload Identity, locally via
//     `gcloud auth application-default login`.
//   - Override: GCSConfig.CredentialsFile → path to a service-account
//     JSON. Used by the storage client AND by SignedURL for local
//     signing (private key embedded in the JSON).
//
// Presign (SignedURL) requires something that can sign bytes. Three
// paths:
//
//  1. Explicit service-account JSON in CredentialsFile — the client
//     constructor extracts GoogleAccessID + PrivateKey from the JSON
//     and uses them directly for every SignedURL call.
//  2. ADC on GCE/GKE/Cloud Run with an attached service account —
//     fallback uses the IAM SignBlob RPC (iamcredentials.googleapis.com)
//     against the attached SA's email, read from GCE metadata.
//  3. ADC with user-credentials only (local `gcloud auth`) — Presign
//     refuses with a clear error asking for a service-account file.
//     The Store's other methods still work.

// GCSConfig bundles the knobs. CredentialsFile is a path to a GCP
// service-account JSON; empty uses Application Default Credentials
// (Workload Identity on GKE / GCE / Cloud Run, user ADC locally).
type GCSConfig struct {
	Bucket          string
	CredentialsFile string
}

// GCSStore implements Store against a single GCS bucket.
type GCSStore struct {
	cfg    GCSConfig
	client *gcs.Client
	bucket *gcs.BucketHandle

	// Signer state — populated eagerly when CredentialsFile is set,
	// lazily resolved from GCE metadata otherwise. presignUnavailable
	// carries the reason to surface in Presign errors when neither
	// path is viable.
	signerEmail        string
	signerPrivateKey   []byte
	presignUnavailable error
}

// serviceAccountFile is the subset of the GCP service-account JSON we
// need to drive SignedURL. Fields match the Google Cloud format.
type serviceAccountFile struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
}

// NewGCSStore builds a GCSStore + probes the bucket so a bad config
// fails at construction time (matches the S3Store pattern —
// configuration surfaces at /healthz rather than at first call).
func NewGCSStore(ctx context.Context, cfg GCSConfig) (*GCSStore, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("storage.NewGCSStore: Bucket is required")
	}
	opts := []option.ClientOption{}
	if cfg.CredentialsFile != "" {
		opts = append(opts, option.WithCredentialsFile(cfg.CredentialsFile))
	}
	client, err := gcs.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("storage.NewGCSStore: new client: %w", err)
	}
	bucket := client.Bucket(cfg.Bucket)
	if _, err := bucket.Attrs(ctx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("storage.NewGCSStore: bucket %q attrs: %w", cfg.Bucket, err)
	}
	s := &GCSStore{cfg: cfg, client: client, bucket: bucket}

	// Resolve the signer identity. Precedence: CredentialsFile JSON
	// (local signing) → GCE metadata (IAM SignBlob fallback) → unavailable.
	if cfg.CredentialsFile != "" {
		if email, key, err := readServiceAccountKey(cfg.CredentialsFile); err == nil {
			s.signerEmail = email
			s.signerPrivateKey = []byte(key)
		} else {
			// Non-SA key file (e.g. user ADC JSON) — keep the client
			// working, record the reason so Presign can surface it.
			s.presignUnavailable = fmt.Errorf("CredentialsFile %q is not a service-account JSON (no client_email/private_key): %w", cfg.CredentialsFile, err)
		}
	} else if metadata.OnGCE() {
		// ADC on GCE/GKE/Cloud Run: resolve the attached SA email so
		// Presign can call IAM SignBlob against it.
		if email, err := metadata.Email(""); err == nil {
			s.signerEmail = email
			// Leave signerPrivateKey empty — presign() detects that +
			// routes through iamcredentials.SignBlob instead.
		} else {
			s.presignUnavailable = fmt.Errorf("Workload Identity detected but GCE metadata email lookup failed: %w", err)
		}
	} else {
		s.presignUnavailable = errors.New("Presign requires a service-account JSON (set storage.credentials) or Workload Identity on GCE/GKE/Cloud Run — user-credentials ADC can't sign URLs")
	}
	return s, nil
}

// Close releases the underlying storage client. Callers should invoke
// it on service shutdown to free the keep-alive connections.
func (s *GCSStore) Close() error {
	if s.client == nil {
		return nil
	}
	return s.client.Close()
}

// Put streams body to gs://<bucket>/<path>. Returns bytes written (from
// the Writer's Attrs) when the backend reports it; falls back to a
// counting Reader when it doesn't.
func (s *GCSStore) Put(ctx context.Context, path string, body io.Reader) (int64, error) {
	path = strings.TrimPrefix(path, "/")
	w := s.bucket.Object(path).NewWriter(ctx)
	cr := &countingReader{r: body}
	if _, err := io.Copy(w, cr); err != nil {
		_ = w.Close()
		return 0, fmt.Errorf("gcs put %q: copy: %w", path, err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("gcs put %q: close: %w", path, err)
	}
	if attrs := w.Attrs(); attrs != nil && attrs.Size > 0 {
		return attrs.Size, nil
	}
	return cr.n, nil
}

// Get returns a ReadCloser on gs://<bucket>/<path>. Missing object →
// ErrNotFound.
func (s *GCSStore) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	path = strings.TrimPrefix(path, "/")
	r, err := s.bucket.Object(path).NewReader(ctx)
	if err != nil {
		if errors.Is(err, gcs.ErrObjectNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("gcs get %q: %w", path, err)
	}
	return r, nil
}

// List enumerates objects under prefix. Unlike S3Store, GCS lists
// without an implicit pagination cap; the `iterator.Done` sentinel
// terminates the loop.
func (s *GCSStore) List(ctx context.Context, prefix string) ([]StoredObject, error) {
	prefix = strings.TrimPrefix(prefix, "/")
	it := s.bucket.Objects(ctx, &gcs.Query{Prefix: prefix})
	out := []StoredObject{}
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("gcs list %q: %w", prefix, err)
		}
		out = append(out, StoredObject{
			Path:       attrs.Name,
			Size:       attrs.Size,
			ModifiedAt: attrs.Updated,
		})
	}
}

// Delete removes the object. Missing → ErrNotFound.
func (s *GCSStore) Delete(ctx context.Context, path string) error {
	path = strings.TrimPrefix(path, "/")
	err := s.bucket.Object(path).Delete(ctx)
	if err == nil {
		return nil
	}
	if errors.Is(err, gcs.ErrObjectNotExist) {
		return ErrNotFound
	}
	return fmt.Errorf("gcs delete %q: %w", path, err)
}

// Presign returns a V4 signed URL valid for `expires`. Signing path:
//
//  1. CredentialsFile with private key → sign locally.
//  2. ADC on GCE/GKE → sign via iamcredentials.SignBlob against the
//     attached SA.
//  3. User ADC — refuse with a clear error.
func (s *GCSStore) Presign(ctx context.Context, path string, expires time.Duration) (string, error) {
	path = strings.TrimPrefix(path, "/")
	if expires <= 0 {
		expires = 5 * time.Minute
	}
	if s.presignUnavailable != nil {
		return "", fmt.Errorf("gcs presign: %w", s.presignUnavailable)
	}
	opts := &gcs.SignedURLOptions{
		Method:         http.MethodGet,
		Expires:        time.Now().Add(expires),
		Scheme:         gcs.SigningSchemeV4,
		GoogleAccessID: s.signerEmail,
	}
	if len(s.signerPrivateKey) > 0 {
		opts.PrivateKey = s.signerPrivateKey
	} else {
		// Workload-Identity path: route each sign request through the
		// IAM SignBlob RPC against the attached SA. One sign call per
		// Presign — fine at operator-UI cadence.
		opts.SignBytes = func(b []byte) ([]byte, error) {
			return iamSignBlob(ctx, s.signerEmail, b)
		}
	}
	return s.bucket.SignedURL(path, opts)
}

// readServiceAccountKey extracts client_email + private_key from a
// GCP service-account JSON file. Returns a typed error when the file
// is a different credential shape (user ADC, external account, etc.)
// so NewGCSStore can surface a clear Presign-unavailable reason.
func readServiceAccountKey(path string) (email, privateKey string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read credentials file: %w", err)
	}
	var sa serviceAccountFile
	if err := json.Unmarshal(b, &sa); err != nil {
		return "", "", fmt.Errorf("parse credentials JSON: %w", err)
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return "", "", errors.New("credentials file has no client_email / private_key fields")
	}
	return sa.ClientEmail, sa.PrivateKey, nil
}

// iamSignBlob calls iamcredentials.projects.serviceAccounts.signBlob
// on the operator's behalf to produce a signature for `blob`. Used by
// Presign when the service runs under Workload Identity (no local
// private key, but the attached SA has `roles/iam.serviceAccountTokenCreator`
// — or `roles/iam.serviceAccountTokenCreator` on itself for self-signing).
func iamSignBlob(ctx context.Context, saEmail string, blob []byte) ([]byte, error) {
	svc, err := iamcredentials.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("iamcredentials new service: %w", err)
	}
	name := "projects/-/serviceAccounts/" + saEmail
	resp, err := svc.Projects.ServiceAccounts.SignBlob(name, &iamcredentials.SignBlobRequest{
		Payload: toBase64(blob),
	}).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("iam signBlob: %w", err)
	}
	return fromBase64(resp.SignedBlob)
}

func toBase64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

func fromBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
