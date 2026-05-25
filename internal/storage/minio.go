package storage

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
)

// ObjectStore wraps a MinIO / S3-compatible client scoped to a single bucket.
type ObjectStore struct {
	client *minio.Client
	bucket string
}

// NewObjectStore builds a client, ensures the target bucket exists, and
// returns the wrapper.
func NewObjectStore(ctx context.Context, endpoint, accessKey, secretKey, bucket string, useSSL bool) (*ObjectStore, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("minio client: %w", err)
	}
	s := &ObjectStore{client: client, bucket: bucket}
	if err := s.ensureBucket(ctx); err != nil {
		return nil, err
	}
	// Best-effort: failure here doesn't block startup — the sweeper is the
	// primary cleanup path (SPEC §6); the lifecycle rule is only a backstop
	// for objects the sweeper misses.
	if err := s.ensureLifecycleBackstop(ctx); err != nil {
		slog.Warn("set bucket lifecycle backstop failed", "bucket", bucket, "error", err)
	}
	return s, nil
}

// ensureLifecycleBackstop installs a 1-day expiration rule on the bucket
// (SPEC §6). Any QR object the sweeper missed gets reaped after a day,
// independent of the worker. SetBucketLifecycle replaces the existing
// configuration, so this is idempotent and safe to run on every boot.
func (s *ObjectStore) ensureLifecycleBackstop(ctx context.Context) error {
	cfg := lifecycle.NewConfiguration()
	cfg.Rules = []lifecycle.Rule{{
		ID:         "shortlink-qr-backstop",
		Status:     "Enabled",
		Expiration: lifecycle.Expiration{Days: lifecycle.ExpirationDays(1)},
		// Empty filter == match every object in the bucket. Day-granular
		// is fine because the sweeper handles the ~15-minute deadline.
		RuleFilter: lifecycle.Filter{Prefix: ""},
	}}
	if err := s.client.SetBucketLifecycle(ctx, s.bucket, cfg); err != nil {
		return fmt.Errorf("set lifecycle on %q: %w", s.bucket, err)
	}
	return nil
}

func (s *ObjectStore) ensureBucket(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("check bucket %q: %w", s.bucket, err)
	}
	if exists {
		return nil
	}
	if err := s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{}); err != nil {
		// Another process may have created the bucket concurrently — re-check
		// before treating this as a failure.
		if exists, chkErr := s.client.BucketExists(ctx, s.bucket); chkErr == nil && exists {
			return nil
		}
		return fmt.Errorf("create bucket %q: %w", s.bucket, err)
	}
	return nil
}

// Upload stores data under key with the given content type.
func (s *ObjectStore) Upload(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("upload %q: %w", key, err)
	}
	return nil
}

// Stat returns the size in bytes of the object at key.
func (s *ObjectStore) Stat(ctx context.Context, key string) (int64, error) {
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return 0, fmt.Errorf("stat %q: %w", key, err)
	}
	return info.Size, nil
}

// PresignGet returns a time-limited download URL for the object at key.
func (s *ObjectStore) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := s.client.PresignedGetObject(ctx, s.bucket, key, ttl, url.Values{})
	if err != nil {
		return "", fmt.Errorf("presign %q: %w", key, err)
	}
	return u.String(), nil
}

// Delete removes the object at key.
func (s *ObjectStore) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete %q: %w", key, err)
	}
	return nil
}

// DeleteMany removes a batch of objects in a single multi-object-delete
// request to S3/MinIO (one HTTP round-trip for up to 1000 objects per chunk).
// Returns the per-key errors collected from the response stream; the slice is
// empty when every delete succeeded. Empty input is a no-op.
func (s *ObjectStore) DeleteMany(ctx context.Context, keys []string) []error {
	if len(keys) == 0 {
		return nil
	}
	objectsCh := make(chan minio.ObjectInfo, len(keys))
	for _, k := range keys {
		objectsCh <- minio.ObjectInfo{Key: k}
	}
	close(objectsCh)
	var errs []error
	for rerr := range s.client.RemoveObjects(ctx, s.bucket, objectsCh, minio.RemoveObjectsOptions{}) {
		if rerr.Err != nil {
			errs = append(errs, fmt.Errorf("delete %q: %w", rerr.ObjectName, rerr.Err))
		}
	}
	return errs
}
