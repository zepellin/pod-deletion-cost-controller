package controller_test

import (
	"context"
	"log/slog"
	"os"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/mdanko/pod-deletion-cost-controller/internal/config"
	"github.com/mdanko/pod-deletion-cost-controller/internal/controller"
	"github.com/mdanko/pod-deletion-cost-controller/internal/metrics"
)

// Package-level envtest state, started once for all tests in this package.
var (
	testEnv *envtest.Environment
	testCfg *rest.Config
	testK8s kubernetes.Interface
)

func TestMain(m *testing.M) {
	testEnv = &envtest.Environment{}

	var err error
	testCfg, err = testEnv.Start()
	if err != nil {
		panic("envtest start: " + err.Error())
	}

	testK8s, err = kubernetes.NewForConfig(testCfg)
	if err != nil {
		panic("build k8s client: " + err.Error())
	}

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		slog.Error("envtest stop", "err", err)
	}
	os.Exit(code)
}

// ── helpers ──────────────────────────────────────────────────────────────────

const (
	annotationKey = "controller.kubernetes.io/pod-deletion-cost"
	defaultBusy   = int32(10000)
	defaultIdle   = int32(0)
	defaultNoMet  = int32(10000)
)

func newNamespace(t *testing.T, ctx context.Context) string {
	t.Helper()
	ns, err := testK8s.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "test-"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = testK8s.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
	})
	return ns.Name
}

// newRunningPod creates a pod and directly sets its status to Phase=Running.
// In envtest there is no scheduler or kubelet, so status must be set manually
// via the /status subresource (the API server accepts any status from admin clients).
func newRunningPod(t *testing.T, ctx context.Context, ns, name string, labels map[string]string, finalizers ...string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  ns,
			Labels:     labels,
			Finalizers: finalizers,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "busybox:latest"},
				{Name: "sidecar", Image: "busybox:latest"},
			},
		},
	}
	created, err := testK8s.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod %s: %v", name, err)
	}

	created.Status.Phase = corev1.PodRunning
	updated, err := testK8s.CoreV1().Pods(ns).UpdateStatus(ctx, created, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("update pod status %s: %v", name, err)
	}
	return updated
}

func newPendingPod(t *testing.T, ctx context.Context, ns, name string, labels map[string]string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "busybox:latest"}}},
	}
	created, err := testK8s.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pending pod %s: %v", name, err)
	}
	// Phase defaults to "" (Pending) when not set.
	return created
}

func getDeletionCost(t *testing.T, ctx context.Context, ns, name string) string {
	t.Helper()
	pod, err := testK8s.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod %s: %v", name, err)
	}
	return pod.Annotations[annotationKey]
}

// cpuGetter returns a GetterFunc that returns pre-set CPU per pod name.
// Pods not in the map get a NotFound error (simulates metrics not yet available).
func cpuGetter(perPod map[string]string) metrics.Getter {
	return metrics.GetterFunc(func(_ context.Context, _, podName string, _ []string) (resource.Quantity, error) {
		if cpu, ok := perPod[podName]; ok {
			return resource.MustParse(cpu), nil
		}
		return resource.Quantity{}, kerrors.NewNotFound(
			schema.GroupResource{Group: "metrics.k8s.io", Resource: "pods"}, podName,
		)
	})
}

// cpuGetterWithCapture returns a GetterFunc that records the containers argument for inspection.
func cpuGetterWithCapture(cpu string, gotContainers *[]string) metrics.Getter {
	return metrics.GetterFunc(func(_ context.Context, _, _ string, containers []string) (resource.Quantity, error) {
		cp := make([]string, len(containers))
		copy(cp, containers)
		*gotContainers = cp
		return resource.MustParse(cpu), nil
	})
}

func transientErrorGetter() metrics.Getter {
	return metrics.GetterFunc(func(_ context.Context, _, _ string, _ []string) (resource.Quantity, error) {
		return resource.Quantity{}, &kerrors.StatusError{ErrStatus: metav1.Status{
			Status: metav1.StatusFailure, Code: 503, Reason: metav1.StatusReasonServiceUnavailable,
		}}
	})
}

