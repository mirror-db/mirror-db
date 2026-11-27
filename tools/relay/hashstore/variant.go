package hashstore

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ErrInvalidVariant means a variant identity or body reference was unusable.
var ErrInvalidVariant = errors.New("hashstore: invalid variant")

// ErrVariantLimit means storing a new variant would exceed a hard bound: one
// URL's variant count, the global variant count, or one encoded metadata row.
// The caller ignores a variant write that fails this way.
var ErrVariantLimit = errors.New("hashstore: variant limit reached")

// variantEncodingVersion lets a future encoding be recognised instead of being
// misread as the current one. Version 2 added the explicit header-presence flag.
// Further boolean properties use spare bits of the flags byte rather than a new
// version: a reader that predates a bit ignores it, and a row written before it
// decodes with the bit clear, so no stored row needs rewriting.
const variantEncodingVersion byte = 2

const (
	variantFlagNoVary byte = 1 << 0
	// variantFlagPersistent marks a variant whose response declared its bytes
	// immutable. It keeps the persistent lifetime across a 304 revalidation even
	// when the 304 no longer repeats that declaration. Older readers ignore an
	// unknown flag bit, so a row written with it still decodes as before.
	variantFlagPersistent byte = 1 << 1

	// maxVariantsPerURL bounds one URL's variants. A request can only produce a
	// few representations; 16 leaves room for Vary combinations without letting
	// one hot URL fill the index.
	maxVariantsPerURL = 16
	// maxVariantsGlobal bounds all metadata rows. The per-row cap bounds what
	// one response can add; this bounds how many responses can accumulate.
	maxVariantsGlobal = 65536
	// maxVariantValueSize bounds one encoded metadata row. It keeps a single
	// response's headers from turning into a multi-megabyte bbolt value.
	maxVariantValueSize = 32 << 10

	// A variant key is also prefixed with the 32-byte body digest in the reverse
	// bucket, so the key itself must leave room for it.
	maxVariantKeySize = bolt.MaxKeySize - 32
)

// Variant is one cached representation of a URL. Its identity is the tuple
// (URL, NoVary, Vary set, values of the request headers named by Vary); the
// identity is compared field by field and is never hashed. Two variants with
// the same identity are the same cache entry: writing one replaces the other.
//
// The body is stored separately by the content store and named here by its
// SHA-256 and size. A variant whose body has vanished is a cache MISS, not an
// error; the caller regenerates it.
type Variant struct {
	// URL is the canonical absolute request URL, including its query string. The
	// caller decides what canonical means; only surrounding whitespace is
	// removed here.
	URL string
	// NoVary marks a single representation per URL. Vary and RequestHeaders are
	// ignored and cleared when it is true, including a Vary of "*".
	NoVary bool
	// Vary lists the response's Vary field names, lowercased and sorted. It is
	// empty when NoVary is true.
	Vary []string
	// RequestHeaders holds the request header values selected by Vary, keyed by
	// lowercase field name. A missing key means the request did not carry that
	// header; a key present with [""] means it was present and empty. Those are
	// different identities. It is empty when NoVary is true.
	RequestHeaders http.Header
	// ResponseHeaders are the sanitised headers stored with the body. The caller
	// should already have removed hop-by-hop and credential-bearing headers; the
	// store filters them a second time as defence in depth.
	ResponseHeaders http.Header
	// BodySHA256 names the body in the content store.
	BodySHA256 string
	// BodySize is the body's logical byte length.
	BodySize int64
	// StoredAt is when the response was fetched. A zero value is stored as zero.
	StoredAt time.Time
	// ExpiresAt is when the response stops being servable without revalidation.
	// A zero value means no expiry is recorded.
	ExpiresAt time.Time
	// Persistent marks a response that declared its own immutability through the
	// relay directive. Its stored lifetime is the relay's persistent one, and a
	// later 304 keeps that property even without restating the declaration. It is
	// metadata, never part of the variant identity.
	Persistent bool
}

