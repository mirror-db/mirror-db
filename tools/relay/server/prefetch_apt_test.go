package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

func aptTestMirror() Mirror {
	return Mirror{Name: "debian", Path: "debian", PolicyTags: []string{"apt"}}
}

func mustParseIndex(t *testing.T, data, mirror string) *aptIndex {
	t.Helper()
	idx, err := parseAPTIndex([]byte(data), mirror)
	if err != nil {
		t.Fatalf("parse index: %v\n%s", err, data)
	}
	return idx
}

func TestAPTIndexTargetPaths(t *testing.T) {
	cases := []struct {
		name   string
		mirror Mirror
		want   string
	}{
		{"path mount", Mirror{Name: "debian", Path: "debian"}, "/debian/.mdb/apt-index.json"},
		{"subdomain", Mirror{Name: "syncthing-pkgs", Host: "syncthing-pkgs"}, "/.mdb/apt-index.json"},
		{"host wins when both", Mirror{Name: "x", Host: "x", Path: "y"}, "/.mdb/apt-index.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, rel := aptIndexTarget(c.mirror)
			if got != c.want {
				t.Fatalf("target = %q, want %q", got, c.want)
			}
			if rel != "/.mdb/apt-index.json" {
				t.Fatalf("relative target = %q", rel)
			}
		})
	}
}

func TestParseAPTIndexValid(t *testing.T) {
	data := `{
		"schema":1,
		"mirror":"debian",
		"generated_at":"2026-01-02T03:04:05Z",
		"expires_at":"2026-01-02T04:04:05Z",
		"suites":[{"suite":"bookworm","release_path":"dists/bookworm/Release","date":"2026-01-01T00:00:00Z","valid_until":"2026-01-10T00:00:00Z","sha256":"` + strings.Repeat("a", 64) + `"}],
		"files":[
			{"path":"dists/bookworm/main/binary-amd64/Packages","size":10,"sha256":"` + strings.Repeat("b", 64) + `","kind":"metadata"},
			{"path":"pool/main/x/x.deb","size":20,"sha1":"` + strings.Repeat("c", 40) + `","kind":"package"}
		]
	}`
	idx := mustParseIndex(t, data, "debian")
	if idx.Schema != 1 || idx.Mirror != "debian" {
		t.Fatalf("header = %+v", idx)
	}
	if len(idx.Suites) != 1 || len(idx.Files) != 2 {
		t.Fatalf("suites/files = %d/%d", len(idx.Suites), len(idx.Files))
	}
	file, ok := idx.byPath["pool/main/x/x.deb"]
	if !ok || file.Kind != "package" || file.SHA1 != strings.Repeat("c", 40) {
		t.Fatalf("package entry = %+v, %v", file, ok)
	}
}

func TestParseAPTIndexRejectsMaliciousInput(t *testing.T) {
	base := `{"schema":1,"mirror":"debian","generated_at":"2026-01-02T03:04:05Z","expires_at":"2026-01-02T04:04:05Z","suites":[],"files":[]}`
	validHash := strings.Repeat("a", 64)
	cases := map[string]string{
		"wrong schema":      strings.Replace(base, `"schema":1`, `"schema":2`, 1),
		"missing schema":    strings.Replace(base, `"schema":1,`, ``, 1),
		"mirror mismatch":   strings.Replace(base, `"mirror":"debian"`, `"mirror":"ubuntu"`, 1),
		"bad generated_at":  strings.Replace(base, `"generated_at":"2026-01-02T03:04:05Z"`, `"generated_at":"yesterday"`, 1),
		"expiry before gen": strings.Replace(base, `"expires_at":"2026-01-02T04:04:05Z"`, `"expires_at":"2026-01-01T04:04:05Z"`, 1),
		"bad valid_until":   `{"schema":1,"mirror":"debian","generated_at":"2026-01-02T03:04:05Z","expires_at":"2026-01-02T04:04:05Z","suites":[{"suite":"bookworm","release_path":"dists/bookworm/Release","valid_until":"soon"}],"files":[]}`,
		"bad date":          `{"schema":1,"mirror":"debian","generated_at":"2026-01-02T03:04:05Z","expires_at":"2026-01-02T04:04:05Z","suites":[{"suite":"bookworm","release_path":"dists/bookworm/Release","date":"yesterday"}],"files":[]}`,
		"unknown field":     strings.Replace(base, `"suites":[]`, `"suites":[],"extra":true`, 1),
		"absolute path":     `{"schema":1,"mirror":"debian","generated_at":"2026-01-02T03:04:05Z","expires_at":"2026-01-02T04:04:05Z","suites":[],"files":[{"path":"/etc/passwd","size":1,"kind":"metadata"}]}`,
		"traversal path":    `{"schema":1,"mirror":"debian","generated_at":"2026-01-02T03:04:05Z","expires_at":"2026-01-02T04:04:05Z","suites":[],"files":[{"path":"pool/../../x","size":1,"kind":"metadata"}]}`,
		"negative size":     `{"schema":1,"mirror":"debian","generated_at":"2026-01-02T03:04:05Z","expires_at":"2026-01-02T04:04:05Z","suites":[],"files":[{"path":"pool/x.deb","size":-1,"kind":"package","sha256":"` + validHash + `"}]}`,
		"bad digest":        `{"schema":1,"mirror":"debian","generated_at":"2026-01-02T03:04:05Z","expires_at":"2026-01-02T04:04:05Z","suites":[],"files":[{"path":"pool/x.deb","size":1,"kind":"package","sha256":"zz"}]}`,
		"bad kind":          `{"schema":1,"mirror":"debian","generated_at":"2026-01-02T03:04:05Z","expires_at":"2026-01-02T04:04:05Z","suites":[],"files":[{"path":"pool/x.deb","size":1,"kind":"other"}]}`,
		"package no hash":   `{"schema":1,"mirror":"debian","generated_at":"2026-01-02T03:04:05Z","expires_at":"2026-01-02T04:04:05Z","suites":[],"files":[{"path":"pool/x.deb","size":1,"kind":"package"}]}`,
		"path conflict":     `{"schema":1,"mirror":"debian","generated_at":"2026-01-02T03:04:05Z","expires_at":"2026-01-02T04:04:05Z","suites":[],"files":[{"path":"pool/x.deb","size":1,"kind":"package","sha256":"` + validHash + `"},{"path":"pool/x.deb","size":2,"kind":"package","sha256":"` + strings.Repeat("b", 64) + `"}]}`,
		"trailing json":     base + `{}`,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseAPTIndex([]byte(data), "debian"); err == nil {
				t.Fatalf("accepted malicious index:\n%s", data)
			}
		})
	}
}

