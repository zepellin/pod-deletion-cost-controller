package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
)

type raw struct {
	SyncInterval     string    `yaml:"syncInterval"`
	BusyCPUThreshold string    `yaml:"busyCPUThreshold"`
	BusyCost         int32     `yaml:"busyCost"`
	IdleCost         int32     `yaml:"idleCost"`
	NoMetricsCost    int32     `yaml:"noMetricsCost"`
	Targets          []Target  `yaml:"targets"`
}

// Target specifies a set of pods to manage via label selector within a namespace.
type Target struct {
	Namespace     string   `yaml:"namespace"`
	LabelSelector string   `yaml:"labelSelector"`
	// Containers lists which container names to include when summing CPU.
	// Empty means all containers (including native sidecars) are summed.
	Containers    []string `yaml:"containers"`
}

// Config is the parsed, validated controller configuration.
type Config struct {
	SyncInterval     time.Duration
	BusyCPUThreshold resource.Quantity
	// BusyCost is the pod-deletion-cost annotation value for pods above the CPU threshold.
	BusyCost      int32
	// IdleCost is the pod-deletion-cost annotation value for idle pods.
	IdleCost      int32
	// NoMetricsCost is the cost assigned when metrics are unavailable (pod still starting).
	NoMetricsCost int32
	Targets       []Target
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	r := raw{
		SyncInterval:     "60s",
		BusyCPUThreshold: "500m",
		BusyCost:         10000,
		IdleCost:         0,
		NoMetricsCost:    10000,
	}
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	syncInterval, err := time.ParseDuration(r.SyncInterval)
	if err != nil {
		return nil, fmt.Errorf("invalid syncInterval %q: %w", r.SyncInterval, err)
	}

	threshold, err := resource.ParseQuantity(r.BusyCPUThreshold)
	if err != nil {
		return nil, fmt.Errorf("invalid busyCPUThreshold %q: %w", r.BusyCPUThreshold, err)
	}

	return &Config{
		SyncInterval:     syncInterval,
		BusyCPUThreshold: threshold,
		BusyCost:         r.BusyCost,
		IdleCost:         r.IdleCost,
		NoMetricsCost:    r.NoMetricsCost,
		Targets:          r.Targets,
	}, nil
}
