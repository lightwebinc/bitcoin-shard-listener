// bitcoin-shard-listener receives IPv6 multicast BSV transaction frames,
// filters by shard and/or subtree, forwards matching frames to a configurable
// downstream unicast host:port over UDP or TCP, and performs NORM-inspired
// NACK-based gap recovery for BRC-124 frames.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/lightwebinc/bitcoin-shard-common/shard"

	"github.com/lightwebinc/bitcoin-shard-listener/config"
	"github.com/lightwebinc/bitcoin-shard-listener/discovery"
	"github.com/lightwebinc/bitcoin-shard-listener/egress"
	"github.com/lightwebinc/bitcoin-shard-listener/filter"
	"github.com/lightwebinc/bitcoin-shard-listener/listener"
	"github.com/lightwebinc/bitcoin-shard-listener/metrics"
	"github.com/lightwebinc/bitcoin-shard-listener/nack"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logLevel := slog.LevelInfo
	if cfg.Debug {
		logLevel = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))

	slog.Info("bitcoin-shard-listener starting",
		"shard_bits", cfg.ShardBits,
		"num_groups", cfg.NumGroups,
		"scope", cfg.MCScope,
		"listen_port", cfg.ListenPort,
		"egress_addr", cfg.EgressAddr,
		"egress_proto", cfg.EgressProto,
		"mc_egress_enabled", cfg.MCEgressEnabled,
		"workers", cfg.NumWorkers,
		"retry_endpoints", len(cfg.RetryEndpoints),
	)
	if cfg.MCEgressEnabled {
		slog.Info("multicast egress enabled",
			"iface", cfg.MCEgressIface.Name,
			"scope", cfg.MCEgressScope,
			"port", cfg.MCEgressPort,
			"hoplimit", cfg.MCEgressHopLimit,
		)
	}

	rec, err := metrics.New(cfg.InstanceID, cfg.NumWorkers, cfg.OTLPEndpoint, cfg.OTLPInterval)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}

	// Build the shard engine.
	engine := shard.New(cfg.MCPrefix, cfg.MCMiddleBytes, cfg.ShardBits)

	// Derive the multicast group addresses to join.
	groups, err := buildGroups(cfg, engine)
	if err != nil {
		return fmt.Errorf("build groups: %w", err)
	}
	slog.Info("multicast groups", "count", len(groups))

	// Build filter.
	filt := filter.New(cfg.ShardInclude, cfg.SubtreeInclude, cfg.SubtreeExclude)

	// Build the endpoint registry (beacon-discovered + static seeds).
	reg := discovery.NewRegistry()

	// Build NACK tracker.
	tracker := nack.New(
		nack.TrackerConfig{
			JitterMax:  cfg.NACKJitterMax,
			BackoffMax: cfg.NACKBackoffMax,
			MaxRetries: cfg.NACKMaxRetries,
			GapTTL:     cfg.NACKGapTTL,
		},
		cfg.RetryEndpoints,
		cfg.Iface,
		rec,
		reg,
	)

	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tracker.Start(ctx)

	// Start metrics server.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec.Serve(cfg.MetricsAddr, done)
	}()

	// Start beacon listener for dynamic endpoint discovery.
	if cfg.BeaconEnabled {
		beaconScopePrefix, ok := config.Scopes[cfg.BeaconScope]
		if !ok {
			beaconScopePrefix = 0xFF05
		}
		beaconIP := shard.ControlGroupAddr(beaconScopePrefix, cfg.MCMiddleBytes, shard.CtrlGroupBeacon)
		beaconGrp := &net.UDPAddr{IP: beaconIP, Port: cfg.BeaconPort}
		bl := &discovery.BeaconListener{
			Registry: reg,
			Groups:   []*net.UDPAddr{beaconGrp},
			Iface:    cfg.Iface,
			Debug:    cfg.Debug,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := bl.Start(ctx); err != nil && ctx.Err() == nil {
				slog.Error("beacon listener error", "err", err)
			}
		}()
		slog.Info("beacon listener started", "group", beaconIP, "port", cfg.BeaconPort)
	}

	// Start workers.
	for i := range cfg.NumWorkers {
		egr, err := egress.New(cfg.EgressAddr, cfg.EgressProto, cfg.StripHeader)
		if err != nil {
			return fmt.Errorf("egress worker %d: %w", i, err)
		}
		defer func() { _ = egr.Close() }()

		var mcastEgr *egress.MCastSender
		if cfg.MCEgressEnabled {
			mcastEgr, err = egress.NewMCast(
				cfg.MCEgressPrefix,
				cfg.MCEgressMiddleBytes,
				cfg.ShardBits,
				cfg.MCEgressPort,
				cfg.MCEgressIface,
				cfg.MCEgressHopLimit,
				cfg.StripHeader,
			)
			if err != nil {
				return fmt.Errorf("mc egress worker %d: %w", i, err)
			}
			defer func() { _ = mcastEgr.Close() }()
		}

		w := listener.New(i, cfg.Iface, cfg.ListenPort, groups, engine, filt, egr, mcastEgr, tracker, rec, cfg.Debug)
		wg.Add(1)
		go func(worker *listener.Worker) {
			defer wg.Done()
			if err := worker.Run(ctx); err != nil {
				slog.Error("worker exited with error", "err", err)
			}
		}(w)
	}

	// Wait for signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	slog.Info("shutdown signal received", "signal", sig)

	if cfg.DrainTimeout > 0 {
		rec.SetDraining()
		slog.Info("draining", "timeout", cfg.DrainTimeout)
		time.Sleep(cfg.DrainTimeout)
	}

	cancel()
	close(done)
	wg.Wait()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	rec.Shutdown(ctx2)

	slog.Info("shutdown complete")
	return nil
}

// buildGroups returns the multicast group addresses this instance should join.
// If ShardInclude is set, only those groups are joined; otherwise all groups.
func buildGroups(cfg *config.Config, engine *shard.Engine) ([]*net.UDPAddr, error) {
	var indices []uint32
	if len(cfg.ShardInclude) > 0 {
		indices = cfg.ShardInclude
	} else {
		indices = make([]uint32, cfg.NumGroups)
		for i := range indices {
			indices[i] = uint32(i)
		}
	}
	groups := make([]*net.UDPAddr, 0, len(indices))
	for _, idx := range indices {
		addr := engine.Addr(idx, cfg.ListenPort)
		groups = append(groups, addr)
	}
	return groups, nil
}
