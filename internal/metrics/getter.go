package metrics

import (
	"context"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Getter is the interface the syncer uses to query pod CPU usage.
type Getter interface {
	PodCPU(ctx context.Context, namespace, podName string, containers []string) (resource.Quantity, error)
}

// GetterFunc is a function that implements Getter, useful in tests.
type GetterFunc func(ctx context.Context, namespace, podName string, containers []string) (resource.Quantity, error)

func (f GetterFunc) PodCPU(ctx context.Context, namespace, podName string, containers []string) (resource.Quantity, error) {
	return f(ctx, namespace, podName, containers)
}
