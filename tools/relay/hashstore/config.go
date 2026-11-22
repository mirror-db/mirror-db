package hashstore

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/docker/go-units"
)

// Algorithm names a content digest.
type Algorithm string

const (
	SHA1   Algorithm = "sha1"
	SHA256 Algorithm = "sha256"
)

// ParseAlgorithm validates an algorithm segment of a content URL.
func ParseAlgorithm(raw string) (Algorithm, error) {
	switch Algorithm(strings.ToLower(strings.TrimSpace(raw))) {
	case SHA1:
		return SHA1, nil
	case SHA256:
		return SHA256, nil
	}
	return "", fmt.Errorf("%w: unknown algorithm %q", ErrInvalidDigest, raw)
}

// NormalizeDigest validates a hex digest for the algorithm and returns it
// lowercase. The length is fixed per algorithm, so a truncated or padded hash
// can never reach a filesystem or object-store key.
func NormalizeDigest(algo Algorithm, raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	want := 40
	if algo == SHA256 {
		want = 64
	}
	if len(s) != want {
		return "", fmt.Errorf("%w: %s digest needs %d hex chars, got %d", ErrInvalidDigest, algo, want, len(s))
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("%w: %s digest is not hexadecimal", ErrInvalidDigest, algo)
		}
	}
	return s, nil
}

// ParseByteSize parses a byte count with an optional binary unit, e.g. "128GiB"
// or "4096". It rejects zero and negative values: a misconfigured capacity must
// fail at startup, not silently disable eviction.
func ParseByteSize(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, errors.New("empty byte size")
	}
	n, err := units.RAMInBytes(s)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("byte size %q must be positive", raw)
	}
	return n, nil
}

// S3Config points the store at a shared, S3-compatible object store (RustFS).
type S3Config struct {
	Bucket   string
	Endpoint string
	Prefix   string
	Region   string
}

// Config describes one store instance. Dir is always the local working
// directory (temp files and the bbolt index); when S3 is set, bodies live in the
// bucket instead of under Dir.
type Config struct {
	Dir string
	// Capacity is the space the store may use. It only bounds the default
	// MaxSize; it is not the LRU limit.
	Capacity int64
	// MaxSize is the LRU budget and the largest single body kept. Defaults to
	// 10% of Capacity.
	MaxSize int64
	S3      *S3Config
}

// Normalize fills defaults and validates the configuration. It is idempotent:
// Open calls it, and the config package calls it too so startup fails fast on a
// bad value before any file or network work happens.
func (c *Config) Normalize() error {
	c.Dir = strings.TrimSpace(c.Dir)
	if c.Dir == "" {
		return errors.New("hashstore: cache dir is empty")
	}
	if c.Capacity <= 0 {
		return fmt.Errorf("hashstore: capacity must be positive, got %d", c.Capacity)
	}
	if c.MaxSize == 0 {
		c.MaxSize = c.Capacity / 10
		if c.MaxSize == 0 {
			c.MaxSize = c.Capacity
		}
	}
	if c.MaxSize < 0 || c.MaxSize > c.Capacity {
		return fmt.Errorf("hashstore: max size %d must be within capacity %d", c.MaxSize, c.Capacity)
	}
	if c.S3 != nil {
		c.S3.Bucket = strings.TrimSpace(c.S3.Bucket)
		if c.S3.Bucket == "" {
			return errors.New("hashstore: S3 bucket is empty")
		}
		c.S3.Endpoint = strings.TrimRight(strings.TrimSpace(c.S3.Endpoint), "/")
		// Clean the prefix so object writes (path.Join) and prune's
		// ListObjectsV2 prefix cannot disagree about "relay//x" or "../x".
		if p := strings.TrimSpace(c.S3.Prefix); p != "" {
			c.S3.Prefix = strings.Trim(path.Clean("/"+p), "/")
		} else {
			c.S3.Prefix = ""
		}
		c.S3.Region = strings.TrimSpace(c.S3.Region)
		if c.S3.Region == "" {
			c.S3.Region = "us-east-1"
		}
	}
	return nil
}