func TestAPTIndexTaskRegistrationAndImmediateFetch(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []string
		bodies   []string
	)
	index := `{"schema":1,"mirror":"debian","generated_at":"2026-01-02T03:04:05Z","expires_at":"2026-01-02T04:04:05Z","suites":[],"files":[]}`
	_, up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path)
		bodies = append(bodies, r.Header.Get("If-None-Match")+"|"+r.Header.Get("Accept-Encoding"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"gen-1"`)
		writeResponse(w, r, http.StatusOK, index, nil)
	})

	clock := newTestClock()
	s, _ := newCacheServer(t, up.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{aptTestMirror()}})
	disablePrefetchJitter(s)

	m := aptTestMirror()
	s.prefetch.NoteAPTResponse(m, clock.Now(), false, nil)
	s.prefetch.NoteAPTResponse(m, clock.Now(), false, nil)
	tasks := s.prefetch.tasks
	if len(tasks) != 1 {
		t.Fatalf("APT tasks = %d, want one shared task", len(tasks))
	}
	task := snapshotTask(t, s.prefetch, aptTaskKey("debian"))
	if task.clientPath != "/debian/.mdb/apt-index.json" {
		t.Fatalf("index path = %q", task.clientPath)
	}
	if !task.hasNext || !task.nextRun.Equal(clock.Now()) {
		t.Fatalf("first fetch is not immediate: %v", task.nextRun)
	}

	dispatchAndWait(s)

	if got := s.aptIndexes.get("debian"); got == nil {
		t.Fatal("index was not installed")
	}
	task = snapshotTask(t, s.prefetch, aptTaskKey("debian"))
	if task.aptEtag != `"gen-1"` {
		t.Fatalf("apt etag = %q", task.aptEtag)
	}

	mu.Lock()
	gotPath := requests[0]
	gotHeaders := bodies[0]
	mu.Unlock()
	if gotPath != "/debian/.mdb/apt-index.json" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotHeaders != "|identity" {
		t.Fatalf("first index headers = %q", gotHeaders)
	}
}

func TestAPTIndexConditionalAndAtomicReplacement(t *testing.T) {
	var (
		mu      sync.Mutex
		version = 1
		seen304 int
	)
	gen1 := `{"schema":1,"mirror":"debian","generated_at":"2026-01-02T03:04:05Z","expires_at":"2026-01-02T04:04:05Z","suites":[],"files":[]}`
	gen2 := `{"schema":1,"mirror":"debian","generated_at":"2026-01-02T05:04:05Z","expires_at":"2026-01-02T06:04:05Z","suites":[],"files":[]}`
	_, up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("If-None-Match") == `"gen-1"` {
			version = 2
			seen304++
			w.Header().Set("ETag", `"gen-1"`)
			w.Header().Set("Cache-Control", "max-age=600")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		etag := `"gen-1"`
		body := gen1
		if version == 2 {
			etag = `"gen-2"`
			body = gen2
		}
		w.Header().Set("ETag", etag)
		writeResponse(w, r, http.StatusOK, body, nil)
	})

	clock := newTestClock()
	s, _ := newCacheServer(t, up.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{aptTestMirror()}})
	disablePrefetchJitter(s)
	m := aptTestMirror()

	s.prefetch.NoteAPTResponse(m, clock.Now(), false, nil)
	dispatchAndWait(s)
	first := s.aptIndexes.get("debian")
	if first == nil || !first.GeneratedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("first generation = %+v", first)
	}

	// 304 keeps the same generation and folds the response freshness.
	clock.Advance(time.Hour)
	task := snapshotTask(t, s.prefetch, aptTaskKey("debian"))
	clock.Advance(task.nextRun.Sub(clock.Now()))
	dispatchAndWait(s)
	mu.Lock()
	got304 := seen304
	mu.Unlock()
	if got304 != 1 {
		t.Fatalf("304 responses = %d", got304)
	}
	if s.aptIndexes.get("debian") != first {
		t.Fatal("304 replaced the index generation")
	}

	// A changed generation replaces the map atomically.
	clock.Advance(10 * time.Minute)
	s.prefetch.mu.Lock()
	s.prefetch.tasks[aptTaskKey("debian")].aptEtag = `"stale"`
	s.prefetch.tasks[aptTaskKey("debian")].nextRun = clock.Now()
	s.prefetch.mu.Unlock()
	dispatchAndWait(s)
	second := s.aptIndexes.get("debian")
	if second == nil || second.GeneratedAt.Equal(first.GeneratedAt) {
		t.Fatalf("second generation was not installed: %+v", second)
	}
}

func TestAPTIndexSchemaFailureKeepsOldGeneration(t *testing.T) {
	var version int
	good := `{"schema":1,"mirror":"debian","generated_at":"2026-01-02T03:04:05Z","expires_at":"2026-01-02T04:04:05Z","suites":[],"files":[]}`
	_, up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if version == 1 {
			writeResponse(w, r, http.StatusOK, `{"schema":2}`, nil)
			return
		}
		writeResponse(w, r, http.StatusOK, good, nil)
	})
	clock := newTestClock()
	s, _ := newCacheServer(t, up.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{aptTestMirror()}})
	disablePrefetchJitter(s)
	m := aptTestMirror()

	s.prefetch.NoteAPTResponse(m, clock.Now(), false, nil)
	dispatchAndWait(s)
	first := s.aptIndexes.get("debian")
	if first == nil {
		t.Fatal("good generation was not installed")
	}

	version = 1
	s.prefetch.mu.Lock()
	s.prefetch.tasks[aptTaskKey("debian")].nextRun = clock.Now()
	s.prefetch.mu.Unlock()
	dispatchAndWait(s)
	if s.aptIndexes.get("debian") != first {
		t.Fatal("malicious generation replaced the previous one")
	}
}

func TestHashPredictionDeliversFromStore(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, "upstream", nil)
	})
	clock := newTestClock()
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{aptTestMirror()}})
	content, err := store.Put(t.Context(), strings.NewReader("deb-body"), 8)
	if err != nil {
		t.Fatalf("put body: %v", err)
	}
	installAPTFile(s, clock, content, "pool/main/x/x.deb")

	resp := cacheRequest(s, http.MethodGet, "/debian/pool/main/x/x.deb", map[string]string{"Accept-Encoding": "gzip, br"})
	if resp.Code != http.StatusOK || resp.Body.String() != "deb-body" {
		t.Fatalf("predicted GET = %d %q", resp.Code, resp.Body.String())
	}
	if got := up.count(); got != 0 {
		t.Fatalf("prediction hit upstream %d times", got)
	}
	if resp.Header().Get("Digest") == "" {
		t.Fatal("predicted response lacks a content digest")
	}
	etag := resp.Header().Get("ETag")
	if etag == "" {
		t.Fatal("predicted response lacks an ETag")
	}

	head := cacheRequest(s, http.MethodHead, "/debian/pool/main/x/x.deb", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("predicted HEAD = %d body %d", head.Code, head.Body.Len())
	}
	if head.Header().Get("Content-Length") != "8" {
		t.Fatalf("predicted HEAD Content-Length = %q", head.Header().Get("Content-Length"))
	}
	if got := up.count(); got != 0 {
		t.Fatalf("predicted HEAD hit upstream %d times", got)
	}

	conditional := cacheRequest(s, http.MethodGet, "/debian/pool/main/x/x.deb", map[string]string{"If-None-Match": etag})
	if conditional.Code != http.StatusNotModified {
		t.Fatalf("conditional predicted GET = %d", conditional.Code)
	}
	if got := up.count(); got != 0 {
		t.Fatalf("conditional prediction hit upstream %d times", got)
	}

	variant, err := store.Variants(t.Context(), "mirror://debian/pool/main/x/x.deb")
	if err != nil {
		t.Fatalf("list variants: %v", err)
	}
	if len(variant) != 0 {
		t.Fatalf("prediction created %d long-term variants", len(variant))
	}
}

func TestHashPredictionRangesAndIfRange(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, "upstream", nil)
	})
	clock := newTestClock()
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{aptTestMirror()}})
	content, err := store.Put(t.Context(), strings.NewReader("0123456789"), 10)
	if err != nil {
		t.Fatalf("put body: %v", err)
	}
	installAPTFile(s, clock, content, "pool/main/y/y.deb")

	single := cacheRequest(s, http.MethodGet, "/debian/pool/main/y/y.deb", map[string]string{"Range": "bytes=2-4"})
	if single.Code != http.StatusPartialContent || single.Body.String() != "234" {
		t.Fatalf("single range = %d %q", single.Code, single.Body.String())
	}
	if single.Header().Get("Content-Range") != "bytes 2-4/10" {
		t.Fatalf("Content-Range = %q", single.Header().Get("Content-Range"))
	}

	multi := cacheRequest(s, http.MethodGet, "/debian/pool/main/y/y.deb", map[string]string{"Range": "bytes=0-0,2-3"})
	if multi.Code != http.StatusPartialContent || !strings.HasPrefix(multi.Header().Get("Content-Type"), "multipart/byteranges") {
		t.Fatalf("multipart range = %d %q", multi.Code, multi.Header().Get("Content-Type"))
	}

	unsatisfiable := cacheRequest(s, http.MethodGet, "/debian/pool/main/y/y.deb", map[string]string{"Range": "bytes=99-"})
	if unsatisfiable.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("416 = %d", unsatisfiable.Code)
	}

	full := cacheRequest(s, http.MethodGet, "/debian/pool/main/y/y.deb", map[string]string{
		"Range":    "bytes=0-1",
		"If-Range": `"sha256:deadbeef"`,
	})
	if full.Code != http.StatusOK || full.Body.String() != "0123456789" {
		t.Fatalf("stale If-Range fallback = %d %q", full.Code, full.Body.String())
	}

	matching := cacheRequest(s, http.MethodGet, "/debian/pool/main/y/y.deb", map[string]string{
		"Range":    "bytes=0-1",
		"If-Range": single.Header().Get("ETag"),
	})
	if matching.Code != http.StatusPartialContent || matching.Body.String() != "01" {
		t.Fatalf("matching If-Range = %d %q", matching.Code, matching.Body.String())
	}
	if got := up.count(); got != 0 {
		t.Fatalf("range prediction hit upstream %d times", got)
	}
}

func TestHashPredictionSHA1Fallback(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, "upstream", nil)
	})
	clock := newTestClock()
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{aptTestMirror()}})
	content, err := store.Put(t.Context(), strings.NewReader("sha1-body"), 9)
	if err != nil {
		t.Fatalf("put body: %v", err)
	}
	idx := testAPTIndex(clock, time.Hour, aptFile{Path: "pool/main/s/s.deb", Size: 9, SHA1: content.SHA1, Kind: "package"})
	s.aptIndexes.set("debian", idx)

	resp := cacheRequest(s, http.MethodGet, "/debian/pool/main/s/s.deb", nil)
	if resp.Code != http.StatusOK || resp.Body.String() != "sha1-body" {
		t.Fatalf("SHA-1 predicted GET = %d %q", resp.Code, resp.Body.String())
	}
	if got := up.count(); got != 0 {
		t.Fatalf("SHA-1 prediction hit upstream %d times", got)
	}
}

func TestHashPredictionFallsBackWhenUnusable(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, "upstream", nil)
	})
	clock := newTestClock()
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{aptTestMirror()}})

	// Body absent.
	s.aptIndexes.set("debian", testAPTIndex(clock, time.Hour, aptFile{
		Path: "pool/main/m/missing.deb", Size: 5, SHA256: strings.Repeat("d", 64), Kind: "package",
	}))
	resp := cacheRequest(s, http.MethodGet, "/debian/pool/main/m/missing.deb", nil)
	if resp.Body.String() != "upstream" {
		t.Fatalf("missing body = %q", resp.Body.String())
	}
	if got := up.count(); got != 1 {
		t.Fatalf("missing body upstream hits = %d", got)
	}

	// Stale index is not authoritative even when the body exists.
	content, err := store.Put(t.Context(), strings.NewReader("stale-body"), 10)
	if err != nil {
		t.Fatalf("put body: %v", err)
	}
	s.aptIndexes.set("debian", testAPTIndex(clock, -time.Minute, aptFile{
		Path: "pool/main/s/stale.deb", Size: 10, SHA256: content.SHA256, Kind: "package",
	}))
	stale := cacheRequest(s, http.MethodGet, "/debian/pool/main/s/stale.deb", nil)
	if stale.Body.String() != "upstream" {
		t.Fatalf("stale index = %q", stale.Body.String())
	}
	if got := up.count(); got != 2 {
		t.Fatalf("stale index upstream hits = %d", got)
	}
}

func TestHashPredictionDropsMismatchedVariant(t *testing.T) {
	var upstreamBody = "old-body"
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"old"`)
		w.Header().Set("Cache-Control", "max-age=3600")
		writeResponse(w, r, http.StatusOK, upstreamBody, nil)
	})
	clock := newTestClock()
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{aptTestMirror()}})

	first := cacheRequest(s, http.MethodGet, "/debian/pool/main/x/x.deb", nil)
	if first.Body.String() != "old-body" {
		t.Fatalf("first body = %q", first.Body.String())
	}
	if got := up.count(); got != 1 {
		t.Fatalf("upstream hits = %d", got)
	}

	content, err := store.Put(t.Context(), strings.NewReader("new-body"), 8)
	if err != nil {
		t.Fatalf("put body: %v", err)
	}
	installAPTFile(s, clock, content, "pool/main/x/x.deb")

	second := cacheRequest(s, http.MethodGet, "/debian/pool/main/x/x.deb", nil)
	if second.Body.String() != "new-body" {
		t.Fatalf("mismatched variant was served: %q", second.Body.String())
	}
	if got := up.count(); got != 1 {
		t.Fatalf("mismatched variant path hit upstream: %d", got)
	}
	variants, err := store.Variants(t.Context(), "mirror://debian/pool/main/x/x.deb")
	if err != nil {
		t.Fatalf("list variants: %v", err)
	}
	if len(variants) != 0 {
		t.Fatalf("old variant was not dropped: %d", len(variants))
	}
}

