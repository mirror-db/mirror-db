package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/geektr-cloud/go-cloudflare-fastest-transport/pool"
)

// With edge selection off (proxy or disguise configured) the endpoint must say
// so, not return an empty pool that looks like "nothing measured yet".
func TestHandlePoolWithoutEngine(t *testing.T) {
	s := &Server{edge: &edgeSelection{}}

	rec := httptest.NewRecorder()
	s.handlePool(rec, httptest.NewRequest(http.MethodGet, "/.mirror-relay/pool", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] == "" {
		t.Fatalf("expected an explanation, got %v", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
}

func TestEdgeSelectionWithoutEngineServesPlainTransport(t *testing.T) {
	e := &edgeSelection{}

	if e.transport() == nil {
		t.Fatal("no transport")
	}
	if e.Snapshot() != nil {
		t.Fatal("snapshot should be nil without an engine")
	}

	// Close must be safe to call more than once and without an engine.
	e.Close()
	e.Close()
}

func TestDescribePoolPreservesOrder(t *testing.T) {
	p := pool.New(pool.Config{Capacity: 4, MinimumScore: 0})
	p.Push(pool.ScoredIP{PoolID: 1, IP: "10.0.0.1", Score: 900})
	p.Push(pool.ScoredIP{PoolID: 1, IP: "10.0.0.2", Score: 500})

	entries := describePool(p)
	if len(entries) != 2 {
		t.Fatalf("got %d entries", len(entries))
	}
	if entries[0].IP != "10.0.0.1" {
		t.Fatalf("best entry not first: %+v", entries)
	}
}