func defaultConfig(ns, selector string, containers []string) *config.Config {
	return &config.Config{
		BusyCPUThreshold: resource.MustParse("100m"),
		BusyCost:         defaultBusy,
		IdleCost:         defaultIdle,
		NoMetricsCost:    defaultNoMet,
		Targets: []config.Target{
			{Namespace: ns, LabelSelector: selector, Containers: containers},
		},
	}
}

func newSyncer(cfg *config.Config, getter metrics.Getter) *controller.Syncer {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return controller.New(testK8s, getter, cfg, log)
}

// ── tests ─────────────────────────────────────────────────────────────────────

func TestSyncer_IdlePod(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	pod := newRunningPod(t, ctx, ns, "idle-pod", map[string]string{"app": "test"})

	// 5m is well below the 100m threshold.
	syncer := newSyncer(defaultConfig(ns, "app=test", nil), cpuGetter(map[string]string{pod.Name: "5m"}))
	syncer.SyncOnce(ctx)

	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "0" {
		t.Errorf("idle pod: want annotation=0, got %q", got)
	}
}

func TestSyncer_BusyPod(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	pod := newRunningPod(t, ctx, ns, "busy-pod", map[string]string{"app": "test"})

	// 500m is above the 100m threshold.
	syncer := newSyncer(defaultConfig(ns, "app=test", nil), cpuGetter(map[string]string{pod.Name: "500m"}))
	syncer.SyncOnce(ctx)

	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "10000" {
		t.Errorf("busy pod: want annotation=10000, got %q", got)
	}
}

func TestSyncer_NoMetricsPod(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	pod := newRunningPod(t, ctx, ns, "starting-pod", map[string]string{"app": "test"})

	// Empty map → pod not found in metrics → noMetricsCost applied.
	syncer := newSyncer(defaultConfig(ns, "app=test", nil), cpuGetter(map[string]string{}))
	syncer.SyncOnce(ctx)

	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "10000" {
		t.Errorf("no-metrics pod: want annotation=10000, got %q", got)
	}
}

func TestSyncer_TransientMetricsError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	pod := newRunningPod(t, ctx, ns, "error-pod", map[string]string{"app": "test"})

	// Non-NotFound error → still protects the pod.
	syncer := newSyncer(defaultConfig(ns, "app=test", nil), transientErrorGetter())
	syncer.SyncOnce(ctx)

	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "10000" {
		t.Errorf("transient error pod: want annotation=10000, got %q", got)
	}
}

func TestSyncer_PendingPodNotAnnotated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	pod := newPendingPod(t, ctx, ns, "pending-pod", map[string]string{"app": "test"})

	syncer := newSyncer(defaultConfig(ns, "app=test", nil), cpuGetter(map[string]string{pod.Name: "500m"}))
	syncer.SyncOnce(ctx)

	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "" {
		t.Errorf("pending pod: want no annotation, got %q", got)
	}
}

func TestSyncer_TerminatingPodNotUpdated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	// Finalizer prevents actual deletion, keeping the pod around with DeletionTimestamp set.
	const finalizer = "test.io/hold"
	pod := newRunningPod(t, ctx, ns, "terminating-pod", map[string]string{"app": "test"}, finalizer)

	// First sync → pod is running, gets idleCost.
	syncer := newSyncer(defaultConfig(ns, "app=test", nil), cpuGetter(map[string]string{pod.Name: "5m"}))
	syncer.SyncOnce(ctx)
	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "0" {
		t.Fatalf("pre-termination: want annotation=0, got %q", got)
	}

	// Trigger deletion — sets DeletionTimestamp but pod remains due to finalizer.
	if err := testK8s.CoreV1().Pods(ns).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete pod: %v", err)
	}

	// Remove finalizer in cleanup so the namespace can be fully cleaned up.
	t.Cleanup(func() {
		p, err := testK8s.CoreV1().Pods(ns).Get(context.Background(), pod.Name, metav1.GetOptions{})
		if err != nil {
			return
		}
		p.Finalizers = nil
		_, _ = testK8s.CoreV1().Pods(ns).Update(context.Background(), p, metav1.UpdateOptions{})
	})

	// Second sync with high CPU — annotation must NOT change because pod is terminating.
	syncer2 := newSyncer(defaultConfig(ns, "app=test", nil), cpuGetter(map[string]string{pod.Name: "900m"}))
	syncer2.SyncOnce(ctx)

	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "0" {
		t.Errorf("terminating pod: annotation should remain 0, got %q", got)
	}
}

