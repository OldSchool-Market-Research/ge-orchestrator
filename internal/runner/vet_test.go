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

// moonSetRelation is the CORRECTED Blood moon recipe (four components); the
// incident strategy's legs cover only three of them.
func moonSetRelation() *store.Relation {
	return &store.Relation{
		RelationID: 104, Kind: "set", Name: "Blood moon armour set", Reversible: true,
		Inputs: []store.RelationLeg{
			{ItemID: 29028, Qty: 1}, {ItemID: 29022, Qty: 1}, {ItemID: 29025, Qty: 1}, {ItemID: 28997, Qty: 1},
		},
		Outputs: []store.RelationLeg{{ItemID: 31136, Qty: 1}},
	}
}

// comboStrat builds a C strategy with the given legs and per-conversion
// entry/exit; the incident fixture numbers are strategy 1291's real ones.
func comboStrat(relID int, entry, exit int64, legs []store.Leg) store.SidecarStrategy {
	st := store.SidecarStrategy{Archetype: "C", EntryPrice: entry, ExitPrice: exit, Legs: legs}
	st.RelationID = &relID
	return st
}

func TestComboRecipeVetoIntegrity(t *testing.T) {
	rel := moonSetRelation()
	full := []store.Leg{
		{ItemID: 29028, Side: "buy", Qty: 1, Price: 788_888},
		{ItemID: 29022, Side: "buy", Qty: 1, Price: 3_820_985},
		{ItemID: 29025, Side: "buy", Qty: 1, Price: 8_874_095},
		{ItemID: 28997, Side: "buy", Qty: 1, Price: 5_750_000},
		{ItemID: 31136, Side: "sell", Qty: 1, Price: 19_220_000},
	}
	// Complete recipe at honest prices: margin 19.22M*0.98 - 19.23M < 0 is
	// the floors' problem, not this gate's — it must pass here.
	if reason := comboRecipeVeto(comboStrat(104, 19_233_968, 18_835_600, full), rel); reason != "" {
		t.Errorf("complete legs should pass integrity, got %q", reason)
	}
	// The incident: three of four components. entry/exit are 1291's values.
	partial := append(append([]store.Leg{}, full[:3]...), full[4])
	if reason := comboRecipeVeto(comboStrat(104, 13_483_968, 18_835_600, partial), rel); reason == "" {
		t.Error("missing macuahuitl leg must be vetoed (the 197M phantom)")
	}
	// Reverse direction (buy the set, break, sell components) is legal on a
	// reversible relation.
	var reversed []store.Leg
	for _, l := range full {
		side := "buy"
		if l.Side == "buy" {
			side = "sell"
		}
		reversed = append(reversed, store.Leg{ItemID: l.ItemID, Side: side, Qty: l.Qty, Price: l.Price})
	}
	if reason := comboRecipeVeto(comboStrat(104, 19_220_000, 19_100_000, reversed), rel); reason != "" {
		t.Errorf("reversed legs on reversible relation should pass, got %q", reason)
	}
	irrev := moonSetRelation()
	irrev.Reversible = false
	if reason := comboRecipeVeto(comboStrat(104, 19_220_000, 19_100_000, reversed), irrev); reason == "" {
		t.Error("reversed legs on a non-reversible relation must be vetoed")
	}
	// No/unknown relation.
	if reason := comboRecipeVeto(comboStrat(104, 1, 2, full), nil); reason == "" {
		t.Error("unknown relation_id must be vetoed")
	}
	st := comboStrat(104, 1, 2, full)
	st.RelationID = nil
	if reason := comboRecipeVeto(st, rel); reason == "" {
		t.Error("missing relation_id must be vetoed")
	}
}

func TestComboRecipeVetoNoArb(t *testing.T) {
	rel := moonSetRelation()
	legs := []store.Leg{
		{ItemID: 29028, Side: "buy", Qty: 1, Price: 788_888},
		{ItemID: 29022, Side: "buy", Qty: 1, Price: 3_820_985},
		{ItemID: 29025, Side: "buy", Qty: 1, Price: 8_874_095},
		{ItemID: 28997, Side: "buy", Qty: 1, Price: 100_000}, // absurd quote keeps legs matching while margin stays incident-sized
		{ItemID: 31136, Side: "sell", Qty: 1, Price: 19_220_000},
	}
	// 5.25M margin on a 19.22M sell side (27%) of a free reversible set:
	// data error by construction.
	if reason := comboRecipeVeto(comboStrat(104, 13_583_968, 18_835_600, legs), rel); reason == "" {
		t.Error("free-conversion margin at 28 percent of the set must be vetoed as bad data")
	}
	// A believable 1.9% set premium (367k on 19.22M) passes.
	if reason := comboRecipeVeto(comboStrat(104, 18_468_600, 18_835_600, legs), rel); reason != "" {
		t.Errorf("2%% set premium should pass, got %q", reason)
	}
	// Over the fraction but under the absolute floor passes: a 12% decant
	// margin on a cheap potion is routine dose-book mispricing.
	decant := &store.Relation{
		RelationID: 12, Kind: "decant", Name: "Prayer potion 3<->4", Reversible: true,
		Inputs:  []store.RelationLeg{{ItemID: 139, Qty: 4}},
		Outputs: []store.RelationLeg{{ItemID: 2434, Qty: 3}},
	}
	dlegs := []store.Leg{
		{ItemID: 139, Side: "buy", Qty: 4, Price: 8_000},
		{ItemID: 2434, Side: "sell", Qty: 3, Price: 12_500},
	}
	if reason := comboRecipeVeto(comboStrat(12, 32_000, 36_000, dlegs), decant); reason != "" {
		t.Errorf("4k margin (11%%) under the 500k floor should pass, got %q", reason)
	}
	// Combines are exempt: skill gates and NPC fees let real margins persist.
	combine := &store.Relation{
		RelationID: 55, Kind: "combine", Name: "Arcane spirit shield", Reversible: false,
		Inputs:  []store.RelationLeg{{ItemID: 12831, Qty: 1}, {ItemID: 12827, Qty: 1}},
		Outputs: []store.RelationLeg{{ItemID: 12825, Qty: 1}},
	}
	clegs := []store.Leg{
		{ItemID: 12831, Side: "buy", Qty: 1, Price: 45_000_000},
		{ItemID: 12827, Side: "buy", Qty: 1, Price: 30_000_000},
		{ItemID: 12825, Side: "sell", Qty: 1, Price: 90_000_000},
	}
	if reason := comboRecipeVeto(comboStrat(55, 75_000_000, 88_200_000, clegs), combine); reason != "" {
		t.Errorf("13.2M margin on a 90 Prayer combine should pass (skill-gated), got %q", reason)
	}
}
