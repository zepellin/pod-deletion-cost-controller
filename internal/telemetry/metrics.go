package telemetry

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Recorder holds all Prometheus metrics for the controller and exposes
// a /metrics HTTP handler backed by a private registry.
//
// It also tracks enough liveness state (whether this replica is leading and
// when it last completed a sync) to answer the health endpoints. All state is
// written from the sync goroutine and read from HTTP handler goroutines.
type Recorder struct {
	reg                *prometheus.Registry
	syncDuration       prometheus.Histogram
	syncCycles         *prometheus.CounterVec
	podsManaged        *prometheus.GaugeVec
	annotationPatches  *prometheus.CounterVec
	metricsUnavailable *prometheus.CounterVec
	leader             prometheus.Gauge
	lastSync           prometheus.Gauge

	leading      atomic.Bool
	lastSyncNano atomic.Int64
}

func NewRecorder() *Recorder {
	reg := prometheus.NewRegistry()

	// Standard Go runtime and process metrics.
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	syncDuration := prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "pdcc",
		Name:      "sync_duration_seconds",
		Help:      "Duration in seconds of a full sync cycle across all targets.",
		Buckets:   prometheus.DefBuckets,
	})
	syncCycles := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "pdcc",
		Name:      "sync_cycles_total",
		Help:      "Total sync cycles completed, labelled by result (success or partial_error).",
	}, []string{"result"})
	podsManaged := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "pdcc",
		Name:      "pods_managed",
		Help:      "Current number of Running pods under management, by namespace and cost class (idle, busy, no_metrics).",
	}, []string{"namespace", "cost_class"})
	annotationPatches := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "pdcc",
		Name:      "annotation_patches_total",
		Help:      "Total pod-deletion-cost annotation patches attempted, by namespace and result (updated, gone or error).",
	}, []string{"namespace", "result"})
	metricsUnavailable := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "pdcc",
		Name:      "metrics_unavailable_total",
		Help:      "Total pods for which CPU metrics were unavailable, by namespace and reason (not_found or error).",
	}, []string{"namespace", "reason"})
	leader := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "pdcc",
		Name:      "leader",
		Help:      "1 if this replica is currently running the sync loop, 0 if it is standing by.",
	})
	lastSync := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "pdcc",
		Name:      "last_sync_timestamp_seconds",
		Help:      "Unix timestamp of the last completed sync cycle.",
	})

	reg.MustRegister(syncDuration, syncCycles, podsManaged, annotationPatches, metricsUnavailable, leader, lastSync)

	r := &Recorder{
		reg:                reg,
		syncDuration:       syncDuration,
		syncCycles:         syncCycles,
		podsManaged:        podsManaged,
		annotationPatches:  annotationPatches,
		metricsUnavailable: metricsUnavailable,
		leader:             leader,
		lastSync:           lastSync,
	}
	// Seed the clock at construction so a replica that has not yet completed its
	// first sync is judged against process start, not against the zero time.
	r.lastSyncNano.Store(time.Now().UnixNano())
	return r
}

// Handler returns an HTTP handler that serves Prometheus metrics.
func (r *Recorder) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{})
}

// ObserveSyncCycle records the duration and result of a completed sync cycle.
func (r *Recorder) ObserveSyncCycle(d time.Duration, hadErrors bool) {
	r.syncDuration.Observe(d.Seconds())
	result := "success"
	if hadErrors {
		result = "partial_error"
	}
	r.syncCycles.WithLabelValues(result).Inc()

	now := time.Now()
	r.lastSyncNano.Store(now.UnixNano())
	r.lastSync.Set(float64(now.Unix()))
}

// SetLeading records whether this replica is running the sync loop. Becoming
// the leader resets the staleness clock, since the new leader has not had a
// chance to sync yet.
func (r *Recorder) SetLeading(leading bool) {
	r.leading.Store(leading)
	if leading {
		r.leader.Set(1)
		r.lastSyncNano.Store(time.Now().UnixNano())
		return
	}
	r.leader.Set(0)
}

// CheckLiveness reports whether the sync loop is making progress. A replica
// that is standing by (not the leader) is always considered live, because it
// is not expected to sync at all. Returns nil when healthy.
func (r *Recorder) CheckLiveness(staleAfter time.Duration) error {
	if !r.leading.Load() {
		return nil
	}
	since := time.Since(time.Unix(0, r.lastSyncNano.Load()))
	if since > staleAfter {
		return fmt.Errorf("no sync cycle completed in %s (limit %s)", since.Truncate(time.Second), staleAfter)
	}
	return nil
}

// RecordAnnotationPatch increments the patch counter. result is "updated" or "error".
func (r *Recorder) RecordAnnotationPatch(namespace, result string) {
	r.annotationPatches.WithLabelValues(namespace, result).Inc()
}

// RecordMetricsUnavailable increments the unavailability counter. reason is "not_found" or "error".
func (r *Recorder) RecordMetricsUnavailable(namespace, reason string) {
	r.metricsUnavailable.WithLabelValues(namespace, reason).Inc()
}

// ResetPodsManaged clears all pods_managed gauge series so stale namespaces
// (e.g. after a target is removed from config) don't linger.
func (r *Recorder) ResetPodsManaged() {
	r.podsManaged.Reset()
}

// SetPodsManaged sets the gauge for a specific (namespace, cost_class) pair.
func (r *Recorder) SetPodsManaged(namespace, costClass string, count float64) {
	r.podsManaged.WithLabelValues(namespace, costClass).Set(count)
}
