package strategy

import (
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
)

// Threshold assigns a fixed BusyCost when CPU exceeds the threshold, IdleCost otherwise.
// This is the default strategy.
type Threshold struct {
	CPUThreshold  resource.Quantity
	BusyCost      int32
	IdleCost      int32
	NoMetricsCost int32
}

func (t *Threshold) Decide(_ types.UID, cpu *resource.Quantity) Decision {
	if cpu == nil {
		return Decision{Cost: t.NoMetricsCost, Class: ClassNoMetrics}
	}
	if cpu.Cmp(t.CPUThreshold) > 0 {
		return Decision{Cost: t.BusyCost, Class: ClassBusy}
	}
	return Decision{Cost: t.IdleCost, Class: ClassIdle}
}
