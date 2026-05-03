package strategy

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Threshold assigns a fixed BusyCost when CPU exceeds the threshold, IdleCost otherwise.
// This is the default strategy.
type Threshold struct {
	CPUThreshold  resource.Quantity
	BusyCost      int32
	IdleCost      int32
	NoMetricsCost int32
}

func (t *Threshold) Decide(_ *corev1.Pod, cpu *resource.Quantity) int32 {
	if cpu == nil {
		return t.NoMetricsCost
	}
	if cpu.Cmp(t.CPUThreshold) > 0 {
		return t.BusyCost
	}
	return t.IdleCost
}
