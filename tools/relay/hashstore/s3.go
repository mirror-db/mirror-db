package hashstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// s3API is the slice of the S3 client the backend uses. Keeping it an interface
// lets the shared-storage logic be tested without a live RustFS.
type s3API interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// s3Backend stores bodies in a shared RustFS bucket. One body object is keyed by
// SHA-256; two tiny pointer objects make the same body discoverable by either
// digest, which is what lets another relay resolve a SHA-1 hit without a local
// index entry.
type s3Backend struct {
	api    s3API
	bucket string
	prefix string
}

func newS3Backend(api s3API, cfg S3Config) *s3Backend {
	return &s3Backend{api: api, bucket: cfg.Bucket, prefix: cfg.Prefix}
}

func (b *s3Backend) key(parts ...string) string {
	return path.Join(append([]string{b.prefix}, parts...)...)
}

// objectKey shards every object by the first two hex chars and the next two, the
// same way local bodies are laid out. A flat prefix would put every object in one
// giant directory for the object store to index.
func (b *s3Backend) objectKey(kind objectKind, digest string) string {
	if len(digest) < 4 {
		return ""
	}
	switch kind {
	case bodyObject:
		return b.key("blobs", "sha256", digest[0:2], digest[2:4], digest)
	case sha1AliasObject:
		return b.key("aliases", "sha1", digest[0:2], digest[2:4], digest)
	default:
		return b.key("aliases", "sha256", digest[0:2], digest[2:4], digest)
	}
}

func (b *s3Backend) bodyKey(sha256 string) string { return b.objectKey(bodyObject, sha256) }

func (b *s3Backend) aliasSHA1Key(sha1 string) string { return b.objectKey(sha1AliasObject, sha1) }

func (b *s3Backend) aliasSHA256Key(sha256 string) string {
	return b.objectKey(sha256AliasObject, sha256)
}

// listPrefix is the only prefix the store ever lists: adding the trailing slash
// keeps sibling prefixes (relay-other/...) out of the scan.
func (b *s3Backend) listPrefix() string {
	if b.prefix == "" {
		return ""
	}
	return b.prefix + "/"
}

// parseKey maps a listed bucket key back to the object it names. Anything that is
// not one of the three sharded shapes is foreign: prune counts it, never deletes
// it.
func (b *s3Backend) parseKey(key string) (objectRef, bool) {
	rel := key
	if prefix := b.listPrefix(); prefix != "" {
		if !strings.HasPrefix(key, prefix) {
			return objectRef{}, false
		}
		rel = strings.TrimPrefix(key, prefix)
	}
	parts := strings.Split(rel, "/")
	if len(parts) != 5 {
		return objectRef{}, false
	}

	var (
		kind objectKind
		algo Algorithm
	)
	switch {
	case parts[0] == "blobs" && parts[1] == string(SHA256):
		kind, algo = bodyObject, SHA256
	case parts[0] == "aliases" && parts[1] == string(SHA1):
		kind, algo = sha1AliasObject, SHA1
	case parts[0] == "aliases" && parts[1] == string(SHA256):
		kind, algo = sha256AliasObject, SHA256
	default:
		return objectRef{}, false
	}

	digest, err := NormalizeDigest(algo, parts[4])
	if err != nil || parts[2] != digest[0:2] || parts[3] != digest[2:4] {
		return objectRef{}, false
	}
	return objectRef{Kind: kind, Digest: digest}, true
}

func (b *s3Backend) put(ctx context.Context, c Content, tmpPath string) error {
	f, err := os.Open(tmpPath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = b.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(b.bodyKey(c.SHA256)),
		Body:          f,
		ContentLength: aws.Int64(c.Size),
	})
	if err != nil {
		return fmt.Errorf("put body: %w", err)
	}
	// Pointers are ~64 bytes and are never counted against content capacity.
	for _, ptr := range []struct{ key, value string }{
		{b.aliasSHA1Key(c.SHA1), c.SHA256},
		{b.aliasSHA256Key(c.SHA256), c.SHA1},
	} {
		if _, err := b.api.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(b.bucket),
			Key:           aws.String(ptr.key),
			Body:          strings.NewReader(ptr.value),
			ContentLength: aws.Int64(int64(len(ptr.value))),
		}); err != nil {
			// Do not leave behind a body this instance will never index (the
			// shared bucket is never LISTed, so nothing would find it again).
			_ = b.delete(ctx, c.SHA256, c.SHA1)
			return fmt.Errorf("put pointer %s: %w", ptr.key, err)
		}
	}
	return nil
}