// Matches reports whether requestHeaders satisfy v's Vary contract. A no-vary
// variant matches every request; otherwise each header named by Vary is
// normalised exactly as PutVariant normalised it before comparison, so a
// caller's lookup cannot disagree with the identity the store wrote. Header
// absence and an explicit empty value are different, just as they are in the
// key.
func (v Variant) Matches(requestHeaders http.Header) bool {
	if v.NoVary {
		return true
	}
	got := normalizeRequestHeaders(requestHeaders, v.Vary)
	for _, name := range v.Vary {
		want, wantPresent := matchedValue(v.RequestHeaders, name)
		have, havePresent := matchedValue(got, name)
		if wantPresent != havePresent || want != have {
			return false
		}
	}
	return true
}

// Variants returns every variant stored for url, in stable key order. It does
// not touch body recency.
func (s *Store) Variants(ctx context.Context, url string) ([]Variant, error) {
	if !s.enter() {
		return nil, ErrClosed
	}
	defer s.leave()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	url = strings.TrimSpace(url)
	if url == "" {
		return nil, fmt.Errorf("%w: empty URL", ErrInvalidVariant)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.index.variants(url)
}

// PutVariant stores v. A variant with the same identity is replaced atomically;
// variants for other header values, or for a different Vary set, are untouched.
// The caller normally Put the body first; PutVariant does not verify that the
// body still exists, because a shared body may be removed by another relay at
// any time.
func (s *Store) PutVariant(ctx context.Context, v Variant) error {
	if !s.enter() {
		return ErrClosed
	}
	defer s.leave()

	if err := ctx.Err(); err != nil {
		return err
	}
	v, err := normalizeVariant(v)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.index.putVariant(v)
}

// ReplaceVariant atomically swaps the variant identified by old for new. It is
// the revalidation path: new may change the Vary set, the matched values, the
// body or the headers. If old no longer exists (another goroutine replaced or
// deleted it), new is still written; replacing a cache entry must not lose the
// fresher copy.
func (s *Store) ReplaceVariant(ctx context.Context, old, next Variant) error {
	if !s.enter() {
		return ErrClosed
	}
	defer s.leave()

	if err := ctx.Err(); err != nil {
		return err
	}
	old, err := normalizeVariantIdentity(old)
	if err != nil {
		return err
	}
	next, err = normalizeVariant(next)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.index.replaceVariant(old, next)
}

// DeleteVariant removes the variant identified by v. Only the identity fields of
// v are used. It is idempotent: deleting an absent variant is not an error.
func (s *Store) DeleteVariant(ctx context.Context, v Variant) error {
	if !s.enter() {
		return ErrClosed
	}
	defer s.leave()

	if err := ctx.Err(); err != nil {
		return err
	}
	v, err := normalizeVariantIdentity(v)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.index.deleteVariant(v)
}

// normalizeVariantIdentity makes the identity fields canonical: trimmed URL, a
// lowercased/deduplicated/sorted Vary set, and one combined value per varied
// header. An empty Vary, or one containing "*", is the single-representation
// case and is folded into NoVary so the same response can never be stored under
// two identities.
func normalizeVariantIdentity(v Variant) (Variant, error) {
	v.URL = strings.TrimSpace(v.URL)
	if v.URL == "" {
		return Variant{}, fmt.Errorf("%w: empty URL", ErrInvalidVariant)
	}

	if !v.NoVary {
		vary := normalizeVary(v.Vary)
		if len(vary) == 0 || containsString(vary, "*") {
			v.NoVary = true
		} else {
			v.Vary = vary
			v.RequestHeaders = normalizeRequestHeaders(v.RequestHeaders, vary)
			return v, nil
		}
	}

	v.NoVary = true
	v.Vary = nil
	v.RequestHeaders = http.Header{}
	return v, nil
}

// normalizeVariant additionally validates and canonicalises the fields that only
// describe a stored response.
func normalizeVariant(v Variant) (Variant, error) {
	v, err := normalizeVariantIdentity(v)
	if err != nil {
		return Variant{}, err
	}

	sha, err := NormalizeDigest(SHA256, v.BodySHA256)
	if err != nil {
		return Variant{}, err
	}
	v.BodySHA256 = sha
	if v.BodySize < 0 {
		return Variant{}, fmt.Errorf("%w: body size %d is negative", ErrInvalidVariant, v.BodySize)
	}
	v.ResponseHeaders = normalizeResponseHeaders(v.ResponseHeaders)
	return v, nil
}

func normalizeVary(vary []string) []string {
	seen := make(map[string]struct{}, len(vary))
	out := make([]string, 0, len(vary))
	for _, name := range vary {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// matchedValue reads one varied header. Normalised request headers are keyed by
// lowercase name, so the map is indexed directly: http.Header.Get would
// canonicalise the name and miss it. The boolean reports presence, which is part
// of the identity.
func matchedValue(h http.Header, name string) (string, bool) {
	values, ok := h[name]
	if !ok {
		return "", false
	}
	if len(values) == 0 {
		return "", true
	}
	return values[0], true
}

// normalizeRequestHeaders keeps only the headers Vary selected and reduces each
// to one value. Header names are case-insensitive, so the lookup must be too:
// http.Header.Get canonicalises the key it is given and would miss a map built
// with lowercase names. A header the request did not carry stays absent; one
// that was present with an empty value stays present with "".
//
// The caller's map may contain the same header under different casings. Map
// iteration is unordered, so entries are merged in sorted original-name order;
// otherwise the same request could produce different identities on two runs.
func normalizeRequestHeaders(h http.Header, vary []string) http.Header {
	type entry struct {
		name   string
		values []string
	}
	grouped := make(map[string][]entry, len(h))
	for name, values := range h {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		grouped[key] = append(grouped[key], entry{name: name, values: values})
	}

	out := make(http.Header, len(vary))
	for _, name := range vary {
		entries := grouped[name]
		if len(entries) == 0 {
			continue
		}
		if len(entries) > 1 {
			sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
		}
		var parts []string
		for _, e := range entries {
			for _, value := range e.values {
				if value = strings.TrimSpace(value); value != "" {
					parts = append(parts, value)
				}
			}
		}
		out[name] = []string{strings.Join(parts, ", ")}
	}
	return out
}

// hopByHopResponseHeaders are stripped from stored metadata. Connection-named
// tokens are added separately, except Vary, which delivery and range evaluation
// always need.
var hopByHopResponseHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-connection":    {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
	// Credential-bearing or derived headers never belong in a shared entry.
	"set-cookie":       {},
	"set-cookie2":      {},
	"www-authenticate": {},
	"content-length":   {},
	"age":              {},
}

// normalizeResponseHeaders keeps the end-to-end header set that may be stored
// with a body. It duplicates the server's storage filter as a second line of
// defence: a caller that hands over raw upstream headers cannot smuggle cookies,
// auth challenges, relay control or hop-by-hop state into the index. Vary is
// always kept, even if a Connection token names it.
func normalizeResponseHeaders(h http.Header) http.Header {
	blocked := make(map[string]struct{}, len(hopByHopResponseHeaders)+8)
	for name := range hopByHopResponseHeaders {
		blocked[name] = struct{}{}
	}
	for name, values := range h {
		if !strings.EqualFold(name, "Connection") {
			continue
		}
		for _, value := range values {
			for _, token := range strings.Split(value, ",") {
				token = strings.ToLower(strings.TrimSpace(token))
				if token != "" && token != "vary" {
					blocked[token] = struct{}{}
				}
			}
		}
	}

	type entry struct {
		name   string
		values []string
	}
	grouped := make(map[string][]entry, len(h))
	for name, values := range h {
		canonical := http.CanonicalHeaderKey(strings.TrimSpace(name))
		if canonical == "" {
			continue
		}
		lower := strings.ToLower(canonical)
		if _, drop := blocked[lower]; drop || strings.HasPrefix(lower, "x-mdb-relay") {
			continue
		}
		grouped[canonical] = append(grouped[canonical], entry{name: name, values: values})
	}

	out := make(http.Header, len(grouped))
	for canonical, entries := range grouped {
		if len(entries) > 1 {
			sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
		}
		for _, e := range entries {
			out[canonical] = append(out[canonical], e.values...)
		}
	}
	return out
}

// variantKey encodes the identity without hashing any field. Every string is
// length-prefixed, so no value can be confused with a separator. A header's
// presence is an explicit byte before its value, so absent and present-but-empty
// are distinct identities.
func variantKey(v Variant) ([]byte, error) {
	buf := make([]byte, 0, len(v.URL)+32)
	buf = appendString(buf, v.URL)
	if v.NoVary {
		buf = append(buf, variantFlagNoVary)
	} else {
		buf = append(buf, 0)
		buf = binary.BigEndian.AppendUint32(buf, uint32(len(v.Vary)))
		for _, name := range v.Vary {
			buf = appendString(buf, name)
			buf = appendMatchedHeader(buf, v.RequestHeaders, name)
		}
	}
	if len(buf) > maxVariantKeySize {
		return nil, fmt.Errorf("%w: identity is %d bytes, limit %d", ErrInvalidVariant, len(buf), maxVariantKeySize)
	}
	return buf, nil
}

func appendMatchedHeader(buf []byte, h http.Header, name string) []byte {
	value, present := matchedValue(h, name)
	if !present {
		return append(buf, 0)
	}
	buf = append(buf, 1)
	return appendString(buf, value)
}

func appendString(buf []byte, s string) []byte {
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(s)))
	return append(buf, s...)
}

