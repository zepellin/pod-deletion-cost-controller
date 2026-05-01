package metrics

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
)

func makePodMetrics(containers map[string]string) *metricsv1beta1.PodMetrics {
	pm := &metricsv1beta1.PodMetrics{ObjectMeta: metav1.ObjectMeta{Name: "test-pod"}}
	for name, cpu := range containers {
		pm.Containers = append(pm.Containers, metricsv1beta1.ContainerMetrics{
			Name:  name,
			Usage: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
		})
	}
	return pm
}

func TestSumCPU_AllContainers(t *testing.T) {
	pm := makePodMetrics(map[string]string{
		"main":    "100m",
		"sidecar": "50m",
	})
	got := sumCPU(pm, nil)
	want := resource.MustParse("150m")
	if got.Cmp(want) != 0 {
		t.Errorf("sumCPU all containers: got %v, want %v", got.String(), want.String())
	}
}

func TestSumCPU_FilteredContainers(t *testing.T) {
	pm := makePodMetrics(map[string]string{
		"main":    "300m",
		"sidecar": "200m",
	})
	got := sumCPU(pm, []string{"main"})
	want := resource.MustParse("300m")
	if got.Cmp(want) != 0 {
		t.Errorf("sumCPU filtered: got %v, want %v", got.String(), want.String())
	}
}

func TestSumCPU_NoMatchingContainers(t *testing.T) {
	pm := makePodMetrics(map[string]string{
		"main": "500m",
	})
	got := sumCPU(pm, []string{"nonexistent"})
	want := resource.MustParse("0m")
	if got.Cmp(want) != 0 {
		t.Errorf("sumCPU no match: got %v, want 0", got.String())
	}
}

func TestSumCPU_EmptyContainers(t *testing.T) {
	pm := makePodMetrics(map[string]string{})
	got := sumCPU(pm, nil)
	want := resource.MustParse("0m")
	if got.Cmp(want) != 0 {
		t.Errorf("sumCPU empty pod: got %v, want 0", got.String())
	}
}

func TestSumCPU_MultipleFilteredContainers(t *testing.T) {
	pm := makePodMetrics(map[string]string{
		"app":     "400m",
		"logger":  "100m",
		"sidecar": "50m",
	})
	got := sumCPU(pm, []string{"app", "logger"})
	want := resource.MustParse("500m")
	if got.Cmp(want) != 0 {
		t.Errorf("sumCPU multi-filter: got %v, want %v", got.String(), want.String())
	}
}
