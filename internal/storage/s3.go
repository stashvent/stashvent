package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// S3Config configures access to S3-compatible object storage.
type S3Config struct {
	Endpoint       string // server → storage (e.g. http://seaweedfs:8333 in Docker)
	PublicEndpoint string // signs presigned URLs; must be reachable by the CLI
	Region         string
	Bucket         string
	UsePathStyle   bool
	AccessKey      string
	SecretKey      string
}

// Objects stores encrypted payloads. Payload bytes never pass through the
// server: it only issues presigned URLs and inspects or deletes objects.
type Objects struct {
	client    *s3.Client // internal endpoint
	presigner *s3.PresignClient
	bucket    string
}

// NewObjects returns Objects for cfg.
func NewObjects(cfg S3Config) *Objects {
	newClient := func(endpoint string) *s3.Client {
		return s3.New(s3.Options{
			Region:       cfg.Region,
			BaseEndpoint: aws.String(endpoint),
			UsePathStyle: cfg.UsePathStyle,
			Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
			// The SDK's default CRC32 checksums break GCS, R2, and B2.
			// AES-GCM already detects a corrupted payload.
			RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
			ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		})
	}
	return &Objects{
		client:    newClient(cfg.Endpoint),
		presigner: s3.NewPresignClient(newClient(cfg.PublicEndpoint)),
		bucket:    cfg.Bucket,
	}
}

// Ping reports whether the bucket can be reached with the configured
// credentials. HeadBucket needs permission on the bucket itself
// (s3:ListBucket on AWS), not only on its objects. The server needs that
// permission anyway: without it, storage answers 403 instead of 404 for a
// missing object, and Size can't tell "not uploaded" from a real failure.
func (o *Objects) Ping(ctx context.Context) error {
	if _, err := o.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(o.bucket)}); err != nil {
		return fmt.Errorf("head bucket: %w", err)
	}
	return nil
}

// PresignPut returns a presigned PUT URL that only accepts exactly size bytes.
func (o *Objects) PresignPut(ctx context.Context, key string, size int64, ttl time.Duration) (string, error) {
	req, err := o.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(o.bucket),
		Key:           aws.String(key),
		ContentLength: aws.Int64(size),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presigning PUT: %w", err)
	}
	return req.URL, nil
}

// PresignGet returns a presigned GET URL for key.
func (o *Objects) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := o.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(o.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presigning GET: %w", err)
	}
	return req.URL, nil
}

// Size returns the stored size of key, or ErrObjectNotFound.
func (o *Objects) Size(ctx context.Context, key string) (int64, error) {
	out, err := o.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(o.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NotFound" || apiErr.ErrorCode() == "NoSuchKey") {
			return 0, ErrObjectNotFound
		}
		return 0, fmt.Errorf("head object: %w", err)
	}
	return aws.ToInt64(out.ContentLength), nil
}

// Delete removes key. Deleting a missing object is not an error.
func (o *Objects) Delete(ctx context.Context, key string) error {
	if _, err := o.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(o.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("delete object: %w", err)
	}
	return nil
}
