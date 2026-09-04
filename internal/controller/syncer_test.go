package controller_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/zepellin/pod-deletion-cost-controller/internal/config"
	"github.com/zepellin/pod-deletion-cost-controller/internal/controller"
	"github.com/zepellin/pod-deletion-cost-controller/internal/metrics"
	"github.com/zepellin/pod-deletion-cost-controller/internal/telemetry"
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
	taskStateKey  = "pdcc/task-state"
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

func getTaskState(t *testing.T, ctx context.Context, ns, name string) string {
	t.Helper()
	pod, err := testK8s.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod %s: %v", name, err)
	}
	return pod.Labels[taskStateKey]
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

func newSyncer(t *testing.T, cfg *config.Config, getter metrics.Getter) *controller.Syncer {
	t.Helper()
	syncer, _ := newSyncerWithRecorder(t, cfg, getter)
	return syncer
}

func newSyncerWithRecorder(t *testing.T, cfg *config.Config, getter metrics.Getter) (*controller.Syncer, *telemetry.Recorder) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rec := telemetry.NewRecorder()
	syncer, err := controller.New(testK8s, getter, cfg, log, rec)
	if err != nil {
		t.Fatalf("create syncer: %v", err)
	}
	return syncer, rec
}

// scrapeMetrics returns the recorder's Prometheus exposition text.
func scrapeMetrics(t *testing.T, rec *telemetry.Recorder) string {
	t.Helper()
	w := httptest.NewRecorder()
	rec.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("scrape metrics: status %d", w.Code)
	}
	return w.Body.String()
}

// ── tests ─────────────────────────────────────────────────────────────────────

func TestSyncer_IdlePod(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	pod := newRunningPod(t, ctx, ns, "idle-pod", map[string]string{"app": "test"})

	// 5m is well below the 100m threshold.
	syncer := newSyncer(t, defaultConfig(ns, "app=test", nil), cpuGetter(map[string]string{pod.Name: "5m"}))
	syncer.SyncOnce(ctx)

	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "0" {
		t.Errorf("idle pod: want annotation=0, got %q", got)
	}
}

func TestSyncer_IdlePodTaskStateLabel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	pod := newRunningPod(t, ctx, ns, "idle-pod", map[string]string{"app": "test"})

	syncer := newSyncer(t, defaultConfig(ns, "app=test", nil), cpuGetter(map[string]string{pod.Name: "5m"}))
	syncer.SyncOnce(ctx)

	if got := getTaskState(t, ctx, ns, pod.Name); got != "idle" {
		t.Errorf("idle pod: want label=idle, got %q", got)
	}
}

func TestSyncer_BusyPod(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	pod := newRunningPod(t, ctx, ns, "busy-pod", map[string]string{"app": "test"})

	// 500m is above the 100m threshold.
	syncer := newSyncer(t, defaultConfig(ns, "app=test", nil), cpuGetter(map[string]string{pod.Name: "500m"}))
	syncer.SyncOnce(ctx)

	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "10000" {
		t.Errorf("busy pod: want annotation=10000, got %q", got)
	}
}

func TestSyncer_BusyPodTaskStateLabel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	pod := newRunningPod(t, ctx, ns, "busy-pod", map[string]string{"app": "test"})

	syncer := newSyncer(t, defaultConfig(ns, "app=test", nil), cpuGetter(map[string]string{pod.Name: "500m"}))
	syncer.SyncOnce(ctx)

	if got := getTaskState(t, ctx, ns, pod.Name); got != "busy" {
		t.Errorf("busy pod: want label=busy, got %q", got)
	}
}

func TestSyncer_NoMetricsPod(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	pod := newRunningPod(t, ctx, ns, "starting-pod", map[string]string{"app": "test"})

	// Empty map → pod not found in metrics → noMetricsCost applied.
	syncer := newSyncer(t, defaultConfig(ns, "app=test", nil), cpuGetter(map[string]string{}))
	syncer.SyncOnce(ctx)

	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "10000" {
		t.Errorf("no-metrics pod: want annotation=10000, got %q", got)
	}
	// No metrics yet means the pod is still starting, not idle — it should
	// carry the same task-state label as a busy pod.
	if got := getTaskState(t, ctx, ns, pod.Name); got != "busy" {
		t.Errorf("no-metrics pod: want label=busy, got %q", got)
	}
}

