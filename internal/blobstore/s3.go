package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
)

// S3Config holds parameters for connecting to S3 or an S3-compatible store.
type S3Config struct {
	Bucket         string
	Region         string
	Endpoint       string
	AccessKey      string
	SecretKey      string
	Prefix         string // key prefix inside the bucket (no trailing slash)
	ForcePathStyle bool
	// TempDir is used for upload staging. Defaults to os.TempDir(). Set this to
	// a path on the same volume as your blob data if the OS temp partition is too
	// small for large image layers.
	TempDir string
}

type S3Store struct {
	client  *s3.Client
	bucket  string
	prefix  string
	tempDir string
	logger  *slog.Logger
}

// NewS3 creates an S3Store. If AccessKey/SecretKey are empty, the SDK falls back
// to the standard credential chain (env vars, ~/.aws/credentials, EC2 metadata).
func NewS3(cfg S3Config, logger *slog.Logger) (*S3Store, error) {
	loadCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(loadCtx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	clientOpts := []func(*s3.Options){
		func(o *s3.Options) { o.UsePathStyle = cfg.ForcePathStyle },
	}
	if cfg.Endpoint != "" {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		})
	}

	tempDir := cfg.TempDir
	if tempDir == "" {
		tempDir = os.TempDir()
	}

	return &S3Store{
		client:  s3.NewFromConfig(awsCfg, clientOpts...),
		bucket:  cfg.Bucket,
		prefix:  strings.TrimSuffix(cfg.Prefix, "/"),
		tempDir: tempDir,
		logger:  logger,
	}, nil
}

func (s *S3Store) Put(ctx context.Context, r io.Reader, expectedDigest string) (string, int64, error) {
	if expectedDigest != "" {
		if err := validate.Digest(expectedDigest); err != nil {
			return "", 0, fmt.Errorf("invalid expected digest: %w", err)
		}
	}

	tmp, err := os.CreateTemp(s.tempDir, "apoci-s3-upload-*")
	if err != nil {
		return "", 0, fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		return "", 0, fmt.Errorf("writing blob: %w", err)
	}

	computed := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if expectedDigest != "" && computed != expectedDigest {
		return "", 0, fmt.Errorf("digest mismatch: expected %s, got %s", expectedDigest, computed)
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", 0, fmt.Errorf("seeking temp file: %w", err)
	}

	key := s.keyForDigest(computed)
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          tmp,
		ContentLength: aws.Int64(size),
	})
	if err != nil {
		return "", 0, fmt.Errorf("uploading blob to S3: %w", err)
	}

	s.logger.Debug("blob stored in S3", "digest", computed, "size", size, "key", key)
	return computed, size, nil
}

func (s *S3Store) Open(ctx context.Context, digest string) (io.ReadSeekCloser, int64, error) {
	if err := validate.Digest(digest); err != nil {
		return nil, 0, err
	}

	key := s.keyForDigest(digest)
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, 0, ErrBlobNotFound
		}
		return nil, 0, fmt.Errorf("headobject for blob: %w", err)
	}

	size := aws.ToInt64(out.ContentLength)
	return &s3ReadSeekCloser{
		client: s.client,
		bucket: s.bucket,
		key:    key,
		size:   size,
		ctx:    ctx,
		logger: s.logger,
	}, size, nil
}

func (s *S3Store) Exists(ctx context.Context, digest string) (bool, error) {
	if err := validate.Digest(digest); err != nil {
		return false, nil
	}
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.keyForDigest(digest)),
	})
	if err == nil {
		return true, nil
	}
	if isS3NotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("checking blob existence: %w", err)
}

func (s *S3Store) Delete(ctx context.Context, digest string) error {
	if err := validate.Digest(digest); err != nil {
		return err
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.keyForDigest(digest)),
	})
	if err != nil && !isS3NotFound(err) {
		return fmt.Errorf("deleting blob from S3: %w", err)
	}
	return nil
}

func (s *S3Store) ListDigests(ctx context.Context) ([]string, error) {
	prefix := "sha256/"
	if s.prefix != "" {
		prefix = s.prefix + "/" + prefix
	}

	var digests []string
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing S3 objects: %w", err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			parts := strings.Split(key, "/")
			if len(parts) == 0 {
				continue
			}
			hash := parts[len(parts)-1]
			if !hexDigestRe.MatchString(hash) {
				continue
			}
			digests = append(digests, "sha256:"+hash)
		}
	}
	return digests, nil
}

func (s *S3Store) keyForDigest(digest string) string {
	hash := strings.TrimPrefix(digest, "sha256:")
	sub := hash[:2] // always safe: validate.Digest guarantees 64 hex chars
	if s.prefix != "" {
		return s.prefix + "/sha256/" + sub + "/" + hash
	}
	return "sha256/" + sub + "/" + hash
}

func isS3NotFound(err error) bool {
	if err == nil {
		return false
	}
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket":
			return true
		}
	}
	return false
}

type s3ReadSeekCloser struct {
	client   *s3.Client
	bucket   string
	key      string
	size     int64
	offset   int64
	body     io.ReadCloser
	fetchErr error
	ctx      context.Context
	logger   *slog.Logger
}

func (r *s3ReadSeekCloser) Read(p []byte) (int, error) {
	if r.fetchErr != nil {
		return 0, r.fetchErr
	}
	if r.body == nil {
		if err := r.fetch(); err != nil {
			r.fetchErr = err
			return 0, err
		}
	}
	n, err := r.body.Read(p)
	r.offset += int64(n)
	return n, err
}

func (r *s3ReadSeekCloser) Seek(offset int64, whence int) (int64, error) {
	var newOffset int64
	switch whence {
	case io.SeekStart:
		newOffset = offset
	case io.SeekCurrent:
		newOffset = r.offset + offset
	case io.SeekEnd:
		newOffset = r.size + offset
	default:
		return 0, fmt.Errorf("invalid whence: %d", whence)
	}
	if newOffset < 0 || newOffset > r.size {
		return 0, fmt.Errorf("seek offset %d out of range [0, %d]", newOffset, r.size)
	}
	if newOffset == r.offset && r.body != nil {
		return newOffset, nil
	}
	if r.body != nil {
		if err := r.body.Close(); err != nil {
			r.logger.Warn("s3: error closing blob stream on seek", "key", r.key, "error", err)
		}
		r.body = nil
	}
	r.fetchErr = nil
	r.offset = newOffset
	return newOffset, nil
}

func (r *s3ReadSeekCloser) Close() error {
	if r.body != nil {
		err := r.body.Close()
		r.body = nil
		return err
	}
	return nil
}

func (r *s3ReadSeekCloser) fetch() error {
	input := &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(r.key),
	}
	if r.offset > 0 {
		input.Range = aws.String(fmt.Sprintf("bytes=%d-", r.offset))
	}
	out, err := r.client.GetObject(r.ctx, input)
	if err != nil {
		if isS3NotFound(err) {
			return ErrBlobNotFound
		}
		return fmt.Errorf("fetching blob from S3: %w", err)
	}
	r.body = out.Body
	return nil
}