func TestHashPredictionIndexUpdateSwitchesBody(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, "upstream", nil)
	})
	clock := newTestClock()
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{aptTestMirror()}})
	first, _ := store.Put(t.Context(), strings.NewReader("first-body"), 10)
	second, _ := store.Put(t.Context(), strings.NewReader("second-body!"), 12)
	path := "pool/main/u/u.deb"

	installAPTFile(s, clock, first, path)
	if got := cacheRequest(s, http.MethodGet, "/debian/"+path, nil).Body.String(); got != "first-body" {
		t.Fatalf("first generation served %q", got)
	}
	installAPTFile(s, clock, second, path)
	if got := cacheRequest(s, http.MethodGet, "/debian/"+path, nil).Body.String(); got != "second-body!" {
		t.Fatalf("second generation served %q", got)
	}
	if got := up.count(); got != 0 {
		t.Fatalf("index update fell back upstream %d times", got)
	}
}

func TestPrefetchPassthroughIsUntouched(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(RelayDirectiveHeader, "prefetch,no-store")
		writeResponse(w, r, http.StatusOK, "pass", nil)
	})
	clock := newTestClock()
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		clock:   clock,
		mirrors: []Mirror{testMirror()},
		policies: []Policy{{
			Name:  "pass",
			Match: PolicyMatch{PathPrefix: "/thing"},
			Rules: PolicyRules{Passthrough: boolRule(true)},
		}},
	})
	disablePrefetchJitter(s)

	resp := cacheRequest(s, http.MethodGet, "/test/thing", nil)
	if resp.Body.String() != "pass" {
		t.Fatalf("passthrough body = %q", resp.Body.String())
	}
	if got := up.count(); got != 1 {
		t.Fatalf("passthrough upstream hits = %d", got)
	}
	if got := resp.Header().Get(RelayDirectiveHeader); got != "prefetch,no-store" {
		t.Fatalf("passthrough stripped the directive: %q", got)
	}
	if len(s.prefetch.tasks) != 0 {
		t.Fatalf("passthrough registered %d prefetch tasks", len(s.prefetch.tasks))
	}
	variants, err := store.Variants(t.Context(), "mirror://test/thing")
	if err != nil {
		t.Fatalf("list variants: %v", err)
	}
	if len(variants) != 0 {
		t.Fatalf("passthrough cached %d variants", len(variants))
	}
}

