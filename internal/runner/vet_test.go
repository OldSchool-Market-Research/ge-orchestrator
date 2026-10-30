package runner

import (
	"strings"
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
	// C holds its own 200k floor: 500k x 0.3 = 150k.
	c := store.SidecarStrategy{Archetype: "C"}
	c.ExpectedValue.PerCycleGp = 500_000
	if reason := calibratedFloorVeto(c, 0.3); !strings.Contains(reason, "200000 lane-C floor") {
		t.Errorf("C under its calibrated floor: %q", reason)
	}
	// Lanes without a floor never trip the rule.
	v := store.SidecarStrategy{Archetype: "V"}
	v.ExpectedValue.PerCycleGp = 1
	if reason := calibratedFloorVeto(v, 0.05); reason != "" {
		t.Errorf("V has no floor, got %q", reason)
	}
}

func TestFlipProjectionVeto(t *testing.T) {
	per1h := func(v int64) *int64 { return &v }
	if reason := flipProjectionVeto(nil); !strings.Contains(reason, "null") {
		t.Errorf("a null projection must veto: %q", reason)
	}
	if reason := flipProjectionVeto(per1h(99_999)); !strings.Contains(reason, "399996") {
		t.Errorf("99,999/hr is 399,996 per cycle: %q", reason)
	}
	if reason := flipProjectionVeto(per1h(100_000)); reason != "" {
		t.Errorf("100k/hr is exactly the floor: %q", reason)
	}
}

func TestEvSanityHasNoSlack(t *testing.T) {
	margin := int64(100)
	st := store.SidecarStrategy{Archetype: "F"}
	st.Size.UnitsUsed = 5_000
	snap := &eval.Snap{Margin: &margin, Vol30m: 100_000} // fillable 120k: units bind, ceiling 500k
	st.ExpectedValue.PerCycleGp = 500_000
	if reason := evSanity(st, snap); reason != "" {
		t.Errorf("a claim at the ceiling passes: %q", reason)
	}
	st.ExpectedValue.PerCycleGp = 500_001
	if reason := evSanity(st, snap); reason == "" {
		t.Error("a claim above the ceiling must veto")
	}
}

