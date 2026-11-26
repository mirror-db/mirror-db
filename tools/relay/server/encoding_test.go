package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestParseContentCoding(t *testing.T) {
	cases := []struct {
		value string
		want  ContentCoding
		ok    bool
	}{
		{"identity", CodingIdentity, true},
		{"GZIP", CodingGzip, true},
		{" Deflate ", CodingDeflate, true},
		{"br", CodingBrotli, true},
		{"ZSTD", CodingZstd, true},
		{"", "", false},
		{"lzma", "", false},
		{"gzip, br", "", false},
	}
	for _, c := range cases {
		got, ok := ParseContentCoding(c.value)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseContentCoding(%q) = (%q, %v), want (%q, %v)", c.value, got, ok, c.want, c.ok)
		}
	}
}

func TestContentCodingFromHeaders(t *testing.T) {
	h := http.Header{}
	if got, ok := ContentCodingFromHeaders(h); got != CodingIdentity || !ok {
		t.Fatalf("absent header = (%q, %v), want identity", got, ok)
	}
	h.Set("Content-Encoding", "GZip")
	if got, ok := ContentCodingFromHeaders(h); got != CodingGzip || !ok {
		t.Fatalf("gzip header = (%q, %v)", got, ok)
	}
	h.Set("Content-Encoding", "lzma")
	if _, ok := ContentCodingFromHeaders(h); ok {
		t.Fatal("unsupported coding reported ok")
	}
	h.Set("Content-Encoding", "")
	if got, ok := ContentCodingFromHeaders(h); got != CodingIdentity || !ok {
		t.Fatalf("empty header = (%q, %v), want identity", got, ok)
	}
}

func TestContentCodingFromHeadersMultipleLayers(t *testing.T) {
	cases := []struct {
		name   string
		values []string
	}{
		{"two field lines", []string{"gzip", "br"}},
		{"repeated same line twice", []string{"gzip", "gzip"}},
		{"one comma separated line", []string{"gzip, br"}},
		{"one line with three layers", []string{"gzip, br, zstd"}},
		{"identity plus gzip", []string{"identity", "gzip"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			for _, value := range c.values {
				h.Add("Content-Encoding", value)
			}
			if coding, ok := ContentCodingFromHeaders(h); ok {
				t.Fatalf("ContentCodingFromHeaders(%q) = (%q, true), want false", c.values, coding)
			}
		})
	}
}

func TestAcceptEncodingQuality(t *testing.T) {
	absent := ParseAcceptEncoding(nil)
	if !absent.Absent() {
		t.Fatal("nil values should mean the header was absent")
	}
	for _, coding := range supportedCodings {
		if q := absent.Quality(coding); q != 1 {
			t.Errorf("absent header quality for %s = %v, want 1", coding, q)
		}
	}

	empty := ParseAcceptEncoding([]string{""})
	if empty.Absent() {
		t.Fatal("a present empty field value is not an absent header")
	}
	if empty.Accepts(CodingIdentity) {
		t.Fatal("an empty Accept-Encoding must accept no coding")
	}

	var zero AcceptEncoding
	if !zero.Absent() {
		t.Fatal("zero value should count as absent")
	}
	if q := zero.Quality(CodingZstd); q != 1 {
		t.Fatalf("zero value quality = %v, want 1", q)
	}

	cases := []struct {
		name   string
		values []string
		coding ContentCoding
		wantQ  float64
	}{
		{"explicit", []string{"gzip;q=0.25"}, CodingGzip, 0.25},
		{"gzip q zero", []string{"gzip;q=0"}, CodingGzip, 0},
		{"identity implicit", []string{"gzip;q=0"}, CodingIdentity, 1},
		{"identity excluded", []string{"identity;q=0"}, CodingIdentity, 0},
		{"wildcard", []string{"*"}, CodingGzip, 1},
		{"wildcard identity", []string{"*"}, CodingIdentity, 1},
		{"wildcard zero excludes identity", []string{"*;q=0"}, CodingIdentity, 0},
		{"wildcard zero excludes gzip", []string{"*;q=0"}, CodingGzip, 0},
		{"wildcard zero plus identity", []string{"*;q=0, identity;q=0.5"}, CodingIdentity, 0.5},
		{"unknown name ignored", []string{"lzma;q=0.9"}, CodingGzip, 0},
		{"invalid q", []string{"gzip;q=bogus"}, CodingGzip, 0},
		{"q out of range", []string{"gzip;q=1.5"}, CodingGzip, 0},
		{"q NaN", []string{"gzip;q=NaN"}, CodingGzip, 0},
		{"duplicate keeps minimum", []string{"gzip;q=1, gzip;q=0"}, CodingGzip, 0},
		{"duplicate q parameters keep minimum", []string{"gzip;q=0.8;q=0.2"}, CodingGzip, 0.2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseAcceptEncoding(c.values).Quality(c.coding)
			if got != c.wantQ {
				t.Fatalf("Quality(%s) = %v, want %v", c.coding, got, c.wantQ)
			}
		})
	}
}