func testAPTIndex(clock *testClock, ttl time.Duration, files ...aptFile) *aptIndex {
	now := clock.Now()
	idx := &aptIndex{
		Schema:      aptIndexSchemaVersion,
		Mirror:      "debian",
		GeneratedAt: now,
		ExpiresAt:   now.Add(ttl),
		byPath:      make(map[string]aptFile, len(files)),
	}
	for _, file := range files {
		idx.Files = append(idx.Files, file)
		idx.byPath[file.Path] = file
	}
	return idx
}

func installAPTFile(s *Server, clock *testClock, content hashstore.Content, path string) {
	s.aptIndexes.set("debian", testAPTIndex(clock, time.Hour, aptFile{
		Path:   path,
		Size:   content.Size,
		SHA256: content.SHA256,
		SHA1:   content.SHA1,
		Kind:   "package",
	}))
}

// aptIndexDocument renders a schema-1 document naming one package, so a test can
// hand the same body to a 200 response and its expiry to the assertion.
func aptIndexDocument(t *testing.T, generated, expires time.Time, file aptFile) string {
	t.Helper()
	entry := map[string]any{
		"path": file.Path,
		"size": file.Size,
		"kind": file.Kind,
	}
	if file.SHA256 != "" {
		entry["sha256"] = file.SHA256
	}
	if file.SHA1 != "" {
		entry["sha1"] = file.SHA1
	}
	data, err := json.Marshal(map[string]any{
		"schema":       aptIndexSchemaVersion,
		"mirror":       "debian",
		"generated_at": generated.Format(time.RFC3339),
		"expires_at":   expires.Format(time.RFC3339),
		"suites":       []any{},
		"files":        []any{entry},
	})
	if err != nil {
		t.Fatalf("encode index: %v", err)
	}
	return string(data)
}

