package strategy

import (
	"math"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
)

// EscalatingWeighted behaves like Escalating, except the per-cycle increment is
// scaled by how much CPU the pod is actually using: Step is the cost added for a
// pod running at exactly CPUReference, so a pod at 1000m gains ten times as much
// per cycle as one at 100m when CPUReference is 1000m.
//
// This is for workloads where the work already done — not just the time spent
// doing it — is what makes a restart expensive: a pod chewing through a core has
// more to lose than one idling just above the busy threshold.
//
// State handling (per-pod counters, reset on idle, Prune, loss on restart) is
// inherited from Escalating.
type EscalatingWeighted struct {
	Escalating
	// CPUReference is the usage that earns exactly one Step per busy cycle.
	CPUReference resource.Quantity
}

func (w *EscalatingWeighted) Decide(uid types.UID, cpu *resource.Quantity) Decision {
	if w.costs == nil {
		w.costs = make(map[types.UID]int32)
	}
	if cpu == nil {
		return Decision{Cost: w.NoMetricsCost, Class: ClassNoMetrics}
	}
	if cpu.Cmp(w.CPUThreshold) <= 0 {
		delete(w.costs, uid)
		return Decision{Cost: w.ResetCost, Class: ClassIdle}
	}
	next := int64(w.costs[uid]) + w.increment(cpu)
	if next > int64(w.Max) {
		next = int64(w.Max)
	}
	w.costs[uid] = int32(next)
	return Decision{Cost: int32(next), Class: ClassBusy}
}

// increment is Step scaled by cpu/CPUReference. Busy pods always gain at least 1
// so that a pod far below the reference — but still above the busy threshold —
// keeps escalating instead of sitting at zero forever.
func (w *EscalatingWeighted) increment(cpu *resource.Quantity) int64 {
	ref, milli, step := w.CPUReference.MilliValue(), cpu.MilliValue(), int64(w.Step)
	if ref <= 0 {
		return step
	}
	// Guard the multiplication rather than wrapping: the result is clamped to Max
	// by the caller anyway, so saturating here loses nothing.
	if milli > 0 && step > math.MaxInt64/milli {
		return int64(w.Max)
	}
	inc := step * milli / ref
	if inc < 1 {
		inc = 1
	}
	return inc
}
