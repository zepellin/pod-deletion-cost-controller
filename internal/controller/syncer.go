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
	"k8s.io/client-go/kubernetes"

	"github.com/mdanko/pod-deletion-cost-controller/internal/config"
	"github.com/mdanko/pod-deletion-cost-controller/internal/metrics"
)

const annotationDeletionCost = "controller.kubernetes.io/pod-deletion-cost"

// Syncer periodically reconciles pod-deletion-cost annotations for all configured targets.
type Syncer struct {
	k8s     kubernetes.Interface
	metrics metrics.Getter
	cfg     *config.Config
	log     *slog.Logger
}

func New(k8s kubernetes.Interface, mc metrics.Getter, cfg *config.Config, log *slog.Logger) *Syncer {
	return &Syncer{k8s: k8s, metrics: mc, cfg: cfg, log: log}
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
	for _, target := range s.cfg.Targets {
		if err := s.syncTarget(ctx, target); err != nil {
			s.log.Error("target sync failed",
				"namespace", target.Namespace,
				"selector", target.LabelSelector,
				"err", err)
		}
	}
}

func (s *Syncer) syncTarget(ctx context.Context, t config.Target) error {
	pods, err := s.k8s.CoreV1().Pods(t.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: t.LabelSelector,
	})
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if err := s.syncPod(ctx, pod, t); err != nil {
			s.log.Error("pod sync failed",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"err", err)
		}
	}
	return nil
}

func (s *Syncer) syncPod(ctx context.Context, pod *corev1.Pod, t config.Target) error {
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

	cost := s.decideCost(ctx, pod, t, log)
	return s.setAnnotation(ctx, pod, cost, log)
}

func (s *Syncer) decideCost(ctx context.Context, pod *corev1.Pod, t config.Target, log *slog.Logger) int32 {
	cpu, err := s.metrics.PodCPU(ctx, pod.Namespace, pod.Name, t.Containers)
	if err != nil {
		// NotFound means metrics-server hasn't scraped this pod yet (typical right after
		// a pod starts). Any other error is transient. In both cases protect the pod.
		if kerrors.IsNotFound(err) {
			log.Debug("metrics not yet available, protecting pod", "cost", s.cfg.NoMetricsCost)
		} else {
			log.Warn("metrics unavailable, protecting pod", "err", err, "cost", s.cfg.NoMetricsCost)
		}
		return s.cfg.NoMetricsCost
	}

	if cpu.Cmp(s.cfg.BusyCPUThreshold) > 0 {
		log.Debug("pod busy", "cpu", cpu.String(), "threshold", s.cfg.BusyCPUThreshold.String(), "cost", s.cfg.BusyCost)
		return s.cfg.BusyCost
	}

	log.Debug("pod idle", "cpu", cpu.String(), "threshold", s.cfg.BusyCPUThreshold.String(), "cost", s.cfg.IdleCost)
	return s.cfg.IdleCost
}

func (s *Syncer) setAnnotation(ctx context.Context, pod *corev1.Pod, cost int32, log *slog.Logger) error {
	desired := strconv.FormatInt(int64(cost), 10)
	if pod.Annotations[annotationDeletionCost] == desired {
		return nil
	}

	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, annotationDeletionCost, desired)
	_, err := s.k8s.CoreV1().Pods(pod.Namespace).Patch(
		ctx,
		pod.Name,
		types.MergePatchType,
		[]byte(patch),
		metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("patch annotation: %w", err)
	}

	log.Info("annotation updated",
		"from", pod.Annotations[annotationDeletionCost],
		"to", desired)
	return nil
}
