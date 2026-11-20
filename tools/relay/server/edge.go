package server

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/geektr-cloud/go-cloudflare-fastest-transport/engine"
	"github.com/geektr-cloud/go-cloudflare-fastest-transport/netgate"
	"github.com/geektr-cloud/go-cloudflare-fastest-transport/pool"
	"github.com/geektr-cloud/go-cloudflare-fastest-transport/probe"
	"github.com/mirror-db/mirror-db/tools/relay/config"
)

// edgeSelection is the Cloudflare edge-selection engine, or a plain transport
// when it could not start.
type edgeSelection struct {
	engine *engine.Engine // nil when the engine is unavailable
	cancel context.CancelFunc
	gate   *netgate.Gate

	// One transport per pool, built once: the engine's transports are stateless
	// apart from their engine pointer, and keeping them stable makes the
	// per-request choice observable (and testable) by identity.
	latency http.RoundTripper
	speed   http.RoundTripper
	plain   http.RoundTripper // used when no engine is available

	closeOnce sync.Once
}

// newEdgeSelection builds and initializes the engine.
//
// Initialization blocks while it probes the network, so it is bounded by
// cf.init_timeout. A failure is logged rather than fatal: a mirror that can
// reach its upstream slowly is still a working mirror, and refusing to start
// would turn a measurement problem into an outage.
func newEdgeSelection() (*edgeSelection, http.RoundTripper) {
	e := &edgeSelection{plain: plainTransport()}

	pr, err := probe.New(probe.Config{
		LatencyURL: config.GetCfLatencyURL(),
		SpeedURL:   config.GetCfSpeedURL(),
	})
	if err != nil {
		log.Printf("edge selection disabled: %v", err)
		return e, e.plain
	}

	ctx, cancel := context.WithCancel(context.Background())
	gate := netgate.New(netgate.Config{})
	go gate.Run(ctx)

	eng, err := engine.New(engineConfig(), pr, gate)
	if err != nil {
		cancel()
		log.Printf("edge selection disabled: %v", err)
		return e, e.plain
	}

	initCtx, initCancel := context.WithTimeout(ctx, config.GetCfInitTimeout())
	defer initCancel()

	start := time.Now()
	if err := eng.Initialize(initCtx); err != nil {
		// Keep the engine: patrol and recruit run against the pools it has, and
		// a startup network hiccup should not disable acceleration for hours.
		log.Printf("edge selection not ready after %s: %v (continuing; pools will fill in background)",
			time.Since(start).Round(time.Millisecond), err)
	} else {
		log.Printf("edge selection ready in %s (%s)", time.Since(start).Round(time.Millisecond), eng.Describe())
	}

	go eng.Start(ctx)

	e.engine = eng
	e.cancel = cancel
	e.gate = gate
	e.latency = eng.LatencyTransport()
	e.speed = eng.SpeedTransport()
	return e, e.transport()
}

// engineConfig maps relay configuration onto the engine's.
func engineConfig() engine.Config {
	cfg := engine.Config{
		TopN:                  config.GetCfTopN(),
		SeedCount:             config.GetCfSeedCount(),
		SpeedSeedCount:        config.GetCfSpeedSeedCount(),
		Penalty:               config.GetCfPenalty(),
		PatrolLatencyInterval: config.GetCfPatrolLatencyInterval(),
		PatrolSpeedInterval:   config.GetCfPatrolSpeedInterval(),

		Intensity:        config.GetCfIntensity(),
		Interval:         config.GetCfInterval(),
		SnapshotPath:     config.GetCfCandidatesFile(),
		SnapshotInterval: config.GetCfSnapshotInterval(),
		BootstrapHost:    config.GetCfBootstrapHost(),

		Logf: log.Printf,
	}

	// Set the fields individually: a pool is allowed to configure only its floor
	// or only its capacity, and the engine fills in whatever is left at zero.
	cfg.Latency.Capacity = config.GetCfLatencyCapacity()
	cfg.Latency.MinimumScore = config.GetCfLatencyMinScore()

	cfg.Speed.Capacity = config.GetCfSpeedCapacity()
	cfg.Speed.MinimumScore = config.GetCfSpeedMinScore()

	return cfg
}

// transport returns the RoundTripper a latency request uses, for callers that
// need "the default" without a request in hand.
func (e *edgeSelection) transport() http.RoundTripper {
	return e.transportFor(trafficLatency)
}

// transportFor picks the edge pool for one request.
//
// Traffic policy is a preference, not an availability requirement: an empty
// speed pool falls back to latency rather than to an unoptimised route, and a
// missing engine (disabled or failed to start) falls back to plain DNS.
func (e *edgeSelection) transportFor(p trafficPolicy) http.RoundTripper {
	if e == nil {
		return plainTransport()
	}
	if e.engine == nil || e.latency == nil {
		if e.plain != nil {
			return e.plain
		}
		return plainTransport()
	}
	if p == trafficSpeed && e.engine.SpeedPool().Len() > 0 {
		return e.speed
	}
	return e.latency
}

// Close stops the maintenance loops and releases pooled connections.
func (e *edgeSelection) Close() {
	e.closeOnce.Do(func() {
		if e.cancel != nil {
			e.cancel()
		}
		if e.engine != nil {
			e.engine.Close()
		}
	})
}

// Snapshot renders the edge selection state for diagnostics. nil means the
// engine never started.
func (e *edgeSelection) Snapshot() *edgeSnapshot {
	if e.engine == nil {
		return nil
	}

	snap := &edgeSnapshot{
		// There is no single serving pool: the distributed policy list chooses
		// per request, and an unavailable speed pool falls back to latency.
		Pool:     "per-request",
		TopN:     e.engine.Config().TopN,
		Latency:  describePool(e.engine.LatencyPool()),
		Speed:    describePool(e.engine.SpeedPool()),
		GateOpen: e.gate == nil || e.gate.Open(),
	}
	if e.gate != nil {
		snap.GateTarget = e.gate.Target()
	}
	return snap
}

func describePool(p *pool.Pool) []edgeEntry {
	entries := p.All()
	out := make([]edgeEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, edgeEntry{IP: e.IP, PoolID: uint32(e.PoolID), Score: e.Score})
	}
	return out
}

// edgeSnapshot is the JSON shape of /.mirror-relay/pool.
type edgeSnapshot struct {
	// Pool is which pool the proxy draws from, so a reader knows which list is
	// the one actually serving traffic.
	Pool       string      `json:"pool"`
	TopN       int         `json:"top_n"`
	GateOpen   bool        `json:"gate_open"`
	GateTarget string      `json:"gate_target,omitempty"`
	Latency    []edgeEntry `json:"latency"`
	Speed      []edgeEntry `json:"speed"`
}

type edgeEntry struct {
	IP     string  `json:"ip"`
	PoolID uint32  `json:"pool_id"`
	Score  float64 `json:"score"`
	// LatencyMS converts a latency score back into the figure an operator
	// thinks in; omitted for speed entries.
	LatencyMS float64 `json:"latency_ms,omitempty"`
}

// plainTransport is used when edge selection is off or unavailable: connections
// go wherever DNS says, unoptimised but working.
func plainTransport() http.RoundTripper {
	return &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
}
