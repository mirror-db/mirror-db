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

	closeOnce sync.Once
}

// newEdgeSelection builds and initializes the engine.
//
// Initialization blocks while it probes the network, so it is bounded by
// cf.init_timeout. A failure is logged rather than fatal: a mirror that can
// reach its upstream slowly is still a working mirror, and refusing to start
// would turn a measurement problem into an outage.
func newEdgeSelection() (*edgeSelection, http.RoundTripper) {
	pr, err := probe.New(probe.Config{
		LatencyURL: config.GetCfLatencyURL(),
		SpeedURL:   config.GetCfSpeedURL(),
	})
	if err != nil {
		log.Printf("edge selection disabled: %v", err)
		return &edgeSelection{}, plainTransport()
	}

	ctx, cancel := context.WithCancel(context.Background())
	gate := netgate.New(netgate.Config{})
	go gate.Run(ctx)

	eng, err := engine.New(engineConfig(), pr, gate)
	if err != nil {
		cancel()
		log.Printf("edge selection disabled: %v", err)
		return &edgeSelection{}, plainTransport()
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

	e := &edgeSelection{engine: eng, cancel: cancel, gate: gate}
	return e, e.transport()
}

// engineConfig maps relay configuration onto the engine's.
func engineConfig() engine.Config {
	cfg := engine.Config{
		TopN:                  config.GetCfTopN(),
		SeedCount:             config.GetCfSeedCount(),
		SpeedSeedCount:        config.GetCfSpeedSeedCount(),
		Penalty:               config.GetCfPenalty(),
		ScanSamples:           config.GetCfScanSamples(),
		RecruitSamples:        config.GetCfRecruitSamples(),
		ScanInterval:          config.GetCfScanInterval(),
		PatrolLatencyInterval: config.GetCfPatrolLatencyInterval(),
		PatrolSpeedInterval:   config.GetCfPatrolSpeedInterval(),
		RecruitInterval:       config.GetCfRecruitInterval(),
	}

	if n := config.GetCfPoolCapacity(); n > 0 {
		cfg.Latency = pool.Config{Capacity: n, MinimumScore: config.GetCfLatencyMinScore(), MaxPerPool: 4}
	}
	if n := config.GetCfSpeedCapacity(); n > 0 {
		cfg.Speed = pool.Config{Capacity: n, MinimumScore: config.GetCfSpeedMinScore(), MaxPerPool: 2}
	}
	return cfg
}

// transport returns the RoundTripper backed by the chosen pool.
func (e *edgeSelection) transport() http.RoundTripper {
	if e.engine == nil {
		return plainTransport()
	}
	if config.GetUpstreamTransport() == "speed" {
		return e.engine.SpeedTransport()
	}
	return e.engine.LatencyTransport()
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
		Pool:     config.GetUpstreamTransport(),
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