// TestAPTIndex304ExtendsEffectiveExpiry covers a document whose expires_at has
// already passed while the upstream still answers 304 with fresh Cache-Control.
// The 304 must extend how long the generation stays authoritative, so hash
// prediction works again; a 200 would instead take the new document's expiry.
func TestAPTIndex304ExtendsEffectiveExpiry(t *testing.T) {
	clock := newTestClock()
	start := clock.Now()
	var (
		mu          sync.Mutex
		indexBody   string
		notModified int
	)
	upRec, up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/.mdb/apt-index.json") {
			writeResponse(w, r, http.StatusOK, "upstream-package", map[string]string{"Cache-Control": "max-age=60"})
			return
		}
		mu.Lock()
		body := indexBody
		mu.Unlock()
		if r.Header.Get("If-None-Match") == `"gen-1"` {
			mu.Lock()
			notModified++
			mu.Unlock()
			w.Header().Set("ETag", `"gen-1"`)
			w.Header().Set("Cache-Control", "max-age=600")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"gen-1"`)
		writeResponse(w, r, http.StatusOK, body, nil)
	})

	s, store := newCacheServer(t, up.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{aptTestMirror()}})
	disablePrefetchJitter(s)
	content, err := store.Put(t.Context(), strings.NewReader("deb-body"), 8)
	if err != nil {
		t.Fatalf("put body: %v", err)
	}
	file := aptFile{Path: "pool/main/x/x.deb", Size: content.Size, SHA256: content.SHA256, Kind: "package"}
	mu.Lock()
	indexBody = aptIndexDocument(t, start, start.Add(time.Minute), file)
	mu.Unlock()

	s.prefetch.NoteAPTResponse(aptTestMirror(), clock.Now(), false, nil)
	dispatchAndWait(s)
	if gen := s.aptIndexes.generation("debian"); gen == nil || !gen.until.Equal(start.Add(time.Minute)) {
		t.Fatalf("generation deadline = %+v", gen)
	}
	if got := upRec.count(); got != 1 {
		t.Fatalf("index fetches = %d, want 1", got)
	}

	// The document's own expiry passes; a real Release request finds the
	// generation stale and asks for an immediate refresh.
	clock.Advance(5 * time.Minute)
	s.prefetch.NoteAPTResponse(aptTestMirror(), clock.Now(), false, s.aptIndexes.generation("debian"))
	task := snapshotTask(t, s.prefetch, aptTaskKey("debian"))
	if !task.hasNext || task.nextRun.After(clock.Now()) {
		t.Fatalf("stale generation is not due for refresh: nextRun %v (hasNext %v)", task.nextRun, task.hasNext)
	}
	dispatchAndWait(s)
	mu.Lock()
	got304 := notModified
	mu.Unlock()
	if got304 != 1 {
		t.Fatalf("304 responses = %d, want 1", got304)
	}

	extended := clock.Now().Add(600 * time.Second)
	if gen := s.aptIndexes.generation("debian"); gen == nil || !gen.until.Equal(extended) {
		t.Fatalf("304 did not extend the effective deadline: %+v", gen)
	}
	task = snapshotTask(t, s.prefetch, aptTaskKey("debian"))
	if !task.nextRun.Equal(extended) {
		t.Fatalf("refresh after extension = %v, want %v", task.nextRun, extended)
	}

	// The extended generation is authoritative again, so the stored package is
	// delivered by hash without touching upstream.
	resp := cacheRequest(s, http.MethodGet, "/debian/pool/main/x/x.deb", nil)
	if resp.Code != http.StatusOK || resp.Body.String() != "deb-body" {
		t.Fatalf("prediction after extension = %d %q", resp.Code, resp.Body.String())
	}
	if got := upRec.count(); got != 2 {
		t.Fatalf("prediction after extension hit upstream: %d fetches", got)
	}
}

// TestAPTIndex304WithoutFreshnessDoesNotExtend is the other half: a 304 that
// states no freshness neither revives a stale generation nor schedules a
// tight retry loop.
func TestAPTIndex304WithoutFreshnessDoesNotExtend(t *testing.T) {
	clock := newTestClock()
	start := clock.Now()
	indexBody := ""
	upRec, up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/.mdb/apt-index.json") {
			writeResponse(w, r, http.StatusOK, "upstream-package", map[string]string{"Cache-Control": "max-age=60"})
			return
		}
		if r.Header.Get("If-None-Match") == `"gen-1"` {
			w.Header().Set("ETag", `"gen-1"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"gen-1"`)
		writeResponse(w, r, http.StatusOK, indexBody, nil)
	})

	s, store := newCacheServer(t, up.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{aptTestMirror()}})
	disablePrefetchJitter(s)
	content, err := store.Put(t.Context(), strings.NewReader("stored-package"), 14)
	if err != nil {
		t.Fatalf("put body: %v", err)
	}
	indexBody = aptIndexDocument(t, start, start.Add(time.Minute), aptFile{
		Path: "pool/main/s/s.deb", Size: content.Size, SHA256: content.SHA256, Kind: "package",
	})

	s.prefetch.NoteAPTResponse(aptTestMirror(), clock.Now(), false, nil)
	dispatchAndWait(s)
	clock.Advance(5 * time.Minute)
	s.prefetch.NoteAPTResponse(aptTestMirror(), clock.Now(), false, s.aptIndexes.generation("debian"))
	dispatchAndWait(s)

	gen := s.aptIndexes.generation("debian")
	if gen == nil || !gen.until.Equal(start.Add(time.Minute)) {
		t.Fatalf("304 without freshness changed the deadline: %+v", gen)
	}
	task := snapshotTask(t, s.prefetch, aptTaskKey("debian"))
	if want := clock.Now().Add(defaultAPTIndexTTL); !task.nextRun.Equal(want) {
		t.Fatalf("retry after a freshness-less 304 = %v, want %v", task.nextRun, want)
	}
	dispatchAndWait(s)
	if got := upRec.count(); got != 2 {
		t.Fatalf("a freshness-less 304 looped: %d fetches, want 2", got)
	}

	resp := cacheRequest(s, http.MethodGet, "/debian/pool/main/s/s.deb", nil)
	if resp.Body.String() != "upstream-package" {
		t.Fatalf("stale generation answered the package: %q", resp.Body.String())
	}
	if got := upRec.count(); got != 3 {
		t.Fatalf("stale generation fetches = %d, want 3 (index, 304, package)", got)
	}
}

// TestHashPredictionSkippedForClientRevalidation: a request that forces
// revalidation must reach the upstream, not be satisfied by the predicted body.
func TestHashPredictionSkippedForClientRevalidation(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"no-cache", map[string]string{"Cache-Control": "no-cache"}},
		{"max-age=0", map[string]string{"Cache-Control": "max-age=0"}},
		{"pragma", map[string]string{"Pragma": "no-cache"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				writeResponse(w, r, http.StatusOK, "upstream", map[string]string{"Cache-Control": "max-age=60"})
			})
			clock := newTestClock()
			s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{aptTestMirror()}})
			content, err := store.Put(t.Context(), strings.NewReader("stored-body"), 11)
			if err != nil {
				t.Fatalf("put body: %v", err)
			}
			installAPTFile(s, clock, content, "pool/main/x/x.deb")

			resp := cacheRequest(s, http.MethodGet, "/debian/pool/main/x/x.deb", c.headers)
			if resp.Code != http.StatusOK || resp.Body.String() != "upstream" {
				t.Fatalf("forced revalidation = %d %q, want the upstream body", resp.Code, resp.Body.String())
			}
			if got := up.count(); got != 1 {
				t.Fatalf("forced revalidation was answered by prediction: %d upstream fetches", got)
			}
		})
	}
}

// TestHashPredictionIgnoresMetadataKind: only a package entry may invalidate a
// stored variant. A metadata path (Packages, Release, ...) is a mutable object
// whose index hash describes the fetched metadata, not the client's cached body.
func TestHashPredictionIgnoresMetadataKind(t *testing.T) {
	const path = "dists/bookworm/main/binary-amd64/Packages"
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"p1"`)
		w.Header().Set("Cache-Control", "max-age=3600")
		writeResponse(w, r, http.StatusOK, "packages-v1", nil)
	})
	clock := newTestClock()
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{aptTestMirror()}})

	first := cacheRequest(s, http.MethodGet, "/debian/"+path, nil)
	if first.Body.String() != "packages-v1" {
		t.Fatalf("first body = %q", first.Body.String())
	}
	s.aptIndexes.set("debian", testAPTIndex(clock, time.Hour, aptFile{
		Path: path, Size: 11, SHA256: strings.Repeat("a", 64), Kind: "metadata",
	}))

	second := cacheRequest(s, http.MethodGet, "/debian/"+path, nil)
	if second.Body.String() != "packages-v1" {
		t.Fatalf("metadata variant was dropped and refetched: %q", second.Body.String())
	}
	if got := up.count(); got != 1 {
		t.Fatalf("metadata path refetched upstream: %d fetches", got)
	}
	variants, err := store.Variants(t.Context(), "mirror://debian/"+path)
	if err != nil {
		t.Fatalf("list variants: %v", err)
	}
	if len(variants) != 1 {
		t.Fatalf("metadata variant was dropped by the index: %d variants", len(variants))
	}
}

