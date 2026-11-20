package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/geektr-cloud/go-cloudflare-fastest-transport/engine"
	"github.com/geektr-cloud/go-cloudflare-fastest-transport/pool"
)

// noopProber never admits anything: an engine built with it starts with two
// empty pools, which is exactly the "speed unavailable" case.
type noopProber struct{}

func (noopProber) Latency(context.Context, string, time.Duration) (time.Duration, bool) {
	return 0, false
}
func (noopProber) Speed(context.Context, string, time.Duration) (float64, bool) {
	return 0, false
}

func newTestEdge(t *testing.T) *edgeSelection {
	t.Helper()
	eng, err := engine.New(engine.Config{}, noopProber{}, nil)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return &edgeSelection{
		engine:  eng,
		latency: eng.LatencyTransport(),
		speed:   eng.SpeedTransport(),
		plain:   plainTransport(),
	}
}

// latency is the default; speed is used only when the speed pool has a member.
func TestTransportForSelectsPool(t *testing.T) {
	e := newTestEdge(t)

	if got := e.transportFor(trafficLatency); got != e.latency {
		t.Fatal("latency policy did not use the latency transport")
	}
	// No measurements yet: a speed preference must not fall back to plain DNS.
	if got := e.transportFor(trafficSpeed); got != e.latency {
		t.Fatal("empty speed pool did not fall back to latency")
	}

	if !e.engine.SpeedPool().Push(pool.ScoredIP{PoolID: 1, IP: "203.0.113.1", Score: 1 << 20}) {
		t.Fatal("could not seed the speed pool")
	}
	if got := e.transportFor(trafficSpeed); got != e.speed {
		t.Fatal("speed policy did not use the speed transport")
	}
	// Latency traffic must keep using the latency pool.
	if got := e.transportFor(trafficLatency); got != e.latency {
		t.Fatal("latency policy changed after the speed pool filled")
	}
}

// No engine means no edge selection: the plain transport serves everything,
// including speed-policy requests.
func TestTransportForWithoutEngine(t *testing.T) {
	e := &edgeSelection{plain: plainTransport()}
	if got := e.transportFor(trafficSpeed); got != e.plain {
		t.Fatal("speed policy did not use the plain fallback")
	}

	// A zero-value edgeSelection (as constructed before the engine starts) must
	// still return a usable transport rather than nil.
	if got := (&edgeSelection{}).transportFor(trafficLatency); got == nil {
		t.Fatal("nil transport")
	}
	if got := (*edgeSelection)(nil).transportFor(trafficLatency); got == nil {
		t.Fatal("nil edgeSelection returned nil transport")
	}
}

// captureTransport records the policy that reached the RoundTripper.
type captureTransport struct {
	policy trafficPolicy
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.policy = trafficPolicyFromContext(req.Context())
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

// handleRequest must carry the matched policy into the transport, and must not
// disturb the default for unmatched requests.
func TestHandleRequestCarriesPolicy(t *testing.T) {
	s := newPolicyTestServer(t,
		[]Mirror{{Name: "debian", Path: "debian", PolicyTags: []string{"apt"}}},
		Policy{Name: "apt-pool", Match: PolicyMatch{Tag: "apt", PathPrefix: "/pool/"}, Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
		Policy{Name: "default", Rules: PolicyRules{TrafficPolicy: trafficLatency}},
	)
	capture := &captureTransport{}
	s.edge = &edgeSelection{plain: capture}
	s.upstream = mustURL(t, "https://mirs.uk")
	s.proxy = s.newReverseProxy()
	s.ready.Store(true)

	cases := []struct {
		target string
		want   trafficPolicy
	}{
		{"/debian/pool/x.deb", trafficSpeed},
		{"/debian/dists/stable/Release", trafficLatency},
		{"/keys/apt/debian", trafficLatency},
	}
	for _, c := range cases {
		capture.policy = ""
		req := httptest.NewRequest(http.MethodGet, "http://relay.example.com"+c.target, nil)
		s.handleRequest(httptest.NewRecorder(), req)
		if capture.policy != c.want {
			t.Errorf("%s: transport saw %q, want %q", c.target, capture.policy, c.want)
		}
	}
}

// An explicit upstream proxy or SSH disguise has one fixed route, so the proxy
// must not be wrapped in policy routing at all.
func TestUpstreamTransportBypassesPolicyWithFixedRoute(t *testing.T) {
	s := newTestServer(t, "https://mirs.uk")
	fixed := plainTransport()
	s.client.Transport = fixed

	if got := s.upstreamTransport(); got != fixed {
		t.Fatal("fixed transport was replaced by policy routing")
	}

	s.edge = newTestEdge(t)
	got := s.upstreamTransport()
	if _, ok := got.(*policyTransport); !ok {
		t.Fatalf("edge selection not wrapped in policy routing: %T", got)
	}
}