func TestRecordKindFactorFallsBackToLane(t *testing.T) {
	measured := 0.7
	rec := &record{cal: map[string]store.CalibrationRow{
		"C":         {Archetype: "C", Factor: 0.3, PSurvive: &measured},
		"C:set":     {Archetype: "C:set", Factor: 0.6, PSurvive: &measured},
		"C:combine": {Archetype: "C:combine", Factor: 0.15}, // below its sample gate
	}}
	set := store.SidecarStrategy{Archetype: "C", RelationKind: "set"}
	decant := store.SidecarStrategy{Archetype: "C", RelationKind: "decant"}
	combine := store.SidecarStrategy{Archetype: "C", RelationKind: "combine"}
	if f := rec.FactorFor(set); f != 0.6 {
		t.Errorf("set factor %v", f)
	}
	if f := rec.FactorFor(decant); f != 0.3 {
		t.Errorf("a kind with no row falls back to its lane: %v", f)
	}
	if f := rec.FactorFor(combine); f != 0.3 {
		t.Errorf("a kind below its sample gate falls back to its lane: %v", f)
	}
	if row := rec.CalRowFor(set); row == nil || row.Archetype != "C:set" {
		t.Errorf("set row %+v", row)
	}
	if row := rec.CalRowFor(decant); row == nil || row.Archetype != "C" {
		t.Errorf("decant row %+v", row)
	}
	if row := rec.CalRowFor(combine); row == nil || row.Archetype != "C" {
		t.Errorf("combine row %+v", row)
	}
	// A measured family beats its kind; a thin one falls through to it.
	rec.cal["C:moon"] = store.CalibrationRow{Archetype: "C:moon", Factor: 0.8, PSurvive: &measured}
	rec.cal["C:robes"] = store.CalibrationRow{Archetype: "C:robes", Factor: 0.15}
	moon := store.SidecarStrategy{Archetype: "C", RelationKind: "set", RelationFamily: "moon"}
	robes := store.SidecarStrategy{Archetype: "C", RelationKind: "set", RelationFamily: "robes"}
	if f := rec.FactorFor(moon); f != 0.8 {
		t.Errorf("moon factor %v", f)
	}
	if f := rec.FactorFor(robes); f != 0.6 {
		t.Errorf("a thin family falls back to its kind: %v", f)
	}
	if kind, family := relationLabels(nil); kind != "" || family != "" {
		t.Errorf("no recipe, no labels: %q %q", kind, family)
	}
	var none *record
	if f := none.FactorFor(set); f != 1 {
		t.Errorf("nil record: %v", f)
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

func TestComboBuyLimitVeto(t *testing.T) {
	// Prayer potion 3->4: buy 4x 3-dose (limit 2000), sell 3x 4-dose.
	legs := []store.Leg{
		{ItemID: 139, Side: "buy", Qty: 4, Price: 6_900},
		{ItemID: 2434, Side: "sell", Qty: 3, Price: 9_700},
	}
	limits := map[int]int64{139: 2000, 2434: 2000}
	st := comboStrat(1, 27_600, 28_518, legs)

	st.Size.UnitsUsed = 2000 // the per-item limit read as conversions: 8000 buys
	if reason := comboBuyLimitVeto(st, limits); !strings.Contains(reason, "at most 500") {
		t.Errorf("2000 conversions x 4 must be vetoed with the 500 bound, got %q", reason)
	}
	st.Size.UnitsUsed = 500 // exactly the limit
	if reason := comboBuyLimitVeto(st, limits); reason != "" {
		t.Errorf("500 conversions x 4 = 2000 fits, got %q", reason)
	}
	st.Size.UnitsUsed = 2000 // sell legs and unknown limits never bind
	if reason := comboBuyLimitVeto(st, map[int]int64{2434: 10}); reason != "" {
		t.Errorf("unknown buy-leg limit must fail open, got %q", reason)
	}

	// several buy legs: the tightest bound is reported, every time (map order)
	set := []store.Leg{
		{ItemID: 139, Name: "Prayer potion(3)", Side: "buy", Qty: 4, Price: 6_900},
		{ItemID: 777, Name: "Rare piece", Side: "buy", Qty: 1, Price: 1},
		{ItemID: 2434, Side: "sell", Qty: 3, Price: 9_700},
	}
	st = comboStrat(1, 27_600, 28_518, set)
	st.Size.UnitsUsed = 200
	for i := 0; i < 50; i++ {
		if reason := comboBuyLimitVeto(st, map[int]int64{139: 2000, 777: 8}); !strings.Contains(reason, "at most 8 here") || !strings.Contains(reason, "Rare piece") {
			t.Fatalf("want the tightest bound (8, Rare piece), got %q", reason)
		}
	}
	// a limit below the per-conversion qty can never fit
	if reason := comboBuyLimitVeto(st, map[int]int64{139: 3}); !strings.Contains(reason, "untradeable") {
		t.Errorf("limit 3 < 4 per conversion must say untradeable, got %q", reason)
	}
}

func TestComboFloorVeto(t *testing.T) {
	legs := []store.Leg{
		{ItemID: 139, Side: "buy", Qty: 4, Price: 6_900},
		{ItemID: 2434, Side: "sell", Qty: 3, Price: 9_700},
	}
	st := comboStrat(1, 27_600, 28_518, legs) // 918 gp per conversion
	st.Size.UnitsUsed = 500                   // 459,000 per batch
	if reason := comboFloorVeto(st); reason != "" {
		t.Errorf("459k per batch clears the floor, got %q", reason)
	}
	st.Size.UnitsUsed = 200 // 183,600: resized to the limit, still claiming 459k
	if reason := comboFloorVeto(st); !strings.Contains(reason, "under the C floor") {
		t.Errorf("183,600 per batch must be vetoed, got %q", reason)
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

func TestSizeConsistencyVeto(t *testing.T) {
	const budget = 50_000_000
	cases := []struct {
		name              string
		units, entry, cap int64
		vetoed            string // substring of the reason; empty = accepted
	}{
		// strategy 1909: units_used was the shard buy limit, capital priced on 10
		{"1909 raw buy limit", 10_000, 1_491_846, 14_918_460, "over the 50000000 research budget"},
		{"1909 as priced", 10, 1_491_846, 14_918_460, ""},
		// Justiciar set: sized at its buy bound of 8, capital for one
		{"big conversion at its buy bound", 8, 29_400_000, 29_400_000, "over the 50000000 research budget"},
		{"big conversion sized by the budget", 1, 29_400_000, 29_400_000, ""},
		{"dose family 4x", 2_000, 25_840, 12_920_000, "size fields disagree"},
		{"capital 9% under units x entry", 109, 100_000, 10_000_000, ""},
		{"capital 20% under units x entry", 125, 100_000, 10_000_000, "size fields disagree"},
		{"overstated capital is not vetoed", 80, 100_000, 10_000_000, ""},
		{"capital 0 still hits the budget", 10_000, 1_491_846, 0, "over the 50000000 research budget"},
		{"capital 0 within budget", 10, 1_000, 0, ""},
		{"no units: left to other rules", 0, 1_000, 10_000, ""},
	}
	for _, c := range cases {
		st := store.SidecarStrategy{EntryPrice: c.entry, CapitalRequired: c.cap}
		st.Size.UnitsUsed = c.units
		reason := sizeConsistencyVeto(st, budget)
		if c.vetoed == "" && reason != "" {
			t.Errorf("%s: vetoed %q, want accepted", c.name, reason)
		}
		if c.vetoed != "" && !strings.Contains(reason, c.vetoed) {
			t.Errorf("%s: reason %q, want one containing %q", c.name, reason, c.vetoed)
		}
	}
}

func TestSizeOverstatedCapitalIsOnlyNoted(t *testing.T) {
	st := store.SidecarStrategy{EntryPrice: 100_000, CapitalRequired: 10_000_000}
	st.Size.UnitsUsed = 80 // 8M of units against 10M of capital
	if note := sizeOverstatedCapital(st); !strings.Contains(note, "not vetoed") {
		t.Errorf("got %q, want an overstated-capital note", note)
	}
	st.Size.UnitsUsed = 100
	if note := sizeOverstatedCapital(st); note != "" {
		t.Errorf("consistent sizes: got note %q", note)
	}
}

func TestProcessVeto(t *testing.T) {
	proj := func(v int64) *int64 { return &v }
	good := &eval.ProcessSnap{Throughput: "fixed"}
	if reason := processVeto(good, proj(250_000)); reason != "" {
		t.Errorf("exactly the floor: %q", reason)
	}
	cases := []struct {
		name string
		snap eval.ProcessSnap
		proj *int64
		want string
	}{
		{"under the floor", eval.ProcessSnap{Throughput: "fixed"}, proj(249_999), "lane-P floor"},
		{"linked", eval.ProcessSnap{Throughput: "fixed", Linked: true}, proj(900_000), "lane C"},
		{"no rate", eval.ProcessSnap{Throughput: "unknown"}, proj(900_000), "no known rate"},
		{"no limit", eval.ProcessSnap{Throughput: "fixed", UnknownLimitLegs: 1}, proj(900_000), "buy limit"},
		{"no price or no earnings", eval.ProcessSnap{Throughput: "fixed"}, nil, "doesn't earn"},
	}
	for _, c := range cases {
		if reason := processVeto(&c.snap, c.proj); !strings.Contains(reason, c.want) {
			t.Errorf("%s: %q", c.name, reason)
		}
	}
}

func TestProcessPersistenceVeto(t *testing.T) {
	for _, c := range []struct {
		positive, total int
		veto            bool
	}{
		{12, 12, false}, {20, 24, false}, {19, 24, true}, {11, 11, true}, {0, 0, true},
	} {
		if got := processPersistenceVeto(c.positive, c.total) != ""; got != c.veto {
			t.Errorf("%d of %d: veto=%v", c.positive, c.total, got)
		}
	}
}
