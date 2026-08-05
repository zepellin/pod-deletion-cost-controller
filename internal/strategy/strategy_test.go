package strategy_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	"github.com/zepellin/pod-deletion-cost-controller/internal/strategy"
)

func ptr(q resource.Quantity) *resource.Quantity { return &q }

var threshold100m = resource.MustParse("100m")

func busy() *resource.Quantity { return ptr(resource.MustParse("500m")) }
func idle() *resource.Quantity { return ptr(resource.MustParse("5m")) }

// ── Threshold ─────────────────────────────────────────────────────────────────

func TestThreshold(t *testing.T) {
	s := &strategy.Threshold{CPUThreshold: threshold100m, BusyCost: 10000, IdleCost: 0, NoMetricsCost: 10000}

	tests := []struct {
		name      string
		cpu       *resource.Quantity
		wantCost  int32
		wantClass string
	}{
		{"no metrics", nil, 10000, strategy.ClassNoMetrics},
		{"above threshold", busy(), 10000, strategy.ClassBusy},
		{"below threshold", idle(), 0, strategy.ClassIdle},
		// Cmp returns 0 when equal — not > 0, so exactly at the threshold is idle.
		{"at threshold is idle", ptr(resource.MustParse("100m")), 0, strategy.ClassIdle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.Decide("p1", tt.cpu)
			if got.Cost != tt.wantCost || got.Class != tt.wantClass {
				t.Errorf("Decide = {%d %s}, want {%d %s}", got.Cost, got.Class, tt.wantCost, tt.wantClass)
			}
		})
	}
}

// ── Escalating ────────────────────────────────────────────────────────────────

func newEscalating() *strategy.Escalating {
	return &strategy.Escalating{CPUThreshold: threshold100m, Step: 3000, Max: 9000, ResetCost: 0, NoMetricsCost: 10000}
}

func TestEscalating_NoMetrics(t *testing.T) {
	got := newEscalating().Decide("p1", nil)
	if got.Cost != 10000 || got.Class != strategy.ClassNoMetrics {
		t.Errorf("Decide = {%d %s}, want {10000 %s}", got.Cost, got.Class, strategy.ClassNoMetrics)
	}
}

func TestEscalating_EscalatesAndCaps(t *testing.T) {
	s := newEscalating()

	for i, want := range []int32{3000, 6000, 9000, 9000} { // tick 4 stays capped
		got := s.Decide("p1", busy())
		if got.Cost != want {
			t.Errorf("tick %d: want %d, got %d", i+1, want, got.Cost)
		}
		if got.Class != strategy.ClassBusy {
			t.Errorf("tick %d: want class %s, got %s", i+1, strategy.ClassBusy, got.Class)
		}
	}
}

func TestEscalating_ResetsOnIdle(t *testing.T) {
	s := newEscalating()

	s.Decide("p1", busy()) // tick 1: 3000
	s.Decide("p1", busy()) // tick 2: 6000

	got := s.Decide("p1", idle())
	if got.Cost != 0 || got.Class != strategy.ClassIdle {
		t.Errorf("after idle: got {%d %s}, want {0 %s}", got.Cost, got.Class, strategy.ClassIdle)
	}
	// After reset, next busy tick starts from scratch.
	if got := s.Decide("p1", busy()); got.Cost != 3000 {
		t.Errorf("after reset, tick 1: want 3000, got %d", got.Cost)
	}
}

func TestEscalating_IndependentPerPod(t *testing.T) {
	s := newEscalating()

	s.Decide("p1", busy()) // p1: 3000
	s.Decide("p1", busy()) // p1: 6000
	s.Decide("p2", busy()) // p2: 3000 (independent)

	if got := s.Decide("p1", busy()); got.Cost != 9000 {
		t.Errorf("p1 tick 3: want 9000, got %d", got.Cost)
	}
	if got := s.Decide("p2", busy()); got.Cost != 6000 {
		t.Errorf("p2 tick 2: want 6000, got %d", got.Cost)
	}
}

func TestEscalating_PruneDropsMissingPods(t *testing.T) {
	s := newEscalating()

	s.Decide("p1", busy()) // p1: 3000
	s.Decide("p1", busy()) // p1: 6000
	s.Decide("p2", busy()) // p2: 3000

	// p1 is gone (e.g. deleted while still busy); p2 is still around.
	s.Prune(map[types.UID]bool{"p2": true})

	// p1's counter was released, so it starts over.
	if got := s.Decide("p1", busy()); got.Cost != 3000 {
		t.Errorf("pruned pod: want 3000 (counter released), got %d", got.Cost)
	}
	// p2 was retained and continues escalating.
	if got := s.Decide("p2", busy()); got.Cost != 6000 {
		t.Errorf("retained pod: want 6000, got %d", got.Cost)
	}
}

