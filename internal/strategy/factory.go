package strategy

import "github.com/mdanko/pod-deletion-cost-controller/internal/config"

// defaultEscalatingMax is the cost ceiling used when escalatingMax is not configured.
// Large enough to be effectively uncapped for typical workloads.
const defaultEscalatingMax int32 = 1_000_000

// New creates the Strategy for t. Strategy names are validated by config.Load,
// so the default branch is unreachable in normal operation.
func New(t config.Target, cfg *config.Config) Strategy {
	switch t.Strategy {
	case "", "threshold":
		return &Threshold{
			CPUThreshold:  cfg.BusyCPUThreshold,
			BusyCost:      cfg.BusyCost,
			IdleCost:      cfg.IdleCost,
			NoMetricsCost: cfg.NoMetricsCost,
		}
	case "escalating":
		step := t.EscalatingStep
		if step == 0 {
			step = cfg.BusyCost / 10
			if step == 0 {
				step = 1000
			}
		}
		max := t.EscalatingMax
		if max == 0 {
			max = defaultEscalatingMax
		}
		return &Escalating{
			CPUThreshold:  cfg.BusyCPUThreshold,
			Step:          step,
			Max:           max,
			ResetCost:     cfg.IdleCost,
			NoMetricsCost: cfg.NoMetricsCost,
		}
	default:
		panic("unreachable: unknown strategy " + t.Strategy)
	}
}
