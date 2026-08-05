package strategy

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
)

// Strategy computes the pod-deletion-cost annotation value for a pod.
// cpu is nil when metrics are unavailable (pod still starting or metrics-server error).
type Strategy interface {
	Decide(pod *corev1.Pod, cpu *resource.Quantity) int32
}

// Pruner is implemented by strategies that keep per-pod state across sync
// cycles. The syncer calls Prune after each successful target listing with the
// set of pods it still sees, so state for deleted pods is released.
//
// Strategies without per-pod state simply do not implement it.
type Pruner interface {
	Prune(live map[types.UID]bool)
}
