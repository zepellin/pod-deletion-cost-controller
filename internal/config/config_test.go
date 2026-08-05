package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/zepellin/pod-deletion-cost-controller/internal/config"
	"github.com/zepellin/pod-deletion-cost-controller/internal/strategy"
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

func TestLoad_UnknownStrategyRejected(t *testing.T) {
	path := writeConfig(t, `
targets:
  - namespace: ns1
    labelSelector: "app=foo"
    strategy: bogus
`)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for unknown strategy, got nil")
	}
	// The error should identify both the offending target and the bad name.
	for _, want := range []string{"ns1/app=foo", `"bogus"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestLoad_KnownStrategiesAccepted(t *testing.T) {
	for _, name := range append(strategy.Names(), "") {
		path := writeConfig(t, fmt.Sprintf(`
targets:
  - namespace: ns1
    labelSelector: "app=foo"
    strategy: %q
`, name))
		if _, err := config.Load(path); err != nil {
			t.Errorf("strategy %q: unexpected error %v", name, err)
		}
	}
}

func TestStrategyParams(t *testing.T) {
	cfg := &config.Config{
		BusyCPUThreshold: resource.MustParse("250m"),
		BusyCost:         10000,
		IdleCost:         -500,
		NoMetricsCost:    7000,
	}
	target := config.Target{EscalatingStep: 250, EscalatingMax: 900}

	got := cfg.StrategyParams(target)
	want := strategy.Params{
		CPUThreshold:   resource.MustParse("250m"),
		BusyCost:       10000,
		IdleCost:       -500,
		NoMetricsCost:  7000,
		EscalatingStep: 250,
		EscalatingMax:  900,
	}
	if got.CPUThreshold.Cmp(want.CPUThreshold) != 0 {
		t.Errorf("CPUThreshold: got %v, want %v", got.CPUThreshold, want.CPUThreshold)
	}
	got.CPUThreshold, want.CPUThreshold = resource.Quantity{}, resource.Quantity{}
	if got != want {
		t.Errorf("StrategyParams: got %+v, want %+v", got, want)
	}
}

func TestStrategyParams_CPUReference(t *testing.T) {
	cfg := &config.Config{BusyCPUThreshold: resource.MustParse("250m")}

	got := cfg.StrategyParams(config.Target{EscalatingCPUReference: "500m"})
	if want := resource.MustParse("500m"); got.EscalatingCPUReference.Cmp(want) != 0 {
		t.Errorf("EscalatingCPUReference: got %s, want 500m", got.EscalatingCPUReference.String())
	}
	// Empty stays zero so the strategy package applies its own default.
	if got := cfg.StrategyParams(config.Target{}); !got.EscalatingCPUReference.IsZero() {
		t.Errorf("empty reference: got %s, want zero", got.EscalatingCPUReference.String())
	}
}

func TestLoad_InvalidCPUReferenceRejected(t *testing.T) {
	for _, value := range []string{"not-a-quantity", "0", "-100m"} {
		path := writeConfig(t, fmt.Sprintf(`
targets:
  - namespace: ns1
    labelSelector: "app=foo"
    strategy: escalating-weighted
    escalatingCPUReference: %q
`, value))
		_, err := config.Load(path)
		if err == nil {
			t.Errorf("escalatingCPUReference %q: expected error, got nil", value)
			continue
		}
		if !strings.Contains(err.Error(), "ns1/app=foo") {
			t.Errorf("escalatingCPUReference %q: error %q should name the target", value, err)
		}
	}
}

func TestLoad_ParsesCPUReference(t *testing.T) {
	path := writeConfig(t, `
targets:
  - namespace: ns1
    labelSelector: "app=foo"
    strategy: escalating-weighted
    escalatingStep: 500
    escalatingCPUReference: "2"
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Targets[0].EscalatingCPUReference; got != "2" {
		t.Errorf("EscalatingCPUReference: got %q, want \"2\"", got)
	}
	if got := cfg.StrategyParams(cfg.Targets[0]); got.EscalatingCPUReference.MilliValue() != 2000 {
		t.Errorf("StrategyParams reference: got %dm, want 2000m", got.EscalatingCPUReference.MilliValue())
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
