package strategy

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Strategy computes the pod-deletion-cost annotation value for a pod.
// cpu is nil when metrics are unavailable (pod still starting or metrics-server error).
type Strategy interface {
	Decide(pod *corev1.Pod, cpu *resource.Quantity) int32
}
