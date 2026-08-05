// Package strategy implements the algorithms that turn a pod's CPU usage into a
// pod-deletion-cost value. It deliberately knows nothing about configuration
// files or Kubernetes clients: callers translate their own settings into Params
// and hand them to New.
package strategy

import (
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
)

// Cost classes reported alongside every decision. These are used as metric
// label values, so changing them breaks existing dashboards.
const (
	ClassBusy      = "busy"
	ClassIdle      = "idle"
	ClassNoMetrics = "no_metrics"
)

// Decision is the outcome of evaluating one pod: the annotation value to apply
// and the class the strategy put the pod in.
type Decision struct {
	Cost  int32
	Class string
}

// Strategy computes the pod-deletion-cost annotation value for a pod.
// cpu is nil when metrics are unavailable (pod still starting or metrics-server error).
type Strategy interface {
	Decide(uid types.UID, cpu *resource.Quantity) Decision
}

// Pruner is implemented by strategies that keep per-pod state across sync
// cycles. The syncer calls Prune after each successful target listing with the
// set of pods it still sees, so state for deleted pods is released.
//
// Strategies without per-pod state simply do not implement it.
type Pruner interface {
	Prune(live map[types.UID]bool)
}