func TestAcceptEncodingNegotiate(t *testing.T) {
	cases := []struct {
		name          string
		values        []string
		stored        ContentCoding
		wantTarget    ContentCoding
		wantTranscode bool
		wantOK        bool
	}{
		{"absent keeps stored", nil, CodingGzip, CodingGzip, false, true},
		{"absent keeps identity", nil, CodingIdentity, CodingIdentity, false, true},
		{"explicit same", []string{"gzip"}, CodingGzip, CodingGzip, false, true},
		{"explicit other", []string{"gzip"}, CodingIdentity, CodingGzip, true, true},
		{"explicit beats implicit identity", []string{"gzip"}, CodingIdentity, CodingGzip, true, true},
		{"identity explicit tie keeps stored", []string{"identity, gzip"}, CodingIdentity, CodingIdentity, false, true},
		{"identity explicit tie keeps gzip", []string{"identity, gzip"}, CodingGzip, CodingGzip, false, true},
		{"q zero rejects stored", []string{"gzip;q=0"}, CodingGzip, CodingIdentity, true, true},
		{"identity excluded", []string{"gzip, identity;q=0"}, CodingIdentity, CodingGzip, true, true},
		{"identity excluded with no alternative", []string{"identity;q=0"}, CodingIdentity, "", false, false},
		{"wildcard rejects all", []string{"*;q=0"}, CodingGzip, "", false, false},
		{"wildcard keeps stored", []string{"*"}, CodingGzip, CodingGzip, false, true},
		{"wildcard keeps identity stored", []string{"*"}, CodingIdentity, CodingIdentity, false, true},
		{"wildcard lower than implicit identity", []string{"*;q=0.5"}, CodingIdentity, CodingIdentity, false, true},
		{"wildcard plus explicit", []string{"*;q=0.2, br;q=0.9, identity;q=0"}, CodingGzip, CodingBrotli, true, true},
		{"wildcard zero plus gzip", []string{"*;q=0, gzip;q=0.5"}, CodingBrotli, CodingGzip, true, true},
		{"wildcard zero plus identity", []string{"*;q=0, identity;q=0.7"}, CodingGzip, CodingIdentity, true, true},
		{"highest q", []string{"gzip;q=0.3, br;q=0.9, zstd;q=0.5, identity;q=0"}, CodingGzip, CodingBrotli, true, true},
		{"tie prefers stored", []string{"gzip, br"}, CodingBrotli, CodingBrotli, false, true},
		{"tie prefers stored gzip", []string{"br, gzip"}, CodingGzip, CodingGzip, false, true},
		{"tie prefers zstd", []string{"gzip, zstd"}, CodingDeflate, CodingZstd, true, true},
		{"tie order br before deflate", []string{"br;q=0.5, deflate;q=0.5, identity;q=0"}, CodingGzip, CodingBrotli, true, true},
		{"case-insensitive", []string{"GZip;Q=0.5, BR;q=0.6, identity;q=0"}, CodingGzip, CodingBrotli, true, true},
		{"lower explicit q loses to implicit identity", []string{"gzip;q=0.9"}, CodingIdentity, CodingIdentity, false, true},
		{"duplicate keeps rejection", []string{"gzip;q=1, gzip;q=0"}, CodingGzip, CodingIdentity, true, true},
		{"invalid q rejects coding", []string{"gzip;q=nope"}, CodingGzip, CodingIdentity, true, true},
		{"empty header accepts nothing", []string{""}, CodingIdentity, "", false, false},
		{"deflate target", []string{"deflate"}, CodingGzip, CodingDeflate, true, true},
		{"brotli target", []string{"br"}, CodingIdentity, CodingBrotli, true, true},
		{"zstd target", []string{"zstd"}, CodingIdentity, CodingZstd, true, true},
		{"unknown coding ignored", []string{"lzma, gzip"}, CodingIdentity, CodingGzip, true, true},
		{"empty stored rejected when absent", nil, "", "", false, false},
		{"empty stored rejected with header", []string{"gzip"}, "", "", false, false},
		{"unsupported stored rejected when absent", nil, ContentCoding("lzma"), "", false, false},
		{"unsupported stored rejected by wildcard", []string{"*"}, ContentCoding("lzma"), "", false, false},
		{"unsupported stored rejected explicitly", []string{"gzip"}, ContentCoding("lzma"), "", false, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target, transcode, ok := ParseAcceptEncoding(c.values).Negotiate(c.stored)
			if target != c.wantTarget || transcode != c.wantTranscode || ok != c.wantOK {
				t.Fatalf("Negotiate(%s) = (%q, %v, %v), want (%q, %v, %v)",
					c.stored, target, transcode, ok, c.wantTarget, c.wantTranscode, c.wantOK)
			}
		})
	}
}