func (b *s3Backend) open(ctx context.Context, sha256 string) (io.ReadCloser, int64, error) {
	key := b.bodyKey(sha256)
	out, err := b.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			// A peer may delete a shared object at any moment: a miss, not an
			// error.
			return nil, 0, ErrNotFound
		}
		return nil, 0, fmt.Errorf("get %s: %w", key, err)
	}
	return out.Body, aws.ToInt64(out.ContentLength), nil
}

func (b *s3Backend) stat(ctx context.Context, sha256 string) (int64, bool, error) {
	key := b.bodyKey(sha256)
	out, err := b.api.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("head %s: %w", key, err)
	}
	return aws.ToInt64(out.ContentLength), true, nil
}

// isS3NotFound reports whether an S3 error means "this key does not exist".
// Everything else (AccessDenied, timeouts, 5xx) is a real error that must not
// invalidate the local index: one denied read would otherwise evict accounting
// for a body that is still there.
func isS3NotFound(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode() {
	// GET returns NoSuchKey; HEAD has no body and reports NotFound. Some
	// S3-compatible servers literally use "404".
	case "NoSuchKey", "NotFound", "404":
		return true
	}
	return false
}

func (b *s3Backend) delete(ctx context.Context, sha256, sha1 string) error {
	keys := []string{b.bodyKey(sha256), b.aliasSHA256Key(sha256)}
	// A record may carry no SHA-1 alias (an adopted object without a pointer). An
	// empty alias must not become an empty object key: delete what is known and
	// leave the unknown pointer behind.
	if key := b.aliasSHA1Key(sha1); key != "" {
		keys = append(keys, key)
	}

	var firstErr error
	for _, key := range keys {
		if _, err := b.api.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(b.bucket),
			Key:    aws.String(key),
		}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// alias reads a digest pointer. The pointer body is a hex digest, so the read is
// capped instead of trusting the object size.
func (b *s3Backend) alias(ctx context.Context, algo Algorithm, digest string) (string, bool) {
	key := b.aliasSHA1Key(digest)
	if algo == SHA256 {
		key = b.aliasSHA256Key(digest)
	}
	out, err := b.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return "", false
	}
	defer out.Body.Close()
	data, err := io.ReadAll(io.LimitReader(out.Body, 128))
	if err != nil {
		return "", false
	}
	// A shared bucket is writable by every peer: never let an unchecked pointer
	// value become part of a key. The alias must be the other digest, in hex.
	want := SHA256
	if algo == SHA256 {
		want = SHA1
	}
	value, err := NormalizeDigest(want, strings.TrimSpace(string(data)))
	if err != nil {
		return "", false
	}
	return value, true
}

func (b *s3Backend) bodies(context.Context) (map[string]struct{}, error) {
	// Never LIST a shared bucket for reconciliation: it belongs to every relay,
	// and the mirror only manages the objects its own index knows about.
	return nil, nil
}

// listObjects pages through the configured prefix. This is the only full listing
// in the store, and only the offline prune command reaches it.
func (b *s3Backend) listObjects(ctx context.Context, fn func(objectRef) error) error {
	paginator := s3.NewListObjectsV2Paginator(b.api, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(b.listPrefix()),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, obj := range page.Contents {
			if err := ctx.Err(); err != nil {
				return err
			}
			ref, ok := b.parseKey(aws.ToString(obj.Key))
			if !ok {
				ref = objectRef{Kind: foreignObject}
			}
			if err := fn(ref); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *s3Backend) deleteObject(ctx context.Context, ref objectRef) error {
	key := b.objectKey(ref.Kind, ref.Digest)
	if key == "" {
		return fmt.Errorf("hashstore: %s has no object key", ref.describe())
	}
	_, err := b.api.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	return err
}

func (b *s3Backend) shared() bool { return true }

// newS3Client builds the RustFS-recommended configuration: AWS SDK v2,
// path-style addressing, and checksums only where the API requires them (the
// SDK default adds them everywhere, which older S3 servers reject).
func newS3Client(cfg S3Config) (s3API, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = true
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	}), nil
}