func TestSyncer_LabelSelectorFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	// Pod with label "app=worker" — the target selector is "app=processor", so it's excluded.
	pod := newRunningPod(t, ctx, ns, "other-pod", map[string]string{"app": "worker"})

	syncer := newSyncer(defaultConfig(ns, "app=processor", nil), cpuGetter(map[string]string{pod.Name: "500m"}))
	syncer.SyncOnce(ctx)

	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "" {
		t.Errorf("non-matching pod: want no annotation, got %q", got)
	}
}

func TestSyncer_ContainerFilterPassedToGetter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	pod := newRunningPod(t, ctx, ns, "filter-pod", map[string]string{"app": "test"})

	var gotContainers []string
	getter := cpuGetterWithCapture("500m", &gotContainers)

	cfg := defaultConfig(ns, "app=test", []string{"main"})
	newSyncer(cfg, getter).SyncOnce(ctx)

	if !reflect.DeepEqual(gotContainers, []string{"main"}) {
		t.Errorf("container filter: getter received %v, want [main]", gotContainers)
	}
	// 500m > 100m threshold → busyCost
	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "10000" {
		t.Errorf("container filter: want annotation=10000, got %q", got)
	}
}

func TestSyncer_MultipleTargets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns1 := newNamespace(t, ctx)
	ns2 := newNamespace(t, ctx)

	pod1 := newRunningPod(t, ctx, ns1, "pod-a", map[string]string{"app": "proc"})
	pod2 := newRunningPod(t, ctx, ns2, "pod-b", map[string]string{"app": "proc"})

	cfg := &config.Config{
		BusyCPUThreshold: resource.MustParse("100m"),
		BusyCost:         defaultBusy,
		IdleCost:         defaultIdle,
		NoMetricsCost:    defaultNoMet,
		Targets: []config.Target{
			{Namespace: ns1, LabelSelector: "app=proc"},
			{Namespace: ns2, LabelSelector: "app=proc"},
		},
	}
	getter := cpuGetter(map[string]string{
		pod1.Name: "5m",   // idle
		pod2.Name: "500m", // busy
	})

	newSyncer(cfg, getter).SyncOnce(ctx)

	if got := getDeletionCost(t, ctx, ns1, pod1.Name); got != "0" {
		t.Errorf("ns1 pod1 idle: want 0, got %q", got)
	}
	if got := getDeletionCost(t, ctx, ns2, pod2.Name); got != "10000" {
		t.Errorf("ns2 pod2 busy: want 10000, got %q", got)
	}
}

func TestSyncer_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	pod := newRunningPod(t, ctx, ns, "idempotent-pod", map[string]string{"app": "test"})

	getter := cpuGetter(map[string]string{pod.Name: "500m"})
	syncer := newSyncer(defaultConfig(ns, "app=test", nil), getter)

	syncer.SyncOnce(ctx)
	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "10000" {
		t.Fatalf("first sync: want 10000, got %q", got)
	}

	// Second sync with same metrics — annotation should stay 10000 with no error.
	syncer.SyncOnce(ctx)
	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "10000" {
		t.Errorf("second sync: want 10000 (unchanged), got %q", got)
	}
}

func TestSyncer_TransitionBusyToIdle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	pod := newRunningPod(t, ctx, ns, "transition-pod", map[string]string{"app": "test"})

	// Start busy.
	newSyncer(defaultConfig(ns, "app=test", nil), cpuGetter(map[string]string{pod.Name: "500m"})).SyncOnce(ctx)
	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "10000" {
		t.Fatalf("busy: want 10000, got %q", got)
	}

	// Now idle — annotation must flip.
	newSyncer(defaultConfig(ns, "app=test", nil), cpuGetter(map[string]string{pod.Name: "5m"})).SyncOnce(ctx)
	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "0" {
		t.Errorf("after going idle: want 0, got %q", got)
	}
}
