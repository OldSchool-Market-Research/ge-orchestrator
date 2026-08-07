package runner

import (
	"testing"
	"time"

	"github.com/osrs-ge/ge-orchestrator/internal/eval"
	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

func strat(entry, kill int64) store.SidecarStrategy {
	st := store.SidecarStrategy{EntryPrice: entry}
	st.KillPrice = &kill
	return st
}

func TestStopCrossed(t *testing.T) {
	cases := []struct {
		name        string
		entry, kill int64
		ref         int64
		want        bool
	}{
		// The Dragon-dart DOA case: stop below entry, market already under it.
		{"below-stop breached", 1180, 1050, 1028, true},
		{"below-stop safe", 1180, 1050, 1150, false},
		{"below-stop exactly at", 1180, 1050, 1050, true},
		{"above-stop breached", 1000, 1200, 1250, true},
		{"above-stop safe", 1000, 1200, 1100, false},
	}
	for _, c := range cases {
		if got := stopCrossed(strat(c.entry, c.kill), c.ref); got != c.want {
			t.Errorf("%s: stopCrossed(entry=%d kill=%d ref=%d) = %v, want %v",
				c.name, c.entry, c.kill, c.ref, got, c.want)
		}
	}
}

func TestFlipPersistenceVeto(t *testing.T) {
	// Gate is okHours/24 >= 0.4, i.e. at least 10 of 24 hours.
	if reason := flipPersistenceVeto(10, 24, 100); reason != "" {
		t.Errorf("10/24 hours (0.42) should pass, got %q", reason)
	}
	if reason := flipPersistenceVeto(9, 24, 100); reason == "" {
		t.Error("9/24 hours (0.375) should be vetoed")
	}
	// Sparse observation does not loosen the gate: 8 persistent hours out of
	// only 9 observed is still 8/24 of the day.
	if reason := flipPersistenceVeto(8, 9, 100); reason == "" {
		t.Error("8 persistent hours (0.33 of the fixed 24) should be vetoed regardless of obs")
	}
	if reason := flipPersistenceVeto(0, 0, 100); reason == "" {
		t.Error("an item with no both-sides history should be vetoed")
	}
}

func TestRefPricePrefersHigh(t *testing.T) {
	high, low := int64(100), int64(90)
	if got := refPrice(&eval.Snap{High: &high, Low: &low}); got == nil || *got != high {
		t.Errorf("refPrice with both legs = %v, want high leg", got)
	}
	if got := refPrice(&eval.Snap{Low: &low}); got == nil || *got != low {
		t.Errorf("refPrice with low only = %v, want low leg", got)
	}
	if got := refPrice(&eval.Snap{}); got != nil {
		t.Errorf("refPrice with no legs = %v, want nil", got)
	}
}

func TestCalibratedFloorVeto(t *testing.T) {
	flip := func(perCycle int64) store.SidecarStrategy {
		st := store.SidecarStrategy{Archetype: "F"}
		st.ExpectedValue.PerCycleGp = perCycle
		return st
	}
	// factor 1 = no measurement: identical to the raw floor check that
	// already ran, so anything at/above the floor passes.
	if reason := calibratedFloorVeto(flip(400_000), 1); reason != "" {
		t.Errorf("factor 1 at the floor should pass, got %q", reason)
	}
	// factor 0.2: a 400k claim is worth 80k calibrated — vetoed.
	if reason := calibratedFloorVeto(flip(400_000), 0.2); reason == "" {
		t.Error("400k claim at factor 0.2 (80k calibrated) should be vetoed")
	}
	// A claim big enough to clear the floor after correction passes.
	if reason := calibratedFloorVeto(flip(2_100_000), 0.2); reason != "" {
		t.Errorf("2.1M claim at factor 0.2 (420k calibrated) should pass, got %q", reason)
	}
	// Lanes without a floor never trip the rule.
	c := store.SidecarStrategy{Archetype: "C"}
	c.ExpectedValue.PerCycleGp = 1
	if reason := calibratedFloorVeto(c, 0.05); reason != "" {
		t.Errorf("C has no floor, got %q", reason)
	}
}

func TestGraveyardVeto(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	g := store.GraveyardRow{ItemID: 561, Archetype: "F", Kills: 5, EstRealizedGp: -1_204_332,
		LastKilledAt: now.Add(-3 * 24 * time.Hour)}
	if reason := graveyardVeto(g, 14*24*time.Hour, now); reason == "" {
		t.Error("kill 3d ago inside a 14d cooldown should veto")
	}
	if reason := graveyardVeto(g, 2*24*time.Hour, now); reason != "" {
		t.Errorf("kill 3d ago outside a 2d cooldown should pass, got %q", reason)
	}
	if reason := graveyardVeto(g, 0, now); reason != "" {
		t.Errorf("cooldown 0 disables the veto, got %q", reason)
	}
}

func TestRecordFactorNilSafe(t *testing.T) {
	var rec *record
	if f := rec.Factor("F"); f != 1 {
		t.Errorf("nil record factor = %v, want 1", f)
	}
	rec = &record{factors: map[string]float64{"F": 0.21, "B": 0}}
	if f := rec.Factor("F"); f != 0.21 {
		t.Errorf("factor = %v, want 0.21", f)
	}
	// A zero factor is a degenerate row, not a license to veto everything.
	if f := rec.Factor("B"); f != 1 {
		t.Errorf("zero-factor fallback = %v, want 1", f)
	}
	if f := rec.Factor("C"); f != 1 {
		t.Errorf("unmeasured archetype = %v, want 1", f)
	}
}
