// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"strings"
	"testing"
)

// Slice 1e.4g — tests covering BackupStorage.Validate +
// BackupMetadata.Validate + the BackupConfig cross-constraint for
// Lambda + filesystem archive.

func TestBackupStorageValidate(t *testing.T) {
	cases := []struct {
		name    string
		s       BackupStorage
		wantErr string
	}{
		{"empty type", BackupStorage{}, "storage.type` is required"},
		{"s3 ok", BackupStorage{Type: "s3", Bucket: "x"}, ""},
		{"s3 missing bucket", BackupStorage{Type: "s3"}, "storage.bucket` is required"},
		{"s3 with path rejected", BackupStorage{Type: "s3", Bucket: "x", Path: "/x"}, "storage.path` is only valid"},
		{"gcs ok", BackupStorage{Type: "gcs", Bucket: "x"}, ""},
		{"gcs missing bucket", BackupStorage{Type: "gcs"}, "storage.bucket` is required"},
		{"filesystem ok", BackupStorage{Type: "filesystem", Path: "/var/sc"}, ""},
		{"filesystem missing path", BackupStorage{Type: "filesystem"}, "storage.path` is required"},
		{"filesystem with bucket", BackupStorage{Type: "filesystem", Path: "/x", Bucket: "y"}, "not valid when `storage.type: filesystem"},
		{"unknown type", BackupStorage{Type: "banana"}, "not recognised"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("Validate: unexpected err: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate: want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestBackupMetadataValidate(t *testing.T) {
	fs := &BackupStorage{Type: "filesystem", Path: "/var"}
	s3 := &BackupStorage{Type: "s3", Bucket: "x"}

	cases := []struct {
		name    string
		m       BackupMetadata
		storage *BackupStorage
		wantErr string
	}{
		{"empty type ok", BackupMetadata{}, s3, ""},
		{"colocated ok", BackupMetadata{Type: "colocated"}, s3, ""},
		{"mongo needs URI", BackupMetadata{Type: "mongo"}, s3, "metadata.uri` is required"},
		{"mongo ok", BackupMetadata{Type: "mongo", URI: "mongodb://h/"}, s3, ""},
		{"postgres ok", BackupMetadata{Type: "postgres", URI: "pg://h/"}, s3, ""},
		{"mysql ok", BackupMetadata{Type: "mysql", URI: "mysql://h/"}, s3, ""},
		{"sqlite on filesystem", BackupMetadata{Type: "sqlite"}, fs, ""},
		{"sqlite on s3 refused", BackupMetadata{Type: "sqlite"}, s3, "only valid with `storage.type: filesystem"},
		{"sqlite without storage", BackupMetadata{Type: "sqlite"}, nil, "requires a `storage:` block"},
		{"unknown type", BackupMetadata{Type: "etcd"}, s3, "not recognised"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.m.Validate(tc.storage)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("Validate: unexpected err: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate: want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestBackupConfigValidate_LambdaFilesystemRefused(t *testing.T) {
	cfg := &BackupConfig{
		DeployTo:   "lambda",
		Image:      "simplecontainer/sc-component-backup:latest",
		SigningKey: "${secret:k}",
		Worker: &BackupWorker{
			Cluster: "x", SubnetIds: []string{"s"}, SecurityGroupIds: []string{"sg"},
		},
		Storage: &BackupStorage{Type: "filesystem", Path: "/tmp"},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "incompatible with `deployTo: lambda") {
		t.Errorf("Validate: want Lambda+filesystem refusal, got %v", err)
	}
}

func TestBackupConfigValidate_NoStorageBlockStillWorks(t *testing.T) {
	// Backward compat: operators on the legacy env-var shape don't
	// declare storage{} yet; Validate must still pass for them.
	cfg := &BackupConfig{
		DeployTo:   "ecs-fargate",
		Image:      "simplecontainer/sc-component-backup:latest",
		SigningKey: "${secret:k}",
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate without storage block: %v", err)
	}
}
