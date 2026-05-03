package strategy_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/mdanko/pod-deletion-cost-controller/internal/strategy"
)

func ptr(q resource.Quantity) *resource.Quantity { return &q }

func pod(uid types.UID) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: uid}}
}

var threshold100m = resource.MustParse("100m")

// ── Threshold ─────────────────────────────────────────────────────────────────

func TestThreshold_NoMetrics(t *testing.T) {
	s := &strategy.Threshold{CPUThreshold: threshold100m, BusyCost: 10000, IdleCost: 0, NoMetricsCost: 10000}
	if got := s.Decide(pod("p1"), nil); got != 10000 {
		t.Errorf("want 10000, got %d", got)
	}
}

func TestThreshold_Busy(t *testing.T) {
	s := &strategy.Threshold{CPUThreshold: threshold100m, BusyCost: 10000, IdleCost: 0, NoMetricsCost: 10000}
	if got := s.Decide(pod("p1"), ptr(resource.MustParse("500m"))); got != 10000 {
		t.Errorf("want 10000, got %d", got)
	}
}

func TestThreshold_Idle(t *testing.T) {
	s := &strategy.Threshold{CPUThreshold: threshold100m, BusyCost: 10000, IdleCost: 0, NoMetricsCost: 10000}
	if got := s.Decide(pod("p1"), ptr(resource.MustParse("5m"))); got != 0 {
		t.Errorf("want 0, got %d", got)
	}
}

func TestThreshold_AtThreshold_IsIdle(t *testing.T) {
	s := &strategy.Threshold{CPUThreshold: threshold100m, BusyCost: 10000, IdleCost: 0, NoMetricsCost: 10000}
	// Cmp returns 0 when equal — not > 0, so it's idle.
	if got := s.Decide(pod("p1"), ptr(resource.MustParse("100m"))); got != 0 {
		t.Errorf("want 0 (at threshold = idle), got %d", got)
	}
}

// ── Escalating ────────────────────────────────────────────────────────────────

func TestEscalating_NoMetrics(t *testing.T) {
	s := &strategy.Escalating{CPUThreshold: threshold100m, Step: 3000, Max: 9000, ResetCost: 0, NoMetricsCost: 10000}
	if got := s.Decide(pod("p1"), nil); got != 10000 {
		t.Errorf("want 10000, got %d", got)
	}
}

func TestEscalating_EscalatesAndCaps(t *testing.T) {
	s := &strategy.Escalating{CPUThreshold: threshold100m, Step: 3000, Max: 9000, ResetCost: 0, NoMetricsCost: 10000}
	p := pod("p1")
	cpu := ptr(resource.MustParse("500m"))

	cases := []int32{3000, 6000, 9000, 9000} // tick 4 stays capped
	for i, want := range cases {
		got := s.Decide(p, cpu)
		if got != want {
			t.Errorf("tick %d: want %d, got %d", i+1, want, got)
		}
	}
}

func TestEscalating_ResetsOnIdle(t *testing.T) {
	s := &strategy.Escalating{CPUThreshold: threshold100m, Step: 3000, Max: 9000, ResetCost: 0, NoMetricsCost: 10000}
	p := pod("p1")

	s.Decide(p, ptr(resource.MustParse("500m"))) // tick 1: 3000
	s.Decide(p, ptr(resource.MustParse("500m"))) // tick 2: 6000

	if got := s.Decide(p, ptr(resource.MustParse("5m"))); got != 0 {
		t.Errorf("after idle: want 0, got %d", got)
	}
	// After reset, next busy tick starts from scratch.
	if got := s.Decide(p, ptr(resource.MustParse("500m"))); got != 3000 {
		t.Errorf("after reset, tick 1: want 3000, got %d", got)
	}
}

func TestEscalating_IndependentPerPod(t *testing.T) {
	s := &strategy.Escalating{CPUThreshold: threshold100m, Step: 3000, Max: 9000, ResetCost: 0, NoMetricsCost: 10000}
	p1, p2 := pod("p1"), pod("p2")
	cpu := ptr(resource.MustParse("500m"))

	s.Decide(p1, cpu) // p1: 3000
	s.Decide(p1, cpu) // p1: 6000
	s.Decide(p2, cpu) // p2: 3000 (independent)

	if got := s.Decide(p1, cpu); got != 9000 {
		t.Errorf("p1 tick 3: want 9000, got %d", got)
	}
	if got := s.Decide(p2, cpu); got != 6000 {
		t.Errorf("p2 tick 2: want 6000, got %d", got)
	}
}
