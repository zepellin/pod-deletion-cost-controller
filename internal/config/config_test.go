package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/zepellin/pod-deletion-cost-controller/internal/config"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoad_Defaults(t *testing.T) {
	path := writeConfig(t, "targets: []\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SyncInterval != 60*time.Second {
		t.Errorf("SyncInterval: got %v, want 60s", cfg.SyncInterval)
	}
	if cfg.BusyCost != 10000 {
		t.Errorf("BusyCost: got %d, want 10000", cfg.BusyCost)
	}
	if cfg.IdleCost != 0 {
		t.Errorf("IdleCost: got %d, want 0", cfg.IdleCost)
	}
	if cfg.NoMetricsCost != 10000 {
		t.Errorf("NoMetricsCost: got %d, want 10000", cfg.NoMetricsCost)
	}
	wantThreshold := resource.MustParse("500m")
	if cfg.BusyCPUThreshold.Cmp(wantThreshold) != 0 {
		t.Errorf("BusyCPUThreshold: got %v, want 500m", cfg.BusyCPUThreshold)
	}
}

func TestLoad_OverrideAll(t *testing.T) {
	path := writeConfig(t, `
syncInterval: "1m"
busyCPUThreshold: "500m"
busyCost: 9999
idleCost: -1000
noMetricsCost: 5000
targets:
  - namespace: "ns1"
    labelSelector: "app=foo"
    containers:
      - main
  - namespace: "ns2"
    labelSelector: "app=bar"
    containers: []
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SyncInterval != time.Minute {
		t.Errorf("SyncInterval: got %v, want 1m", cfg.SyncInterval)
	}
	want500m := resource.MustParse("500m")
	if cfg.BusyCPUThreshold.Cmp(want500m) != 0 {
		t.Errorf("BusyCPUThreshold: got %v, want 500m", cfg.BusyCPUThreshold)
	}
	if cfg.BusyCost != 9999 {
		t.Errorf("BusyCost: got %d, want 9999", cfg.BusyCost)
	}
	if cfg.IdleCost != -1000 {
		t.Errorf("IdleCost: got %d, want -1000", cfg.IdleCost)
	}
	if cfg.NoMetricsCost != 5000 {
		t.Errorf("NoMetricsCost: got %d, want 5000", cfg.NoMetricsCost)
	}
	if len(cfg.Targets) != 2 {
		t.Fatalf("Targets: got %d, want 2", len(cfg.Targets))
	}
	if cfg.Targets[0].Namespace != "ns1" || cfg.Targets[0].LabelSelector != "app=foo" {
		t.Errorf("Targets[0]: %+v", cfg.Targets[0])
	}
	if len(cfg.Targets[0].Containers) != 1 || cfg.Targets[0].Containers[0] != "main" {
		t.Errorf("Targets[0].Containers: %v", cfg.Targets[0].Containers)
	}
	if len(cfg.Targets[1].Containers) != 0 {
		t.Errorf("Targets[1].Containers: expected empty, got %v", cfg.Targets[1].Containers)
	}
}

func TestLoad_InvalidSyncInterval(t *testing.T) {
	path := writeConfig(t, "syncInterval: \"not-a-duration\"\ntargets: []\n")
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for invalid syncInterval, got nil")
	}
}

func TestLoad_InvalidCPUThreshold(t *testing.T) {
	path := writeConfig(t, "busyCPUThreshold: \"bad-value\"\ntargets: []\n")
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for invalid busyCPUThreshold, got nil")
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := config.Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoad_EmptyTargets(t *testing.T) {
	path := writeConfig(t, "targets: []\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Targets == nil {
		t.Error("Targets should be empty slice, not nil")
	}
	if len(cfg.Targets) != 0 {
		t.Errorf("Targets: got %d, want 0", len(cfg.Targets))
	}
}
