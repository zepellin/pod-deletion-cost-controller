package metrics

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

// Client wraps the metrics-server API.
type Client struct {
	mc metricsclient.Interface
}

func New(mc metricsclient.Interface) *Client {
	return &Client{mc: mc}
}

// PodCPU returns the total CPU usage across the specified containers for the given pod.
// If containers is empty, all containers reported by the metrics-server are summed
// (this includes native sidecar containers in k8s 1.29+).
// Returns a wrapped NotFound error when metrics are not yet available for the pod.
func (c *Client) PodCPU(ctx context.Context, namespace, podName string, containers []string) (resource.Quantity, error) {
	pm, err := c.mc.MetricsV1beta1().PodMetricses(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("get pod metrics %s/%s: %w", namespace, podName, err)
	}
	return sumCPU(pm, containers), nil
}

func sumCPU(pm *metricsv1beta1.PodMetrics, containers []string) resource.Quantity {
	include := make(map[string]bool, len(containers))
	for _, c := range containers {
		include[c] = true
	}

	total := resource.NewMilliQuantity(0, resource.DecimalSI)
	for _, cm := range pm.Containers {
		if len(containers) > 0 && !include[cm.Name] {
			continue
		}
		if cpu, ok := cm.Usage[corev1.ResourceCPU]; ok {
			total.Add(cpu)
		}
	}
	return *total
}