func TestAcceptEncodingFromRequest(t *testing.T) {
	r, err := http.NewRequest(http.MethodGet, "https://relay.example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !AcceptEncodingFromRequest(r).Absent() {
		t.Fatal("request without Accept-Encoding should be absent")
	}
	r.Header.Set("Accept-Encoding", "gzip")
	if AcceptEncodingFromRequest(r).Absent() {
		t.Fatal("request with Accept-Encoding should not be absent")
	}
	if !AcceptEncodingFromRequest(nil).Absent() {
		t.Fatal("nil request should behave as absent")
	}
}

func TestApplyTranscodedResponseHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Length", "1234")
	h.Set("Content-MD5", "Q2hlY2s=")
	h.Set("Digest", "sha-256=:abc:")
	h.Set("Docker-Content-Digest", "sha256:abc")
	h.Set("ETag", `"v1"`)
	h.Set("Content-Encoding", "identity")
	h.Set("Content-Type", "text/plain")
	h.Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
	h.Set("Cache-Control", "public, max-age=60")

	ApplyTranscodedResponseHeaders(h, CodingZstd)

	for _, name := range []string{"Content-Length", "Content-MD5", "Digest", "Docker-Content-Digest"} {
		if got := h.Get(name); got != "" {
			t.Errorf("%s survived: %q", name, got)
		}
	}
	if got := h.Get("ETag"); got != `W/"v1"` {
		t.Errorf("ETag = %q, want weak form", got)
	}
	if got := h.Get("Content-Encoding"); got != "zstd" {
		t.Errorf("Content-Encoding = %q, want zstd", got)
	}
	for name, want := range map[string]string{
		"Content-Type":  "text/plain",
		"Last-Modified": "Mon, 02 Jan 2006 15:04:05 GMT",
		"Cache-Control": "public, max-age=60",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestApplyTranscodedResponseHeadersWeakAndIdentity(t *testing.T) {
	h := http.Header{}
	h.Add("ETag", `W/"already-weak"`)
	h.Add("ETag", `"strong"`)
	h.Set("Content-Encoding", "gzip")

	ApplyTranscodedResponseHeaders(h, CodingIdentity)

	values := h.Values("ETag")
	if len(values) != 2 {
		t.Fatalf("ETag values = %v", values)
	}
	if values[0] != `W/"already-weak"` || values[1] != `W/"strong"` {
		t.Errorf("ETag values = %v", values)
	}
	if got := h.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want removed for identity", got)
	}

	lower := http.Header{}
	lower.Set("ETag", `w/"lower"`)
	ApplyTranscodedResponseHeaders(lower, CodingGzip)
	if got := lower.Get("ETag"); got != `w/"lower"` {
		t.Errorf("lowercase weak ETag changed: %q", got)
	}
}

func TestApplyTranscodedResponseHeadersNil(t *testing.T) {
	ApplyTranscodedResponseHeaders(nil, CodingGzip)
}

func TestCacheControlNoTransform(t *testing.T) {
	cases := []struct {
		values []string
		want   bool
	}{
		{nil, false},
		{[]string{"public, max-age=60"}, false},
		{[]string{"no-transform"}, true},
		{[]string{"public, No-Transform"}, true},
		{[]string{"no-transform=1"}, true},
		{[]string{"no-store", "no-transform"}, true},
	}
	for _, c := range cases {
		if got := CacheControlNoTransform(c.values); got != c.want {
			t.Errorf("CacheControlNoTransform(%q) = %v, want %v", c.values, got, c.want)
		}
	}
}

