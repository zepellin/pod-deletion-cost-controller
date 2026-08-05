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
	NameThreshold  = "threshold"
	NameEscalating = "escalating"
)

// defaultEscalatingMax is the cost ceiling used when EscalatingMax is not configured.
// Large enough to be effectively uncapped for typical workloads.
const defaultEscalatingMax int32 = 1_000_000

// defaultEscalatingStep is used when neither EscalatingStep nor BusyCost gives
// a usable increment.
const defaultEscalatingStep int32 = 1000

// Params carries everything any strategy needs to be built. Fields that apply
// to a single strategy are ignored by the others.
type Params struct {
	CPUThreshold  resource.Quantity
	BusyCost      int32
	IdleCost      int32
	NoMetricsCost int32

	// EscalatingStep is the cost increment per busy cycle. Zero means BusyCost/10.
	EscalatingStep int32
	// EscalatingMax is the cost ceiling. Zero means defaultEscalatingMax.
	EscalatingMax int32
}

// Names lists the strategy names accepted in configuration, for error messages.
func Names() []string { return []string{NameThreshold, NameEscalating} }

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
		}, nil

	default:
		return nil, fmt.Errorf("unknown strategy %q (valid: %s)", name, strings.Join(Names(), ", "))
	}
}

// Validate reports whether name selects a known strategy. It defers to New so
// the set of valid names cannot drift from the set New can actually build; the
// strategy it builds is discarded.
func Validate(name string) error {
	_, err := New(name, Params{})
	return err
}