func TestSyncer_TransientMetricsError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	pod := newRunningPod(t, ctx, ns, "error-pod", map[string]string{"app": "test"})

	// Non-NotFound error → still protects the pod.
	syncer := newSyncer(t, defaultConfig(ns, "app=test", nil), transientErrorGetter())
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

	syncer := newSyncer(t, defaultConfig(ns, "app=test", nil), cpuGetter(map[string]string{pod.Name: "500m"}))
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
	syncer := newSyncer(t, defaultConfig(ns, "app=test", nil), cpuGetter(map[string]string{pod.Name: "5m"}))
	var got string
	for range 5 {
		syncer.SyncOnce(ctx)
		got = getDeletionCost(t, ctx, ns, pod.Name)
		if got == "0" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got != "0" {
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
	syncer2 := newSyncer(t, defaultConfig(ns, "app=test", nil), cpuGetter(map[string]string{pod.Name: "900m"}))
	syncer2.SyncOnce(ctx)

	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "0" {
		t.Errorf("terminating pod: annotation should remain 0, got %q", got)
	}
}

func TestSyncer_PodDeletedMidCycleIsNotAnError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	pod := newRunningPod(t, ctx, ns, "vanishing-pod", map[string]string{"app": "test"})

	// The metrics lookup happens after the pod is listed but before it is
	// patched, so deleting from inside the getter reproduces a pod that goes
	// away mid-cycle — routine during the scale-down this controller influences.
	var grace int64
	getter := metrics.GetterFunc(func(_ context.Context, _, podName string, _ []string) (resource.Quantity, error) {
		if err := testK8s.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{
			GracePeriodSeconds: &grace,
		}); err != nil {
			t.Errorf("delete pod mid-cycle: %v", err)
		}
		return resource.MustParse("500m"), nil
	})

	syncer, rec := newSyncerWithRecorder(t, defaultConfig(ns, "app=test", nil), getter)
	syncer.SyncOnce(ctx)

	if _, err := testK8s.CoreV1().Pods(ns).Get(ctx, pod.Name, metav1.GetOptions{}); !kerrors.IsNotFound(err) {
		t.Fatalf("pod should be gone, got err=%v", err)
	}

	body := scrapeMetrics(t, rec)
	if want := `result="gone"`; !strings.Contains(body, want) {
		t.Errorf("expected a patch counted as %s, got:\n%s", want, patchMetrics(body))
	}
	if unwanted := `result="error"`; strings.Contains(body, unwanted) {
		t.Errorf("deleted pod must not count as an error, got:\n%s", patchMetrics(body))
	}
	// A cycle whose only incident was a vanished pod is a clean cycle.
	if !strings.Contains(body, `pdcc_sync_cycles_total{result="success"} 1`) {
		t.Errorf("expected a successful sync cycle, got:\n%s", patchMetrics(body))
	}
}

// patchMetrics extracts the controller's own counters from an exposition body
// for readable assertion failures.
func patchMetrics(body string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "pdcc_") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func TestSyncer_LabelSelectorFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	// Pod with label "app=worker" — the target selector is "app=processor", so it's excluded.
	pod := newRunningPod(t, ctx, ns, "other-pod", map[string]string{"app": "worker"})

	syncer := newSyncer(t, defaultConfig(ns, "app=processor", nil), cpuGetter(map[string]string{pod.Name: "500m"}))
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
	newSyncer(t, cfg, getter).SyncOnce(ctx)

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

	newSyncer(t, cfg, getter).SyncOnce(ctx)

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
	syncer := newSyncer(t, defaultConfig(ns, "app=test", nil), getter)

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
	newSyncer(t, defaultConfig(ns, "app=test", nil), cpuGetter(map[string]string{pod.Name: "500m"})).SyncOnce(ctx)
	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "10000" {
		t.Fatalf("busy: want 10000, got %q", got)
	}

	// Now idle — annotation must flip.
	newSyncer(t, defaultConfig(ns, "app=test", nil), cpuGetter(map[string]string{pod.Name: "5m"})).SyncOnce(ctx)
	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "0" {
		t.Errorf("after going idle: want 0, got %q", got)
	}
	if got := getTaskState(t, ctx, ns, pod.Name); got != "idle" {
		t.Errorf("after going idle: want label=idle, got %q", got)
	}
}