func TestEscalating_PruneOnNilStateIsSafe(t *testing.T) {
	s := &strategy.Escalating{CPUThreshold: threshold100m, Step: 3000, Max: 9000}
	s.Prune(map[types.UID]bool{"p1": true}) // must not panic before first Decide
}

// ── EscalatingWeighted ────────────────────────────────────────────────────────

// Step 1000 per 1000m: a pod at 1000m gains 1000 per cycle, one at 100m gains 100.
func newWeighted() *strategy.EscalatingWeighted {
	return &strategy.EscalatingWeighted{
		Escalating:   strategy.Escalating{CPUThreshold: threshold100m, Step: 1000, Max: 9000, ResetCost: 0, NoMetricsCost: 10000},
		CPUReference: resource.MustParse("1000m"),
	}
}

func TestEscalatingWeighted_NoMetrics(t *testing.T) {
	got := newWeighted().Decide("p1", nil)
	if got.Cost != 10000 || got.Class != strategy.ClassNoMetrics {
		t.Errorf("Decide = {%d %s}, want {10000 %s}", got.Cost, got.Class, strategy.ClassNoMetrics)
	}
}

func TestEscalatingWeighted_IncrementScalesWithCPU(t *testing.T) {
	tests := []struct {
		cpu  string
		want int32
	}{
		{"1000m", 1000},   // exactly the reference: a full step
		{"2000m", 2000},   // twice the reference: twice the step
		{"200m", 200},     // a fifth of the reference
		{"101m", 101},     // just above the threshold: still escalates
		{"100000m", 9000}, // far above: clamped to Max on the first tick
	}
	for _, tt := range tests {
		t.Run(tt.cpu, func(t *testing.T) {
			cpu := resource.MustParse(tt.cpu)
			got := newWeighted().Decide("p1", &cpu)
			if got.Cost != tt.want || got.Class != strategy.ClassBusy {
				t.Errorf("%s: got {%d %s}, want {%d %s}", tt.cpu, got.Cost, got.Class, tt.want, strategy.ClassBusy)
			}
		})
	}
}

// A pod above the threshold but far below the reference still gains at least 1,
// so it cannot sit at zero cost forever.
func TestEscalatingWeighted_TinyIncrementFloorsAtOne(t *testing.T) {
	s := &strategy.EscalatingWeighted{
		Escalating:   strategy.Escalating{CPUThreshold: threshold100m, Step: 2, Max: 9000},
		CPUReference: resource.MustParse("1000m"),
	}
	cpu := resource.MustParse("101m") // 2 * 101 / 1000 = 0 before the floor
	if got := s.Decide("p1", &cpu); got.Cost != 1 {
		t.Errorf("want 1 (floored), got %d", got.Cost)
	}
}

// A heavy pod out-accumulates a light one over the same number of cycles — the
// whole point of the strategy.
func TestEscalatingWeighted_HeavyPodOutpacesLight(t *testing.T) {
	s := newWeighted()
	heavy, light := resource.MustParse("1000m"), resource.MustParse("200m")

	for i := 0; i < 3; i++ {
		s.Decide("heavy", &heavy)
		s.Decide("light", &light)
	}
	if got := s.Decide("heavy", &heavy); got.Cost != 4000 {
		t.Errorf("heavy after 4 ticks: want 4000, got %d", got.Cost)
	}
	if got := s.Decide("light", &light); got.Cost != 800 {
		t.Errorf("light after 4 ticks: want 800, got %d", got.Cost)
	}
}

func TestEscalatingWeighted_AccumulatesAndCaps(t *testing.T) {
	s := newWeighted()
	cpu := resource.MustParse("4000m") // 4 steps = 4000 per cycle

	for i, want := range []int32{4000, 8000, 9000, 9000} { // capped at Max from tick 3
		got := s.Decide("p1", &cpu)
		if got.Cost != want {
			t.Errorf("tick %d: want %d, got %d", i+1, want, got.Cost)
		}
	}
}

func TestEscalatingWeighted_ResetsOnIdleAndPrunes(t *testing.T) {
	s := newWeighted()
	cpu := resource.MustParse("1000m")

	s.Decide("p1", &cpu)
	s.Decide("p1", &cpu) // 2000
	if got := s.Decide("p1", idle()); got.Cost != 0 || got.Class != strategy.ClassIdle {
		t.Errorf("after idle: got {%d %s}, want {0 %s}", got.Cost, got.Class, strategy.ClassIdle)
	}
	if got := s.Decide("p1", &cpu); got.Cost != 1000 {
		t.Errorf("after reset: want 1000, got %d", got.Cost)
	}

	s.Decide("p2", &cpu)
	s.Prune(map[types.UID]bool{"p2": true}) // p1 is gone
	if got := s.Decide("p1", &cpu); got.Cost != 1000 {
		t.Errorf("pruned pod: want 1000 (counter released), got %d", got.Cost)
	}
	if got := s.Decide("p2", &cpu); got.Cost != 2000 {
		t.Errorf("retained pod: want 2000, got %d", got.Cost)
	}
}

