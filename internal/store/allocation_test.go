package store

import (
	"math"
	"testing"
	"time"
)

func perShip(v int64) *int64 { return &v }

func TestAllocationTargets(t *testing.T) {
	got := allocationTargets([]LaneYield{
		{Lane: "C", Closed: 174, PerShipGp: perShip(1_700_000)},
		{Lane: "F", Closed: 81, PerShipGp: perShip(48_000)},
		{Lane: "V", Closed: 3, PerShipGp: perShip(9_000_000)}, // below the gate: ignored, even as the best
		{Lane: "U", Closed: 25, PerShipGp: perShip(-500_000)},
	})
	if got["C"] != allocWeightCeil {
		t.Errorf("the best lane earns the ceiling: %v", got["C"])
	}
	if f := got["F"]; f < 0.29 || f > 0.31 { // 0.25 + 1.75 x 0.028
		t.Errorf("F target %v", f)
	}
	if _, ok := got["V"]; ok {
		t.Error("a lane under the sample gate has no target")
	}
	if got["U"] != allocWeightFloor {
		t.Errorf("a losing lane sits on the floor: %v", got["U"])
	}
	none := allocationTargets([]LaneYield{{Lane: "F", Closed: 81, PerShipGp: perShip(-1)}})
	if len(none) != 0 {
		t.Errorf("no positive yield means no targets: %v", none)
	}
}

func TestStepWeights(t *testing.T) {
	base := map[string]float64{"C": 1.5, "F": 0.5, "B": 0, "V": 0.5}
	targets := map[string]float64{"C": 2, "F": 0.3, "B": 2}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

	// First row: no elapsed time, so the operator's weights exactly.
	w := stepWeights(base, nil, targets, 0)
	if w["C"] != 1.5 || w["F"] != 0.5 || w["B"] != 0 || w["V"] != 0.5 {
		t.Fatalf("first row %v", w)
	}
	// An hour moves at most 0.25/24.
	w = stepWeights(base, map[string]float64{"C": 1.5, "F": 0.5, "V": 0.5}, targets, time.Hour)
	if !near(w["C"], 1.5+0.25/24) || !near(w["F"], 0.5-0.25/24) {
		t.Errorf("one hour %v", w)
	}
	if w["V"] != 0.5 {
		t.Errorf("a lane with no target holds: %v", w["V"])
	}
	if w["B"] != 0 {
		t.Errorf("only the operator moves a lane off 0: %v", w["B"])
	}
	// A long gap can't jump more than a day's step.
	w = stepWeights(base, map[string]float64{"C": 1.0}, targets, 72*time.Hour)
	if !near(w["C"], 1.25) {
		t.Errorf("capped at one day: %v", w["C"])
	}
	// The step lands on the target, without overshoot.
	w = stepWeights(base, map[string]float64{"F": 0.31}, targets, 24*time.Hour)
	if !near(w["F"], 0.3) {
		t.Errorf("landing %v", w["F"])
	}
	// A live lane never falls through the floor, whatever it was before.
	w = stepWeights(base, map[string]float64{"F": 0.1}, map[string]float64{}, time.Hour)
	if w["F"] != allocWeightFloor {
		t.Errorf("floor %v", w["F"])
	}
	// A lane the operator re-enables restarts from its base, not from 0.
	w = stepWeights(base, map[string]float64{"F": 0}, nil, time.Hour)
	if w["F"] != 0.5 {
		t.Errorf("re-enabled lane %v", w["F"])
	}
}

func TestMedianOf(t *testing.T) {
	if medianOf(nil) != nil {
		t.Error("no values, no median")
	}
	if m := medianOf([]float64{3, 1, 2}); *m != 2 {
		t.Errorf("odd %v", *m)
	}
	if m := medianOf([]float64{4, 1, 2, 3}); *m != 2.5 {
		t.Errorf("even %v", *m)
	}
}
