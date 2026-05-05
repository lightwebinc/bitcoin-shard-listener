// Package metrics initialises an OpenTelemetry MeterProvider backed by both
// a Prometheus exporter (for scraping) and an optional OTLP gRPC exporter
// (for push-based delivery to any OTel-compatible backend).
//
// All instrument handles are allocated once at [New] time. Record methods use
// them directly — no map lookups on the critical path.
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	prometheusexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ServiceName is the OTel service.name resource attribute value.
const ServiceName = "bitcoin-shard-listener"

// Version is set at build time via -ldflags "-X metrics.Version=<ver>".
var Version = "dev"

// Recorder holds all pre-allocated OTel instrument handles and readiness state.
type Recorder struct {
	provider   *sdkmetric.MeterProvider
	promReg    promclient.Gatherer
	numWorkers int
	startTime  time.Time
	readyCount atomic.Int32
	draining   atomic.Bool
	shutdownFn func(context.Context) error

	// Ingress counters
	framesReceived  metric.Int64Counter // by worker, iface, version
	framesDropped   metric.Int64Counter // by reason
	framesForwarded metric.Int64Counter // by worker, proto
	egressErrors    metric.Int64Counter

	// Multicast egress counters
	mcEgressErrors metric.Int64Counter

	// NACK / gap counters
	gapsDetected     metric.Int64Counter
	gapsSuppressed   metric.Int64Counter // cancelled by retransmit fill or ACK response
	nacksDispatched  metric.Int64Counter
	nacksUnrecovered metric.Int64Counter // retries exhausted or TTL exceeded
}

// New constructs and returns a Recorder.
func New(instanceID string, numWorkers int, otlpEndpoint string, otlpInterval time.Duration) (*Recorder, error) {
	if instanceID == "" {
		h, err := os.Hostname()
		if err != nil {
			h = "unknown"
		}
		instanceID = h
	}

	res, err := resource.New(context.Background(),
		resource.WithAttributes(
			attribute.String("service.name", ServiceName),
			attribute.String("service.instance.id", instanceID),
			attribute.String("service.version", Version),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: build resource: %w", err)
	}

	reg := promclient.NewRegistry()
	promExp, err := prometheusexporter.New(prometheusexporter.WithRegisterer(reg))
	if err != nil {
		return nil, fmt.Errorf("metrics: prometheus exporter: %w", err)
	}

	runtimeReg := promclient.NewRegistry()
	runtimeReg.MustRegister(collectors.NewGoCollector())
	runtimeReg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	mpOpts := []sdkmetric.Option{
		sdkmetric.WithReader(promExp),
		sdkmetric.WithResource(res),
	}

	var shutdownFuncs []func(context.Context) error

	if otlpEndpoint != "" {
		otlpExp, oerr := otlpmetricgrpc.New(
			context.Background(),
			otlpmetricgrpc.WithEndpoint(otlpEndpoint),
			otlpmetricgrpc.WithInsecure(),
		)
		if oerr != nil {
			return nil, fmt.Errorf("metrics: OTLP exporter: %w", oerr)
		}
		mpOpts = append(mpOpts, sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(otlpExp, sdkmetric.WithInterval(otlpInterval)),
		))
		shutdownFuncs = append(shutdownFuncs, otlpExp.Shutdown)
		slog.Info("OTLP exporter enabled", "endpoint", otlpEndpoint, "interval", otlpInterval)
	}

	mp := sdkmetric.NewMeterProvider(mpOpts...)
	shutdownFuncs = append(shutdownFuncs, mp.Shutdown)

	r := &Recorder{
		provider:   mp,
		promReg:    promclient.Gatherers{reg, runtimeReg},
		numWorkers: numWorkers,
		startTime:  time.Now(),
		shutdownFn: func(ctx context.Context) error {
			var last error
			for _, fn := range shutdownFuncs {
				if err := fn(ctx); err != nil {
					last = err
				}
			}
			return last
		},
	}

	meter := mp.Meter(ServiceName)

	if r.framesReceived, err = meter.Int64Counter("bsl_frames_received_total",
		metric.WithDescription("Multicast frames received")); err != nil {
		return nil, err
	}
	if r.framesDropped, err = meter.Int64Counter("bsl_frames_dropped_total",
		metric.WithDescription("Frames dropped before egress")); err != nil {
		return nil, err
	}
	if r.framesForwarded, err = meter.Int64Counter("bsl_frames_forwarded_total",
		metric.WithDescription("Frames forwarded to downstream unicast")); err != nil {
		return nil, err
	}
	if r.egressErrors, err = meter.Int64Counter("bsl_egress_errors_total",
		metric.WithDescription("Errors sending to downstream")); err != nil {
		return nil, err
	}
	if r.mcEgressErrors, err = meter.Int64Counter("bsl_mc_egress_errors_total",
		metric.WithDescription("Errors sending to multicast egress")); err != nil {
		return nil, err
	}
	if r.gapsDetected, err = meter.Int64Counter("bsl_gaps_detected_total",
		metric.WithDescription("Sequence gaps detected (missing frames)")); err != nil {
		return nil, err
	}
	if r.gapsSuppressed, err = meter.Int64Counter("bsl_gaps_suppressed_total",
		metric.WithDescription("Gaps cancelled by retransmit fill or ACK response")); err != nil {
		return nil, err
	}
	if r.nacksDispatched, err = meter.Int64Counter("bsl_nacks_dispatched_total",
		metric.WithDescription("NACK datagrams sent to retry endpoints")); err != nil {
		return nil, err
	}
	if r.nacksUnrecovered, err = meter.Int64Counter("bsl_gaps_unrecovered_total",
		metric.WithDescription("Gaps evicted after retries exhausted or TTL exceeded")); err != nil {
		return nil, err
	}

	return r, nil
}

