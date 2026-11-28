package activecache

import (
	"encoding/hex"
	"hash"
	"sync/atomic"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// digestSink is one tiered PrefixSink: it hashes every byte the cache accepted
// as the contiguous object prefix and counts those bytes. The count is the O(1)
// completeness check for publication. A sink that saw fewer bytes than the
// object's extent means the cache never held the complete object; one that saw
// more is not describing that object at all. Either way its digest is not the
// body's, and entryStore falls back to hashstore.Adopt, which recomputes both
// digests from the file.
//
// The tiered core feeds sinks synchronously and in order while it caches the
// bytes, so the counts and the hash state are final by the time it starts the
// store handoff. Sum is only read after that point, when the handoff guarantees
// the sinks finished the complete prefix. The count proves completeness, not
// content identity: that the hashed bytes are the bytes the cache registered
// rests on the tiered core's prefix contract (ADR 0019).
type digestSink struct {
	h hash.Hash
	n atomic.Int64
}

func newDigestSink(h hash.Hash) *digestSink { return &digestSink{h: h} }

func (s *digestSink) Write(p []byte) (int, error) {
	n, err := s.h.Write(p)
	s.n.Add(int64(n))
	return n, err
}

// sum returns the hex digest and how many bytes produced it.
func (s *digestSink) sum() (string, int64) {
	return hex.EncodeToString(s.h.Sum(nil)), s.n.Load()
}

// assembledContent names the complete object the sinks hashed, or reports false
// when either sink did not see exactly size bytes. A nil sink or a negative size
// also refuses: without a full prefix there is no trustworthy content.
func assembledContent(sha1Sink, sha256Sink *digestSink, size int64) (hashstore.Content, bool) {
	if sha1Sink == nil || sha256Sink == nil || size < 0 {
		return hashstore.Content{}, false
	}
	sha1Hex, sha1N := sha1Sink.sum()
	sha256Hex, sha256N := sha256Sink.sum()
	if sha1N != size || sha256N != size {
		return hashstore.Content{}, false
	}
	return hashstore.Content{SHA1: sha1Hex, SHA256: sha256Hex, Size: size}, true
}