// A zero reference would divide by zero; fall back to an unweighted step instead.
func TestEscalatingWeighted_ZeroReferenceFallsBackToStep(t *testing.T) {
	s := &strategy.EscalatingWeighted{
		Escalating: strategy.Escalating{CPUThreshold: threshold100m, Step: 1000, Max: 9000},
	}
	if got := s.Decide("p1", busy()); got.Cost != 1000 {
		t.Errorf("want 1000 (plain step), got %d", got.Cost)
	}
}

// ── New / Validate ────────────────────────────────────────────────────────────

func TestNew_SelectsStrategy(t *testing.T) {
	p := strategy.Params{CPUThreshold: threshold100m, BusyCost: 10000, NoMetricsCost: 10000}

	tests := []struct {
		name string
		want any
	}{
		{"", &strategy.Threshold{}},
		{strategy.NameThreshold, &strategy.Threshold{}},
		{strategy.NameEscalating, &strategy.Escalating{}},
		{strategy.NameEscalatingWeighted, &strategy.EscalatingWeighted{}},
	}
	for _, tt := range tests {
		t.Run("name="+tt.name, func(t *testing.T) {
			s, err := strategy.New(tt.name, p)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotType, wantType := typeName(s), typeName(tt.want); gotType != wantType {
				t.Errorf("got %s, want %s", gotType, wantType)
			}
		})
	}
}

func TestNew_UnknownStrategy(t *testing.T) {
	_, err := strategy.New("bogus", strategy.Params{})
	if err == nil {
		t.Fatal("expected error for unknown strategy, got nil")
	}
	// The message should name the offender and list the alternatives.
	for _, want := range []string{`"bogus"`, strategy.NameThreshold, strategy.NameEscalating, strategy.NameEscalatingWeighted} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestNew_EscalatingDefaults(t *testing.T) {
	tests := []struct {
		name     string
		params   strategy.Params
		wantStep int32
		wantMax  int32
	}{
		{
			name:     "explicit values are kept",
			params:   strategy.Params{BusyCost: 10000, EscalatingStep: 250, EscalatingMax: 500},
			wantStep: 250,
			wantMax:  500,
		},
		{
			name:     "step defaults to busyCost/10",
			params:   strategy.Params{BusyCost: 10000},
			wantStep: 1000,
			wantMax:  1_000_000,
		},
		{
			name:     "step falls back when busyCost is too small",
			params:   strategy.Params{BusyCost: 5},
			wantStep: 1000,
			wantMax:  1_000_000,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := strategy.New(strategy.NameEscalating, tt.params)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			e, ok := s.(*strategy.Escalating)
			if !ok {
				t.Fatalf("got %T, want *strategy.Escalating", s)
			}
			if e.Step != tt.wantStep {
				t.Errorf("Step: got %d, want %d", e.Step, tt.wantStep)
			}
			if e.Max != tt.wantMax {
				t.Errorf("Max: got %d, want %d", e.Max, tt.wantMax)
			}
		})
	}
}

func TestNew_EscalatingWeightedDefaults(t *testing.T) {
	tests := []struct {
		name    string
		params  strategy.Params
		wantRef string
	}{
		{
			name:    "reference defaults to one core",
			params:  strategy.Params{BusyCost: 10000},
			wantRef: "1000m",
		},
		{
			name:    "explicit reference is kept",
			params:  strategy.Params{BusyCost: 10000, EscalatingCPUReference: resource.MustParse("250m")},
			wantRef: "250m",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := strategy.New(strategy.NameEscalatingWeighted, tt.params)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			w, ok := s.(*strategy.EscalatingWeighted)
			if !ok {
				t.Fatalf("got %T, want *strategy.EscalatingWeighted", s)
			}
			want := resource.MustParse(tt.wantRef)
			if w.CPUReference.Cmp(want) != 0 {
				t.Errorf("CPUReference: got %s, want %s", w.CPUReference.String(), tt.wantRef)
			}
			// The escalating defaults are shared with the unweighted strategy.
			if w.Step != 1000 || w.Max != 1_000_000 {
				t.Errorf("Step/Max: got %d/%d, want 1000/1000000", w.Step, w.Max)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	for _, name := range append(strategy.Names(), "") {
		if err := strategy.Validate(name); err != nil {
			t.Errorf("Validate(%q): unexpected error %v", name, err)
		}
	}
	if err := strategy.Validate("nope"); err == nil {
		t.Error("Validate(\"nope\"): expected error, got nil")
	}
}

func typeName(v any) string {
	switch v.(type) {
	case *strategy.Threshold:
		return "Threshold"
	case *strategy.EscalatingWeighted:
		return "EscalatingWeighted"
	case *strategy.Escalating:
		return "Escalating"
	default:
		return "unknown"
	}
}
