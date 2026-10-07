package backups

import (
	"context"
	"fmt"
	"os"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/peaceful/cloud-console/internal/config"
)

// s3Target uploads and downloads backup archives on S3-compatible storage.
type s3Target struct {
	client *minio.Client
	bucket string
	prefix string
}

// newS3Target builds a client for any S3-compatible endpoint (AWS S3, MinIO,
// Backblaze B2, Wasabi, ...).
func newS3Target(cfg *config.Config) (*s3Target, error) {
	client, err := minio.New(cfg.S3Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		Secure: cfg.S3UseSSL,
		Region: cfg.S3Region,
	})
	if err != nil {
		return nil, fmt.Errorf("create s3 client: %w", err)
	}

	return &s3Target{client: client, bucket: cfg.S3Bucket, prefix: cfg.S3Prefix}, nil
}

// EnsureBucket creates the bucket when it does not exist yet.
func (t *s3Target) EnsureBucket(ctx context.Context) error {
	exists, err := t.client.BucketExists(ctx, t.bucket)
	if err != nil {
		return fmt.Errorf("check bucket: %w", err)
	}
	if exists {
		return nil
	}
	if err := t.client.MakeBucket(ctx, t.bucket, minio.MakeBucketOptions{Region: regionOf(t)}); err != nil {
		return fmt.Errorf("create bucket: %w", err)
	}
	return nil
}

// Key builds the object key for an instance backup.
func (t *s3Target) Key(instanceName, backupName string) string {
	if t.prefix == "" {
		return fmt.Sprintf("%s/%s.tar.gz", instanceName, backupName)
	}
	return fmt.Sprintf("%s/%s/%s.tar.gz", t.prefix, instanceName, backupName)
}

// Upload sends a local archive to the bucket.
func (t *s3Target) Upload(ctx context.Context, key, path string) (int64, error) {
	info, err := t.client.FPutObject(ctx, t.bucket, key, path, minio.PutObjectOptions{
		ContentType: "application/gzip",
	})
	if err != nil {
		return 0, fmt.Errorf("upload %s: %w", key, err)
	}
	return info.Size, nil
}

// Download fetches an archive stored in the bucket.
func (t *s3Target) Download(ctx context.Context, key, path string) error {
	if err := t.client.FGetObject(ctx, t.bucket, key, path, minio.GetObjectOptions{}); err != nil {
		return fmt.Errorf("download %s: %w", key, err)
	}
	return nil
}

// Delete removes an archive from the bucket. A missing object is not an error.
func (t *s3Target) Delete(ctx context.Context, key string) error {
	if err := t.client.RemoveObject(ctx, t.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

// Stat returns an object's size.
func (t *s3Target) Stat(ctx context.Context, key string) (int64, error) {
	info, err := t.client.StatObject(ctx, t.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return 0, err
	}
	return info.Size, nil
}

// Exists reports whether an object is present.
func (t *s3Target) Exists(ctx context.Context, key string) bool {
	_, err := t.client.StatObject(ctx, t.bucket, key, minio.StatObjectOptions{})
	return err == nil
}

func regionOf(t *s3Target) string {
	// The client already carries the configured region; re-query it cheaply.
	if loc, err := t.client.GetBucketLocation(context.Background(), t.bucket); err == nil && loc != "" {
		return loc
	}
	return ""
}

// removeLocal deletes a file, ignoring a missing file.
func removeLocal(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