// FrameReceived records receipt of a multicast frame.
// version should be "v1" or "v2".
func (r *Recorder) FrameReceived(workerID int, iface, version string) {
	r.framesReceived.Add(context.Background(), 1, metric.WithAttributes(
		attribute.Int("worker", workerID),
		attribute.String("network.interface.name", iface),
		attribute.String("version", version),
	))
}

// FrameDropped records a dropped frame.
// reason: "decode_error", "shard_filter", "subtree_filter".
func (r *Recorder) FrameDropped(workerID int, reason string) {
	r.framesDropped.Add(context.Background(), 1, metric.WithAttributes(
		attribute.Int("worker", workerID),
		attribute.String("reason", reason),
	))
}

// FrameForwarded records a successfully forwarded frame.
func (r *Recorder) FrameForwarded(workerID int, proto string) {
	r.framesForwarded.Add(context.Background(), 1, metric.WithAttributes(
		attribute.Int("worker", workerID),
		attribute.String("proto", proto),
	))
}

// EgressError records a send failure to downstream.
func (r *Recorder) EgressError(workerID int) {
	r.egressErrors.Add(context.Background(), 1, metric.WithAttributes(
		attribute.Int("worker", workerID),
	))
}

// MCEgressError records a send failure on the multicast egress path.
func (r *Recorder) MCEgressError(workerID int) {
	r.mcEgressErrors.Add(context.Background(), 1, metric.WithAttributes(
		attribute.Int("worker", workerID),
	))
}

// GapDetected records a newly detected sequence gap.
func (r *Recorder) GapDetected() {
	r.gapsDetected.Add(context.Background(), 1)
}

// GapSuppressed records a gap cancelled by a retransmit fill or ACK response.
func (r *Recorder) GapSuppressed() {
	r.gapsSuppressed.Add(context.Background(), 1)
}

// NACKDispatched records a NACK datagram sent to a retry endpoint.
func (r *Recorder) NACKDispatched() {
	r.nacksDispatched.Add(context.Background(), 1)
}

// GapUnrecovered records a gap evicted after retries exhausted or TTL exceeded.
func (r *Recorder) GapUnrecovered() {
	r.nacksUnrecovered.Add(context.Background(), 1)
}

// WorkerReady signals a worker has entered its receive loop.
func (r *Recorder) WorkerReady() { r.readyCount.Add(1) }

// WorkerDone signals a worker has exited its receive loop.
func (r *Recorder) WorkerDone() { r.readyCount.Add(-1) }

// SetDraining marks the recorder as draining; /readyz returns 503.
func (r *Recorder) SetDraining() { r.draining.Store(true) }

// Shutdown flushes all pending OTLP exports and releases SDK resources.
func (r *Recorder) Shutdown(ctx context.Context) {
	if err := r.shutdownFn(ctx); err != nil {
		slog.Warn("metrics shutdown error", "err", err)
	}
}

// Serve starts the HTTP metrics server on addr.
func (r *Recorder) Serve(addr string, done <-chan struct{}) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(r.promReg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", r.handleHealthz)
	mux.HandleFunc("/readyz", r.handleReadyz)

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		slog.Info("metrics server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("metrics server error", "err", err)
		}
	}()
	<-done
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Warn("metrics server shutdown error", "err", err)
	}
}

func (r *Recorder) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"status":"ok","uptime_seconds":%.1f}`, time.Since(r.startTime).Seconds())
}

func (r *Recorder) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	ready := int(r.readyCount.Load())
	total := r.numWorkers
	w.Header().Set("Content-Type", "application/json")
	if r.draining.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, `{"status":"draining","workers_ready":%d,"workers_total":%d}`, ready, total)
		return
	}
	if ready >= total && total > 0 {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"status":"ready","workers_ready":%d,"workers_total":%d}`, ready, total)
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = fmt.Fprintf(w, `{"status":"starting","workers_ready":%d,"workers_total":%d}`, ready, total)
}
