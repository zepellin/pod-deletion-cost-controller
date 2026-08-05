package strategy

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
)

// Escalating increments cost by Step each sync cycle a pod remains above the CPU threshold,
// up to Max. When the pod drops below threshold, cost resets to ResetCost.
//
// This gives long-running busy pods progressively stronger deletion protection than
// pods that only recently became busy — useful when restarting a job that has been
// processing for a long time is more costly than restarting one that just started.
//
// Per-pod counters are held in memory only: they are released by Prune once a pod
// disappears, and are lost entirely on restart or leader failover (an escalated
// pod starts again from Step on the next cycle).
type Escalating struct {
	CPUThreshold  resource.Quantity
	Step          int32
	Max           int32
	ResetCost     int32
	NoMetricsCost int32
	costs         map[types.UID]int32
}

func (e *Escalating) Decide(pod *corev1.Pod, cpu *resource.Quantity) int32 {
	if e.costs == nil {
		e.costs = make(map[types.UID]int32)
	}
	if cpu == nil {
		return e.NoMetricsCost
	}
	if cpu.Cmp(e.CPUThreshold) > 0 {
		next := e.costs[pod.UID] + e.Step
		if next > e.Max {
			next = e.Max
		}
		e.costs[pod.UID] = next
		return next
	}
	delete(e.costs, pod.UID)
	return e.ResetCost
}

// Prune drops counters for pods that are no longer present. Without this, a pod
// that is deleted while busy — the normal end state for job-style workloads —
// would leave its entry behind for the lifetime of the process.
func (e *Escalating) Prune(live map[types.UID]bool) {
	for uid := range e.costs {
		if !live[uid] {
			delete(e.costs, uid)
		}
	}
}
