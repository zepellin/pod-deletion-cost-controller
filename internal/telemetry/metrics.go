package telemetry

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Recorder holds all Prometheus metrics for the controller and exposes
// a /metrics HTTP handler backed by a private registry.
type Recorder struct {
	reg                *prometheus.Registry
	syncDuration       prometheus.Histogram
	syncCycles         *prometheus.CounterVec
	podsManaged        *prometheus.GaugeVec
	annotationPatches  *prometheus.CounterVec
	metricsUnavailable *prometheus.CounterVec
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
		Help:      "Total pod-deletion-cost annotation patches attempted, by namespace and result (updated or error).",
	}, []string{"namespace", "result"})
	metricsUnavailable := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "pdcc",
		Name:      "metrics_unavailable_total",
		Help:      "Total pods for which CPU metrics were unavailable, by namespace and reason (not_found or error).",
	}, []string{"namespace", "reason"})

	reg.MustRegister(syncDuration, syncCycles, podsManaged, annotationPatches, metricsUnavailable)

	return &Recorder{
		reg:                reg,
		syncDuration:       syncDuration,
		syncCycles:         syncCycles,
		podsManaged:        podsManaged,
		annotationPatches:  annotationPatches,
		metricsUnavailable: metricsUnavailable,
	}
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