func TestSyncer_EscalatingStrategy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	pod := newRunningPod(t, ctx, ns, "escalating-pod", map[string]string{"app": "test"})

	// Use a mutable variable so the same getter closure can simulate CPU changing between syncs.
	cpuStr := "500m"
	getter := metrics.GetterFunc(func(_ context.Context, _, name string, _ []string) (resource.Quantity, error) {
		if name == pod.Name {
			return resource.MustParse(cpuStr), nil
		}
		return resource.Quantity{}, nil
	})

	cfg := &config.Config{
		BusyCPUThreshold: resource.MustParse("100m"),
		BusyCost:         10000,
		IdleCost:         0,
		NoMetricsCost:    10000,
		Targets: []config.Target{{
			Namespace:      ns,
			LabelSelector:  "app=test",
			Strategy:       "escalating",
			EscalatingStep: 3000,
			EscalatingMax:  9000,
		}},
	}
	syncer := newSyncer(t, cfg, getter)

	syncer.SyncOnce(ctx)
	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "3000" {
		t.Errorf("tick 1: want 3000, got %q", got)
	}

	syncer.SyncOnce(ctx)
	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "6000" {
		t.Errorf("tick 2: want 6000, got %q", got)
	}

	syncer.SyncOnce(ctx)
	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "9000" {
		t.Errorf("tick 3: want 9000, got %q", got)
	}

	// At the cap — annotation must not change (idempotent).
	syncer.SyncOnce(ctx)
	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "9000" {
		t.Errorf("tick 4 (capped): want 9000, got %q", got)
	}

	// Pod goes idle — cost resets to 0.
	cpuStr = "5m"
	syncer.SyncOnce(ctx)
	if got := getDeletionCost(t, ctx, ns, pod.Name); got != "0" {
		t.Errorf("after idle: want 0, got %q", got)
	}
}

func TestSyncer_EscalatingWeightedStrategy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	heavy := newRunningPod(t, ctx, ns, "heavy-pod", map[string]string{"app": "test"})
	light := newRunningPod(t, ctx, ns, "light-pod", map[string]string{"app": "test"})

	// heavy runs at 5x the light pod, so it must accumulate 5x the cost per cycle.
	getter := cpuGetter(map[string]string{heavy.Name: "1000m", light.Name: "200m"})

	cfg := &config.Config{
		BusyCPUThreshold: resource.MustParse("100m"),
		BusyCost:         10000,
		IdleCost:         0,
		NoMetricsCost:    10000,
		Targets: []config.Target{{
			Namespace:              ns,
			LabelSelector:          "app=test",
			Strategy:               "escalating-weighted",
			EscalatingStep:         1000,
			EscalatingMax:          9000,
			EscalatingCPUReference: "1000m",
		}},
	}
	syncer := newSyncer(t, cfg, getter)

	syncer.SyncOnce(ctx)
	if got := getDeletionCost(t, ctx, ns, heavy.Name); got != "1000" {
		t.Errorf("heavy tick 1: want 1000, got %q", got)
	}
	if got := getDeletionCost(t, ctx, ns, light.Name); got != "200" {
		t.Errorf("light tick 1: want 200, got %q", got)
	}

	syncer.SyncOnce(ctx)
	if got := getDeletionCost(t, ctx, ns, heavy.Name); got != "2000" {
		t.Errorf("heavy tick 2: want 2000, got %q", got)
	}
	if got := getDeletionCost(t, ctx, ns, light.Name); got != "400" {
		t.Errorf("light tick 2: want 400, got %q", got)
	}
}