func TestHasContentDigest(t *testing.T) {
	if HasContentDigest(http.Header{}) {
		t.Fatal("empty headers reported a digest")
	}
	for _, name := range []string{"Content-MD5", "Digest", "Docker-Content-Digest"} {
		h := http.Header{}
		h.Set(name, "value")
		if !HasContentDigest(h) {
			t.Errorf("%s not detected", name)
		}
	}
	h := http.Header{}
	h.Set("Digest", "  ")
	if HasContentDigest(h) {
		t.Fatal("blank digest reported present")
	}
}

func TestIsOCIDigestPath(t *testing.T) {
	sha256 := strings.Repeat("a", 64)
	sha512 := strings.Repeat("b", 128)

	cases := []struct {
		path string
		want bool
	}{
		{"/v2/library/nginx/blobs/sha256:" + sha256, true},
		{"/v2/library/nginx/manifests/sha256:" + sha256, true},
		{"/v2/library/nginx/manifests/sha512:" + sha512, true},
		{"/v2/library/nginx@sha256:" + sha256, true},
		{"/v2/library/nginx/manifests/latest", false},
		{"/v2/library/nginx/blobs/sha256:short", false},
		{"/v2/library/nginx/blobs/md5:" + sha256, false},
		{"/debian/pool/main/x/y.deb", false},
		{"/", false},
	}
	for _, c := range cases {
		if got := IsOCIDigestPath(c.path); got != c.want {
			t.Errorf("IsOCIDigestPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestOCIDigestFromPath(t *testing.T) {
	sha256hex := strings.Repeat("a", 64)
	sha512hex := strings.Repeat("b", 128)

	cases := []struct {
		path   string
		digest string
		ok     bool
	}{
		{"/v2/library/nginx/blobs/sha256:" + sha256hex, sha256hex, true},
		{"/v2/library/nginx/manifests/sha256:" + sha256hex, sha256hex, true},
		{"/blobs/sha256:" + sha256hex, sha256hex, true},
		{"/v2/library/nginx/manifests/nginx@sha256:" + sha256hex, sha256hex, true},
		{"/v2/library/nginx/blobs/sha256:" + strings.ToUpper(sha256hex), sha256hex, true},
		{"/v2/library/nginx/manifests/latest", "", false},
		{"/v2/library/nginx/blobs/sha256:short", "", false},
		{"/v2/library/nginx/blobs/md5:" + sha256hex, "", false},
		{"/v2/library/nginx/manifests/sha512:" + sha512hex, "", false},
		{"/v2/library/nginx/blobs/upload", "", false},
		{"/v2/library/nginx@sha256:" + sha256hex, "", false},
		{"/debian/pool/main/x/y.deb", "", false},
		{"/", "", false},
	}
	for _, c := range cases {
		got, ok := OCIDigestFromPath(c.path)
		if ok != c.ok || got != c.digest {
			t.Errorf("OCIDigestFromPath(%q) = %q, %v; want %q, %v", c.path, got, ok, c.digest, c.ok)
		}
	}
}

func TestTranscodingForbidden(t *testing.T) {
	digestPath := "/v2/library/nginx/blobs/sha256:" + strings.Repeat("a", 64)

	cases := []struct {
		name   string
		header http.Header
		path   string
		want   bool
	}{
		{"plain", http.Header{}, "/debian/pool/main/x/y.deb", false},
		{"no-transform", http.Header{"Cache-Control": {"public, no-transform"}}, "/x", true},
		{"content md5", http.Header{"Content-Md5": {"abc"}}, "/x", true},
		{"digest", http.Header{"Digest": {"sha-256=:abc:"}}, "/x", true},
		{"docker digest", http.Header{"Docker-Content-Digest": {"sha256:abc"}}, "/x", true},
		{"oci digest path", http.Header{}, digestPath, true},
		{"oci tag path", http.Header{}, "/v2/library/nginx/manifests/latest", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TranscodingForbidden(c.header, c.path); got != c.want {
				t.Fatalf("TranscodingForbidden = %v, want %v", got, c.want)
			}
		})
	}
}