// encodeVariant stores the whole variant in the value, so listing a URL needs
// only the key prefix and no decoding of the key.
func encodeVariant(v Variant) ([]byte, error) {
	sha, err := hex.DecodeString(v.BodySHA256)
	if err != nil || len(sha) != 32 {
		return nil, fmt.Errorf("%w: body digest %q", ErrInvalidVariant, v.BodySHA256)
	}

	var flags byte
	if v.NoVary {
		flags |= variantFlagNoVary
	}
	if v.Persistent {
		flags |= variantFlagPersistent
	}

	buf := make([]byte, 0, 64+len(v.URL))
	buf = append(buf, variantEncodingVersion, flags)
	buf = appendString(buf, v.URL)

	buf = binary.BigEndian.AppendUint32(buf, uint32(len(v.Vary)))
	for _, name := range v.Vary {
		buf = appendString(buf, name)
		buf = appendMatchedHeader(buf, v.RequestHeaders, name)
	}

	buf = appendHeaderMap(buf, v.ResponseHeaders)
	buf = append(buf, sha...)
	buf = binary.BigEndian.AppendUint64(buf, uint64(v.BodySize))
	buf = binary.BigEndian.AppendUint64(buf, encodeVariantTime(v.StoredAt))
	buf = binary.BigEndian.AppendUint64(buf, encodeVariantTime(v.ExpiresAt))
	if len(buf) > maxVariantValueSize {
		return nil, fmt.Errorf("%w: encoded metadata is %d bytes, limit %d", ErrVariantLimit, len(buf), maxVariantValueSize)
	}
	return buf, nil
}

