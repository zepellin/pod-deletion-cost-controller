package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	"github.com/zepellin/pod-deletion-cost-controller/internal/config"
	"github.com/zepellin/pod-deletion-cost-controller/internal/metrics"
	"github.com/zepellin/pod-deletion-cost-controller/internal/strategy"
	"github.com/zepellin/pod-deletion-cost-controller/internal/telemetry"
)

const annotationDeletionCost = "controller.kubernetes.io/pod-deletion-cost"

// patchBackoff is used when the API server signals it is overloaded (429 / 503 / timeout).
// Five attempts: ~200 ms → 400 ms → 800 ms → 1.6 s → 3.2 s, capped at 8 s.
var patchBackoff = wait.Backoff{
	Duration: 200 * time.Millisecond,
	Factor:   2.0,
	Jitter:   0.1,
	Steps:    5,
	Cap:      8 * time.Second,
}

func isRetryablePatchErr(err error) bool {
	return kerrors.IsTooManyRequests(err) ||
		kerrors.IsServerTimeout(err) ||
		kerrors.IsServiceUnavailable(err)
}

// syncStats accumulates per-sync telemetry across all targets.
type syncStats struct {
	// pods maps namespace → cost_class → pod count for the current cycle.
	pods   map[string]map[string]int
	errors bool
}

func (s *syncStats) recordPod(namespace, costClass string) {
	if s.pods[namespace] == nil {
		s.pods[namespace] = make(map[string]int)
	}
	s.pods[namespace][costClass]++
}

// Syncer periodically reconciles pod-deletion-cost annotations for all configured targets.
type Syncer struct {
	k8s        kubernetes.Interface
	metrics    metrics.Getter
	cfg        *config.Config
	log        *slog.Logger
	strategies []strategy.Strategy
	rec        *telemetry.Recorder
}

func New(k8s kubernetes.Interface, mc metrics.Getter, cfg *config.Config, log *slog.Logger, rec *telemetry.Recorder) *Syncer {
	strategies := make([]strategy.Strategy, len(cfg.Targets))
	for i, t := range cfg.Targets {
		strategies[i] = strategy.New(t, cfg)
	}
	return &Syncer{k8s: k8s, metrics: mc, cfg: cfg, log: log, strategies: strategies, rec: rec}
}

// SyncOnce runs a single reconciliation pass across all targets.
// Useful for testing; the Run loop calls this on each tick.
func (s *Syncer) SyncOnce(ctx context.Context) {
	s.syncAll(ctx)
}

// Run blocks, syncing on every tick, until ctx is cancelled.
func (s *Syncer) Run(ctx context.Context) error {
	s.log.Info("syncer started", "interval", s.cfg.SyncInterval, "targets", len(s.cfg.Targets))

	s.syncAll(ctx)

	ticker := time.NewTicker(s.cfg.SyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.syncAll(ctx)
		}
	}
}

func (s *Syncer) syncAll(ctx context.Context) {
	start := time.Now()
	stats := &syncStats{pods: make(map[string]map[string]int)}

	for i, target := range s.cfg.Targets {
		if err := s.syncTarget(ctx, target, s.strategies[i], stats); err != nil {
			stats.errors = true
			s.log.Error("target sync failed",
				"namespace", target.Namespace,
				"selector", target.LabelSelector,
				"err", err)
		}
	}

	// Rebuild the pods_managed gauge from scratch each cycle so removed targets
	// or pods don't leave stale label sets behind.
	s.rec.ResetPodsManaged()
	for ns, classes := range stats.pods {
		for class, count := range classes {
			s.rec.SetPodsManaged(ns, class, float64(count))
		}
	}
	s.rec.ObserveSyncCycle(time.Since(start), stats.errors)
}

func (s *Syncer) syncTarget(ctx context.Context, t config.Target, strat strategy.Strategy, stats *syncStats) error {
	pods, err := s.k8s.CoreV1().Pods(t.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: t.LabelSelector,
	})
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if err := s.syncPod(ctx, pod, t, strat, stats); err != nil {
			stats.errors = true
			s.log.Error("pod sync failed",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"err", err)
		}
	}
	return nil
}

func (s *Syncer) syncPod(ctx context.Context, pod *corev1.Pod, t config.Target, strat strategy.Strategy, stats *syncStats) error {
	log := s.log.With("pod", pod.Name, "namespace", pod.Namespace)

	// Skip pods not in the Running phase — Pending/Succeeded/Failed/Unknown pods
	// are either not stable or done; don't interfere with their lifecycle.
	if pod.Status.Phase != corev1.PodRunning {
		log.Debug("skip: pod not running", "phase", pod.Status.Phase)
		return nil
	}

	// Skip pods already being terminated.
	if pod.DeletionTimestamp != nil {
		log.Debug("skip: pod terminating")
		return nil
	}

	cost, costClass := s.decideCost(ctx, pod, t, strat, log)
	stats.recordPod(pod.Namespace, costClass)
	return s.setAnnotation(ctx, pod, cost, log)
}

func (s *Syncer) decideCost(ctx context.Context, pod *corev1.Pod, t config.Target, strat strategy.Strategy, log *slog.Logger) (int32, string) {
	cpu, err := s.metrics.PodCPU(ctx, pod.Namespace, pod.Name, t.Containers)
	if err != nil {
		// NotFound means metrics-server hasn't scraped this pod yet (typical right after
		// a pod starts). Any other error is transient. In both cases protect the pod.
		if kerrors.IsNotFound(err) {
			s.rec.RecordMetricsUnavailable(pod.Namespace, "not_found")
			log.Debug("metrics not yet available")
		} else {
			s.rec.RecordMetricsUnavailable(pod.Namespace, "error")
			log.Warn("metrics unavailable", "err", err)
		}
		cost := strat.Decide(pod, nil)
		log.Debug("cost decided (no metrics)", "cost", cost)
		return cost, "no_metrics"
	}

	var costClass string
	if cpu.Cmp(s.cfg.BusyCPUThreshold) > 0 {
		costClass = "busy"
	} else {
		costClass = "idle"
	}

	cost := strat.Decide(pod, &cpu)
	log.Debug("cost decided", "cpu", cpu.String(), "cost", cost)
	return cost, costClass
}

func (s *Syncer) setAnnotation(ctx context.Context, pod *corev1.Pod, cost int32, log *slog.Logger) error {
	desired := strconv.FormatInt(int64(cost), 10)
	if pod.Annotations[annotationDeletionCost] == desired {
		return nil
	}

	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, annotationDeletionCost, desired)

	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, patchBackoff, func(ctx context.Context) (bool, error) {
		_, lastErr = s.k8s.CoreV1().Pods(pod.Namespace).Patch(
			ctx, pod.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{},
		)
		if lastErr == nil {
			return true, nil
		}
		if isRetryablePatchErr(lastErr) {
			log.Warn("patch throttled, retrying", "err", lastErr)
			return false, nil
		}
		return false, lastErr
	})
	if wait.Interrupted(err) {
		s.rec.RecordAnnotationPatch(pod.Namespace, "error")
		return fmt.Errorf("patch annotation: retries exhausted: %w", lastErr)
	}
	if err != nil {
		s.rec.RecordAnnotationPatch(pod.Namespace, "error")
		return fmt.Errorf("patch annotation: %w", err)
	}

	s.rec.RecordAnnotationPatch(pod.Namespace, "updated")
	log.Info("annotation updated",
		"from", pod.Annotations[annotationDeletionCost],
		"to", desired)
	return nil
}