// TestAPTIndex200ResetsEffectiveExpiry: a 304 extension describes one document.
// A new 200 generation is authoritative for exactly its own expires_at, so a
// longer deadline inherited from the previous generation must not survive.
func TestAPTIndex200ResetsEffectiveExpiry(t *testing.T) {
	clock := newTestClock()
	start := clock.Now()
	indexBody := aptIndexDocument(t, start, start.Add(2*time.Minute), aptFile{
		Path: "pool/main/x/x.deb", Size: 1, SHA256: strings.Repeat("c", 64), Kind: "package",
	})
	_, up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, indexBody, nil)
	})
	s, _ := newCacheServer(t, up.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{aptTestMirror()}})
	disablePrefetchJitter(s)

	// An immediate task does not consult the table's current generation.
	s.prefetch.NoteAPTResponse(aptTestMirror(), clock.Now(), false, nil)
	// Pretend a 304 had extended the previous generation by an hour.
	s.aptIndexes.set("debian", testAPTIndex(clock, time.Minute))
	s.aptIndexes.extend("debian", start.Add(time.Hour))

	dispatchAndWait(s)

	gen := s.aptIndexes.generation("debian")
	if gen == nil || !gen.until.Equal(start.Add(2*time.Minute)) {
		t.Fatalf("200 generation deadline = %+v, want the document's expires_at", gen)
	}
}