func appendHeaderMap(buf []byte, h http.Header) []byte {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)

	buf = binary.BigEndian.AppendUint32(buf, uint32(len(names)))
	for _, name := range names {
		buf = appendString(buf, name)
		values := h[name]
		buf = binary.BigEndian.AppendUint32(buf, uint32(len(values)))
		for _, value := range values {
			buf = appendString(buf, value)
		}
	}
	return buf
}

func decodeVariant(data []byte) (Variant, error) {
	if len(data) > maxVariantValueSize {
		return Variant{}, fmt.Errorf("%w: metadata is %d bytes, limit %d", ErrInvalidVariant, len(data), maxVariantValueSize)
	}
	d := variantDecoder{data: data}
	version, err := d.byte()
	if err != nil {
		return Variant{}, err
	}
	if version != variantEncodingVersion {
		return Variant{}, fmt.Errorf("%w: unknown encoding version %d", ErrInvalidVariant, version)
	}
	flags, err := d.byte()
	if err != nil {
		return Variant{}, err
	}

	v := Variant{
		NoVary:     flags&variantFlagNoVary != 0,
		Persistent: flags&variantFlagPersistent != 0,
	}
	if v.URL, err = d.string(); err != nil {
		return Variant{}, err
	}

	n, err := d.uint32()
	if err != nil {
		return Variant{}, err
	}
	if n > uint32(len(data)) {
		return Variant{}, fmt.Errorf("%w: corrupt Vary count %d", ErrInvalidVariant, n)
	}
	for i := uint32(0); i < n; i++ {
		name, err := d.string()
		if err != nil {
			return Variant{}, err
		}
		present, err := d.byte()
		if err != nil {
			return Variant{}, err
		}
		v.Vary = append(v.Vary, name)
		switch present {
		case 0:
			// The request did not carry this header.
		case 1:
			value, err := d.string()
			if err != nil {
				return Variant{}, err
			}
			if v.RequestHeaders == nil {
				v.RequestHeaders = make(http.Header, n)
			}
			v.RequestHeaders[name] = []string{value}
		default:
			return Variant{}, fmt.Errorf("%w: corrupt header presence %d", ErrInvalidVariant, present)
		}
	}
	if v.RequestHeaders == nil {
		v.RequestHeaders = http.Header{}
	}

	if v.ResponseHeaders, err = d.headerMap(); err != nil {
		return Variant{}, err
	}

	sha, err := d.bytes(32)
	if err != nil {
		return Variant{}, err
	}
	v.BodySHA256 = hex.EncodeToString(sha)

	size, err := d.uint64()
	if err != nil {
		return Variant{}, err
	}
	if int64(size) < 0 {
		return Variant{}, fmt.Errorf("%w: corrupt body size", ErrInvalidVariant)
	}
	v.BodySize = int64(size)

	storedAt, err := d.uint64()
	if err != nil {
		return Variant{}, err
	}
	expiresAt, err := d.uint64()
	if err != nil {
		return Variant{}, err
	}
	v.StoredAt = decodeVariantTime(storedAt)
	v.ExpiresAt = decodeVariantTime(expiresAt)
	if !d.done() {
		return Variant{}, fmt.Errorf("%w: %d trailing bytes", ErrInvalidVariant, d.remaining())
	}
	return v, nil
}

