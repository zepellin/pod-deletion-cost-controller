package strategy

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Strategy names accepted in configuration. The empty name selects the default.
// Keep in sync with the strategy enum in
// helm/pod-deletion-cost-controller/values.schema.json, which validates the same
// names at chart-render time.
const (
	NameThreshold          = "threshold"
	NameEscalating         = "escalating"
	NameEscalatingWeighted = "escalating-weighted"
)

// defaultEscalatingMax is the cost ceiling used when EscalatingMax is not configured.
// Large enough to be effectively uncapped for typical workloads.
const defaultEscalatingMax int32 = 1_000_000

// defaultEscalatingStep is used when neither EscalatingStep nor BusyCost gives
// a usable increment.
const defaultEscalatingStep int32 = 1000

// defaultEscalatingCPUReference is the usage that earns a full step under
// escalating-weighted when EscalatingCPUReference is not configured: one core,
// so the step is read as "cost per core-cycle".
const defaultEscalatingCPUReference = "1000m"

// Params carries everything any strategy needs to be built. Fields that apply
// to a single strategy are ignored by the others.
type Params struct {
	CPUThreshold  resource.Quantity
	BusyCost      int32
	IdleCost      int32
	NoMetricsCost int32

	// EscalatingStep is the cost increment per busy cycle. Zero means BusyCost/10.
	// Under escalating-weighted it is the increment for a pod at EscalatingCPUReference.
	EscalatingStep int32
	// EscalatingMax is the cost ceiling. Zero means defaultEscalatingMax.
	EscalatingMax int32
	// EscalatingCPUReference is the CPU usage that earns a full step under
	// escalating-weighted. Zero means defaultEscalatingCPUReference.
	EscalatingCPUReference resource.Quantity
}

// Names lists the strategy names accepted in configuration, for error messages.
func Names() []string { return []string{NameThreshold, NameEscalating, NameEscalatingWeighted} }

// New builds the strategy called name. An empty name selects the default.
// It is the single place that knows which names exist.
func New(name string, p Params) (Strategy, error) {
	switch name {
	case "", NameThreshold:
		return &Threshold{
			CPUThreshold:  p.CPUThreshold,
			BusyCost:      p.BusyCost,
			IdleCost:      p.IdleCost,
			NoMetricsCost: p.NoMetricsCost,
		}, nil

	case NameEscalating:
		return newEscalating(p), nil

	case NameEscalatingWeighted:
		ref := p.EscalatingCPUReference
		if ref.IsZero() {
			ref = resource.MustParse(defaultEscalatingCPUReference)
		}
		return &EscalatingWeighted{Escalating: *newEscalating(p), CPUReference: ref}, nil

	default:
		return nil, fmt.Errorf("unknown strategy %q (valid: %s)", name, strings.Join(Names(), ", "))
	}
}

// newEscalating applies the escalating defaults. It is shared by both escalating
// strategies, which differ only in how the step is applied.
func newEscalating(p Params) *Escalating {
	step := p.EscalatingStep
	if step == 0 {
		step = p.BusyCost / 10
		if step == 0 {
			step = defaultEscalatingStep
		}
	}
	max := p.EscalatingMax
	if max == 0 {
		max = defaultEscalatingMax
	}
	return &Escalating{
		CPUThreshold:  p.CPUThreshold,
		Step:          step,
		Max:           max,
		ResetCost:     p.IdleCost,
		NoMetricsCost: p.NoMetricsCost,
	}
}

// Validate reports whether name selects a known strategy. It defers to New so
// the set of valid names cannot drift from the set New can actually build; the
// strategy it builds is discarded.
func Validate(name string) error {
	_, err := New(name, Params{})
	return err
}