// TestAPTIndexTaskRetargetsOnMountChange: a manifest reload can move a mirror's
// mount. The existing index task must follow it, with no validator or retry
// delay inherited from the old address.
func TestAPTIndexTaskRetargetsOnMountChange(t *testing.T) {
	clock := newTestClock()
	p := newTestPrefetchScheduler(clock, 2, 8, nil)
	original := aptTestMirror()

	p.NoteAPTResponse(original, clock.Now(), false, nil)
	key := aptTaskKey("debian")
	p.mu.Lock()
	task := p.tasks[key]
	task.backoff = 2
	task.nextRun = clock.Now().Add(15 * time.Minute)
	task.hasNext = true
	task.aptEtag = `"old-generation"`
	p.mu.Unlock()

	moved := Mirror{Name: "debian", Host: "debian"}
	p.NoteAPTResponse(moved, clock.Now(), false, nil)
	got := snapshotTask(t, p, key)
	if got.clientPath != "/.mdb/apt-index.json" || got.relPath != "/.mdb/apt-index.json" {
		t.Fatalf("retargeted paths = (%q, %q)", got.clientPath, got.relPath)
	}
	if got.mirror.Host != "debian" || got.mirror.Path != "" {
		t.Fatalf("retargeted mirror = %+v", got.mirror)
	}
	if got.backoff != 0 || !got.hasNext || !got.nextRun.Equal(clock.Now()) {
		t.Fatalf("retarget kept the old retry state: backoff %d, nextRun %v, hasNext %v", got.backoff, got.nextRun, got.hasNext)
	}
	if got.aptEtag != "" {
		t.Fatalf("retarget kept the old validator %q", got.aptEtag)
	}

	// A response for the same mount must not reset a pending retry.
	p.mu.Lock()
	task = p.tasks[key]
	task.backoff = 1
	task.nextRun = clock.Now().Add(5 * time.Minute)
	task.hasNext = true
	p.mu.Unlock()
	p.NoteAPTResponse(moved, clock.Now(), false, nil)
	got = snapshotTask(t, p, key)
	if got.backoff != 1 || !got.nextRun.Equal(clock.Now().Add(5*time.Minute)) {
		t.Fatalf("unchanged mount reset the retry: backoff %d, nextRun %v", got.backoff, got.nextRun)
	}

	// A run already in flight belongs to the old target; its outcome must not
	// take over the retargeted task.
	p.mu.Lock()
	stale := p.tasks[key]
	stale.running = true
	p.running = 1
	p.mu.Unlock()

	movedAgain := Mirror{Name: "debian", Path: "debian-archive"}
	p.NoteAPTResponse(movedAgain, clock.Now(), false, nil)
	if got := snapshotTask(t, p, key); got.clientPath != "/debian-archive/.mdb/apt-index.json" {
		t.Fatalf("second retarget = %q", got.clientPath)
	}

	p.finish(stale, prefetchOutcome{next: clock.Now().Add(time.Hour)})
	got = snapshotTask(t, p, key)
	if !got.hasNext || !got.nextRun.Equal(clock.Now()) {
		t.Fatalf("stale run rescheduled the retargeted task: nextRun %v, hasNext %v", got.nextRun, got.hasNext)
	}
	if got := p.running; got != 0 {
		t.Fatalf("running after the stale run finished = %d", got)
	}
}