// encodeVariantTime stores zero as zero: UnixNano of the zero time is a large
// negative number, not a sentinel.
func encodeVariantTime(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	return uint64(t.UnixNano())
}

func decodeVariantTime(n uint64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(n)).UTC()
}

type variantDecoder struct {
	data []byte
	off  int
}

func (d *variantDecoder) remaining() int { return len(d.data) - d.off }
func (d *variantDecoder) done() bool     { return d.off == len(d.data) }

func (d *variantDecoder) bytes(n int) ([]byte, error) {
	if n < 0 || d.remaining() < n {
		return nil, fmt.Errorf("%w: truncated record", ErrInvalidVariant)
	}
	out := d.data[d.off : d.off+n]
	d.off += n
	return out, nil
}

func (d *variantDecoder) byte() (byte, error) {
	b, err := d.bytes(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (d *variantDecoder) uint32() (uint32, error) {
	b, err := d.bytes(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

func (d *variantDecoder) uint64() (uint64, error) {
	b, err := d.bytes(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b), nil
}

func (d *variantDecoder) string() (string, error) {
	n, err := d.uint32()
	if err != nil {
		return "", err
	}
	b, err := d.bytes(int(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (d *variantDecoder) headerMap() (http.Header, error) {
	n, err := d.uint32()
	if err != nil {
		return nil, err
	}
	if n > uint32(len(d.data)) {
		return nil, fmt.Errorf("%w: corrupt header count %d", ErrInvalidVariant, n)
	}
	out := make(http.Header, n)
	for i := uint32(0); i < n; i++ {
		name, err := d.string()
		if err != nil {
			return nil, err
		}
		count, err := d.uint32()
		if err != nil {
			return nil, err
		}
		if count > uint32(len(d.data)) {
			return nil, fmt.Errorf("%w: corrupt header value count %d", ErrInvalidVariant, count)
		}
		for j := uint32(0); j < count; j++ {
			value, err := d.string()
			if err != nil {
				return nil, err
			}
			out[name] = append(out[name], value)
		}
	}
	return out, nil
}
