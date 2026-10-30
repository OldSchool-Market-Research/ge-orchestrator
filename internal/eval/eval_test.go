package eval

import (
	"context"
	"testing"
	"time"

	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

// fakeSource scripts every PriceSource answer.
type fakeSource struct {
	snaps   map[int]*Snap
	winBuy  WindowStats
	winSell WindowStats
	z       float64
	zN      int
	zMove   *float64
}

func (f *fakeSource) Snapshot(_ context.Context, id int) (*Snap, error) { return f.snaps[id], nil }
func (f *fakeSource) SnapshotMany(_ context.Context, ids []int) (map[int]*Snap, error) {
	out := map[int]*Snap{}
	for _, id := range ids {
		out[id] = f.snaps[id]
	}
	return out, nil
}
func (f *fakeSource) WindowStats(_ context.Context, _ int, _ time.Time, w Window) (WindowStats, error) {
	if w.FromHow == 50 { // the tests' buy window
		return f.winBuy, nil
	}
	return f.winSell, nil
}
func (f *fakeSource) VolumeZ(_ context.Context, _ int, _ time.Duration) (float64, int, *float64, error) {
	return f.z, f.zN, f.zMove, nil
}

func i64(v int64) *int64    { return &v }
func iptr(v int) *int       { return &v }
func sptr(s string) *string { return &s }

// Tue 2026-07-14 12:00 UTC: weekday 2 -> how = 2*24+12 = 60.
var tue1200 = time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

func TestHourOfWeekConvention(t *testing.T) {
	if got := HourOfWeek(tue1200); got != 60 {
		t.Fatalf("Tue 12:00 UTC should be bucket 60, got %d", got)
	}
	sun0 := time.Date(2026, 7, 12, 0, 30, 0, 0, time.UTC) // Sunday 00:xx
	if got := HourOfWeek(sun0); got != 0 {
		t.Fatalf("Sun 00:30 UTC should be bucket 0, got %d", got)
	}
	sat23 := time.Date(2026, 7, 18, 23, 0, 0, 0, time.UTC)
	if got := HourOfWeek(sat23); got != 167 {
		t.Fatalf("Sat 23:00 UTC should be bucket 167, got %d", got)
	}
}

func fixedEvaluator(src PriceSource, at time.Time) *Evaluator {
	return &Evaluator{Source: src, Now: func() time.Time { return at }}
}

func seasonalStrategy() store.Strategy {
	return store.Strategy{
		StrategyID: 1, Archetype: "S", PrimaryItemID: 100,
		EntryPrice: 240, ExitPrice: 265, KillPrice: i64(210),
		UnitsUsed: i64(20000), PerCycleGp: i64(300000), Per1hGp: i64(1785),
		BuyWindow:  &store.HourWindow{FromHow: 50, ToHow: 53},   // Tue 02:00-05:00 in the fake
		SellWindow: &store.HourWindow{FromHow: 162, ToHow: 165}, // Sat evening
		OpenedAt:   tue1200.Add(-24 * time.Hour), State: "open",
		EvalWindow: 168 * time.Hour,
	}
}

func TestSeasonalOutOfWindowNeverDegrades(t *testing.T) {
	// Tue 12:00 (bucket 60) is outside both windows; entry unreachable prices
	// must not degrade the strategy.
	src := &fakeSource{
		snaps: map[int]*Snap{100: {
			High: i64(500), Low: i64(490), // way above entry — irrelevant out of window
			HighAgeS: iptr(60), LowAgeS: iptr(60), Vol30m: 100000,
		}},
		winBuy:  WindowStats{MedLow: i64(240), MedHigh: i64(245), Volume: 500000, Obs: 10},
		winSell: WindowStats{MedLow: i64(260), MedHigh: i64(266), Volume: 400000, Obs: 8},
	}
	ev := fixedEvaluator(src, tue1200)
	e, checks, err := ev.computeSeasonal(context.Background(), seasonalStrategy())
	if err != nil {
		t.Fatal(err)
	}
	if !checks["out_of_window"] {
		t.Fatal("expected out_of_window")
	}
	if e.Verdict == "degraded" {
		t.Fatalf("out-of-window tick must not degrade, got %s", e.Verdict)
	}
	if e.RealizedPer1hGp == nil {
		t.Fatal("realized should compute from window stats")
	}
	// Raw per-cycle: units 20000 x (266 - 266/50=5 - 240) = 20000 x 21 = 420000; /168 = 2500.
	// The scalar column carries the HAIRCUT figure: capped units =
	// 0.15 x min(vols)=400000 -> 60000 > units, so units bind; slip 0.5%:
	// buy 240*1.005=241 (int), sell 266*0.995=264 (int), tax 264/50=5:
	// 20000 x (264-5-241) = 360000; /168 = 2142.
	if *e.RealizedPer1hGp != 2142 {
		t.Fatalf("haircut realized/1h: want 2142, got %d", *e.RealizedPer1hGp)
	}
}

func TestSeasonalInBuyWindowGating(t *testing.T) {
	tue0300 := time.Date(2026, 7, 14, 3, 0, 0, 0, time.UTC) // bucket 51, inside buy window
	src := &fakeSource{
		snaps: map[int]*Snap{100: {
			High: i64(250), Low: i64(300), // low 300 > entry 240*1.02 -> unreachable
			HighAgeS: iptr(60), LowAgeS: iptr(60), Vol30m: 100000,
		}},
		winBuy:  WindowStats{MedLow: i64(240), MedHigh: i64(245), Volume: 500000, Obs: 10},
		winSell: WindowStats{MedLow: i64(260), MedHigh: i64(266), Volume: 400000, Obs: 8},
	}
	ev := fixedEvaluator(src, tue0300)
	e, checks, err := ev.computeSeasonal(context.Background(), seasonalStrategy())
	if err != nil {
		t.Fatal(err)
	}
	if checks["entry_reachable"] {
		t.Fatal("entry should be unreachable at low=300")
	}
	if e.Verdict != "degraded" {
		t.Fatalf("in-window unreachable entry should degrade, got %s", e.Verdict)
	}
}

func TestSeasonalWindowGapDeadKills(t *testing.T) {
	// Observed windows show the sell side BELOW the buy side post-tax.
	src := &fakeSource{
		snaps: map[int]*Snap{100: {
			High: i64(250), Low: i64(245), HighAgeS: iptr(60), LowAgeS: iptr(60), Vol30m: 100000,
		}},
		winBuy:  WindowStats{MedLow: i64(260), MedHigh: i64(262), Volume: 500000, Obs: 10},
		winSell: WindowStats{MedLow: i64(255), MedHigh: i64(258), Volume: 400000, Obs: 9},
	}
	ev := fixedEvaluator(src, tue1200)
	e, _, err := ev.computeSeasonal(context.Background(), seasonalStrategy())
	if err != nil {
		t.Fatal(err)
	}
	if e.Verdict != "kill_signal" {
		t.Fatalf("dead observed window gap should kill, got %s", e.Verdict)
	}
}

func TestSeasonalWrapWindow(t *testing.T) {
	// Buy window wraps the week end: Sat 22:00 - Sun 01:00 (166..1).
	st := seasonalStrategy()
	st.BuyWindow = &store.HourWindow{FromHow: 166, ToHow: 1}
	st.SellWindow = &store.HourWindow{FromHow: 80, ToHow: 83}
	sun0030 := time.Date(2026, 7, 12, 0, 30, 0, 0, time.UTC) // bucket 0 — inside wrap
	src := &fakeSource{
		snaps: map[int]*Snap{100: {
			High: i64(250), Low: i64(238), HighAgeS: iptr(60), LowAgeS: iptr(60), Vol30m: 200000,
		}},
		// Window keyed on FromHow==50 in the fake; wrap window returns winSell.
		winBuy:  WindowStats{},
		winSell: WindowStats{MedLow: i64(240), MedHigh: i64(266), Volume: 400000, Obs: 9},
	}
	ev := fixedEvaluator(src, sun0030)
	_, checks, err := ev.computeSeasonal(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if checks["out_of_window"] {
		t.Fatal("Sun 00:30 must count as inside the wrapping buy window")
	}
	if !checks["entry_reachable"] {
		t.Fatal("low 238 <= 240*1.02 should be reachable")
	}
}

func volumeStrategy(state string) store.Strategy {
	return store.Strategy{
		StrategyID: 2, Archetype: "V", PrimaryItemID: 200,
		EntryPrice: 95, ExitPrice: 120, KillPrice: i64(80),
		UnitsUsed: i64(25000), PerCycleGp: i64(500000), Per1hGp: i64(20000),
		Trigger:   &store.Trigger{Metric: "volume_zscore", Threshold: 4, Direction: "above", Window: "1h"},
		Direction: sptr("ride"),
		OpenedAt:  tue1200.Add(-2 * time.Hour), State: state,
		EvalWindow: 96 * time.Hour,
	}
}

func TestArmedTriggerFires(t *testing.T) {
	src := &fakeSource{z: 5.2, zN: 100}
	ev := fixedEvaluator(src, tue1200)
	e, checks, err := ev.computeVolume(context.Background(), volumeStrategy("armed"))
	if err != nil {
		t.Fatal(err)
	}
	if !checks["trigger_fired"] {
		t.Fatal("z=5.2 above threshold 4 should fire")
	}
	if e.Verdict != "healthy" {
		t.Fatalf("armed tick verdict should be healthy, got %s", e.Verdict)
	}
}

func TestArmedTriggerBelowDirection(t *testing.T) {
	st := volumeStrategy("armed")
	st.Trigger.Direction = "below"
	src := &fakeSource{z: -4.5, zN: 100}
	ev := fixedEvaluator(src, tue1200)
	_, checks, err := ev.computeVolume(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if !checks["trigger_fired"] {
		t.Fatal("z=-4.5 with direction=below threshold 4 should fire")
	}
	src.z = -2
	_, checks, _ = ev.computeVolume(context.Background(), st)
	if checks["trigger_fired"] {
		t.Fatal("z=-2 must not fire threshold 4")
	}
}

func TestArmedThinBaselineNeverFires(t *testing.T) {
	src := &fakeSource{z: 50, zN: 2} // huge z but 2 baseline samples
	ev := fixedEvaluator(src, tue1200)
	_, checks, err := ev.computeVolume(context.Background(), volumeStrategy("armed"))
	if err != nil {
		t.Fatal(err)
	}
	if checks["trigger_fired"] {
		t.Fatal("n_baseline < 3 must not fire")
	}
}

func TestVolumeOpenRideAndFade(t *testing.T) {
	// Price moved 95 -> 110.
	snap := &Snap{High: i64(111), Low: i64(109), HighAgeS: iptr(60), LowAgeS: iptr(60), Vol30m: 50000}
	st := volumeStrategy("open")
	trig := tue1200.Add(-4 * time.Hour)
	st.TriggeredAt = &trig

	ev := fixedEvaluator(&fakeSource{snaps: map[int]*Snap{200: snap}}, tue1200)
	e, _, err := ev.computeVolume(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if e.RealizedPer1hGp == nil || *e.RealizedPer1hGp <= 0 {
		t.Fatalf("ride with price up should realize positive, got %v", e.RealizedPer1hGp)
	}

	st.Direction = sptr("fade")
	e2, _, err := ev.computeVolume(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if e2.RealizedPer1hGp == nil || *e2.RealizedPer1hGp >= 0 {
		t.Fatalf("fade with price up should realize negative, got %v", e2.RealizedPer1hGp)
	}
}

func TestVolumeKillBreach(t *testing.T) {
	// kill 80 below entry 95: breach when price falls to/below 80.
	snap := &Snap{High: i64(78), Low: i64(76), HighAgeS: iptr(60), LowAgeS: iptr(60), Vol30m: 50000}
	st := volumeStrategy("open")
	ev := fixedEvaluator(&fakeSource{snaps: map[int]*Snap{200: snap}}, tue1200)
	e, _, err := ev.computeVolume(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if e.Verdict != "kill_signal" {
		t.Fatalf("price through the stop should kill, got %s", e.Verdict)
	}
}

func comboStrategy() store.Strategy {
	return store.Strategy{
		StrategyID: 3, Archetype: "C", PrimaryItemID: 2434,
		EntryPrice: 27600, ExitPrice: 27930, // projected margin 330/conversion
		UnitsUsed: i64(1000), PerCycleGp: i64(330000), Per1hGp: i64(82500),
		Legs: []store.Leg{
			{ItemID: 139, Name: "Prayer potion(3)", Side: "buy", Qty: 4, Price: 6900},
			{ItemID: 2434, Name: "Prayer potion(4)", Side: "sell", Qty: 3, Price: 9500},
		},
		OpenedAt: tue1200.Add(-time.Hour), State: "open", EvalWindow: 48 * time.Hour,
	}
}

func TestComboMarginMath(t *testing.T) {
	src := &fakeSource{snaps: map[int]*Snap{
		139:  {Low: i64(6900), High: i64(7000), LowAgeS: iptr(120), HighAgeS: iptr(120), Vol30m: 9000, Vol4h: 72000, HighTicks24h: 288, LowTicks24h: 288},
		2434: {Low: i64(9300), High: i64(9500), LowAgeS: iptr(90), HighAgeS: iptr(90), Vol30m: 6000, Vol4h: 48000, HighTicks24h: 288, LowTicks24h: 288},
	}}
	ev := fixedEvaluator(src, tue1200)
	e, checks, err := ev.computeCombo(context.Background(), comboStrategy())
	if err != nil {
		t.Fatal(err)
	}
	// Raw: sell 3x(9500-190) - buy 4x6900 = 27930-27600 = 330; x 1000 = 330k >= 100k ✓
	if !checks["margin_ok"] || !checks["legs_fresh"] || !checks["legs_priced"] {
		t.Fatalf("checks: %v", checks)
	}
	if e.Verdict != "healthy" {
		t.Fatalf("got %s", e.Verdict)
	}
}

func TestComboNullLegDegrades(t *testing.T) {
	src := &fakeSource{snaps: map[int]*Snap{
		139:  {Low: nil, High: i64(7000), LowAgeS: nil, HighAgeS: iptr(120), Vol30m: 9000, Vol4h: 72000, HighTicks24h: 288, LowTicks24h: 288},
		2434: {Low: i64(9300), High: i64(9500), LowAgeS: iptr(90), HighAgeS: iptr(90), Vol30m: 6000, Vol4h: 48000, HighTicks24h: 288, LowTicks24h: 288},
	}}
	ev := fixedEvaluator(src, tue1200)
	e, checks, err := ev.computeCombo(context.Background(), comboStrategy())
	if err != nil {
		t.Fatal(err)
	}
	if checks["legs_priced"] {
		t.Fatal("null buy leg must fail legs_priced")
	}
	if e.Verdict != "degraded" {
		t.Fatalf("null leg should degrade (not kill), got %s", e.Verdict)
	}
	if e.RealizedPer1hGp != nil {
		t.Fatal("unpriceable conversion must not fabricate realized value")
	}
}

func TestComboCollapsedMarginKills(t *testing.T) {
	src := &fakeSource{snaps: map[int]*Snap{
		139:  {Low: i64(7200), High: i64(7300), LowAgeS: iptr(120), HighAgeS: iptr(120), Vol30m: 9000, Vol4h: 72000, HighTicks24h: 288, LowTicks24h: 288},
		2434: {Low: i64(9300), High: i64(9400), LowAgeS: iptr(90), HighAgeS: iptr(90), Vol30m: 6000, Vol4h: 48000, HighTicks24h: 288, LowTicks24h: 288},
	}}
	// Raw margin: 3x(9400-188) - 4x7200 = 27636 - 28800 = -1164 per conversion.
	ev := fixedEvaluator(src, tue1200)
	e, _, err := ev.computeCombo(context.Background(), comboStrategy())
	if err != nil {
		t.Fatal(err)
	}
	if e.Verdict != "kill_signal" {
		t.Fatalf("collapsed combo margin should kill, got %s", e.Verdict)
	}
}

func TestSellTaxCap(t *testing.T) {
	if sellTax(100) != 2 {
		t.Fatalf("100/50=2, got %d", sellTax(100))
	}
	if sellTax(1_000_000_000) != 5_000_000 {
		t.Fatalf("tax must cap at 5M, got %d", sellTax(1_000_000_000))
	}
}

func TestHaircutUnits(t *testing.T) {
	ev := &Evaluator{}
	if got := ev.haircutUnits(20000, 400000); got != 20000 {
		t.Fatalf("units under participation cap should pass through, got %d", got)
	}
	if got := ev.haircutUnits(20000, 10000); got != 1500 {
		t.Fatalf("cap = 0.15*10000 = 1500, got %d", got)
	}
}

func TestHoldRelaxedGates(t *testing.T) {
	st := store.Strategy{
		StrategyID: 4, Archetype: "H", PrimaryItemID: 300,
		EntryPrice: 14800, ExitPrice: 15900, KillPrice: i64(13500),
		UnitsUsed: i64(100), Per1hGp: i64(327),
		OpenedAt: tue1200.Add(-100 * time.Hour), State: "open", EvalWindow: 336 * time.Hour,
	}
	// 2h-old legs: stale for a flip, fine for a hold.
	snap := &Snap{High: i64(15100), Low: i64(14900), HighAgeS: iptr(7200), LowAgeS: iptr(7200), Vol30m: 100}
	ev := fixedEvaluator(&fakeSource{snaps: map[int]*Snap{300: snap}}, tue1200)
	e, checks, err := ev.computeHold(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if !checks["legs_fresh"] {
		t.Fatal("2h-old legs should be fresh under the 6h hold threshold")
	}
	if e.Verdict != "healthy" {
		t.Fatalf("got %s (checks %v)", e.Verdict, checks)
	}
}

func TestPolicyPerKind(t *testing.T) {
	if p := policyFor("H"); p.KillConsecutive != 6 || p.MinTickGap != time.Hour {
		t.Fatalf("H policy wrong: %+v", p)
	}
	if p := policyFor("S"); p.KillConsecutive != 3 || p.MinTickGap != 0 {
		t.Fatalf("S policy wrong: %+v", p)
	}
}

func TestProjectFlipPer1h(t *testing.T) {
	// entry 900, exit 1000: slipSell(1000)=995, slipBuy(900)=904,
	// tax(995)=19 -> perUnit 72; vol30m 4000 -> 15% of 32000 = 4800 caps
	// nothing below it, units 1000 -> 72*1000/4 = 18000 per hour.
	got := ProjectFlipPer1h(900, 1000, 1000, 4000)
	if got == nil || *got != 18000 {
		t.Fatalf("ProjectFlipPer1h(900,1000,1000,4000) = %v, want 18000", got)
	}
	// Participation cap binds: units 10000 vs 4800 fillable -> 72*4800/4.
	got = ProjectFlipPer1h(900, 1000, 10000, 4000)
	if got == nil || *got != 72*4800/4 {
		t.Fatalf("participation-capped projection = %v, want %d", got, 72*4800/4)
	}
	// Degenerate inputs fall back to nil (the agent's claim stays the
	// denominator rather than storing a nonsense projection).
	if ProjectFlipPer1h(1000, 900, 1000, 4000) != nil {
		t.Error("inverted spread should project nil")
	}
	if ProjectFlipPer1h(900, 1000, 0, 4000) != nil {
		t.Error("zero units should project nil")
	}
	if ProjectFlipPer1h(900, 905, 1000, 4000) != nil {
		t.Error("spread eaten by slippage+tax should project nil")
	}
}

func TestComboKillBarIsAbsolute(t *testing.T) {
	// 1000 conversions: half the 200k floor is 100 gp per conversion. The
	// sell leg stays at 3x(9500-190) = 27930; the buy leg sets the margin.
	priced := func(buy int64) *fakeSource {
		return &fakeSource{snaps: map[int]*Snap{
			139:  {Low: i64(buy), High: i64(buy + 100), LowAgeS: iptr(120), HighAgeS: iptr(120), Vol30m: 9000, Vol4h: 72000, HighTicks24h: 288, LowTicks24h: 288},
			2434: {Low: i64(9300), High: i64(9500), LowAgeS: iptr(90), HighAgeS: iptr(90), Vol30m: 6000, Vol4h: 48000, HighTicks24h: 288, LowTicks24h: 288},
		}}
	}
	cases := []struct {
		name string
		buy  int64 // 4 x buy on the input side
		kill bool
	}{
		{"102/conversion clears the bar", 6957, false}, // 27930-27828 = 102
		{"98/conversion is under it", 6958, true},      // 27930-27832 = 98
		{"a negative margin is under it", 7200, true},  // 27930-28800 = -870
	}
	for _, c := range cases {
		// The calibration factor no longer moves the bar, in either direction.
		for _, factors := range []map[string]float64{nil, {"C": 0.2}, {"C": 1.2}} {
			ev := fixedEvaluator(priced(c.buy), tue1200)
			ev.CalFactors = factors
			e, _, err := ev.computeCombo(context.Background(), comboStrategy())
			if err != nil {
				t.Fatal(err)
			}
			if got := e.Verdict == "kill_signal"; got != c.kill {
				t.Errorf("%s (factors %v): verdict %s", c.name, factors, e.Verdict)
			}
		}
	}

	// No recorded size: only a non-positive margin fails.
	st := comboStrategy()
	st.UnitsUsed = nil
	ev := fixedEvaluator(priced(6958), tue1200)
	if e, _, err := ev.computeCombo(context.Background(), st); err != nil {
		t.Fatal(err)
	} else if e.Verdict == "kill_signal" {
		t.Error("a positive margin with no recorded size must not kill")
	}
	ev = fixedEvaluator(priced(7200), tue1200)
	if e, _, err := ev.computeCombo(context.Background(), st); err != nil {
		t.Fatal(err)
	} else if e.Verdict != "kill_signal" {
		t.Errorf("a negative margin with no recorded size: %s", e.Verdict)
	}
}

func TestCalFactorFailOpen(t *testing.T) {
	ev := &Evaluator{} // no Store, no overrides
	if f := ev.calFactor(context.Background(), "F"); f != 1 {
		t.Errorf("no store: factor = %v, want 1", f)
	}
	ev.CalFactors = map[string]float64{"F": 0.21, "B": -3}
	if f := ev.calFactor(context.Background(), "F"); f != 0.21 {
		t.Errorf("override: factor = %v, want 0.21", f)
	}
	if f := ev.calFactor(context.Background(), "B"); f != 1 {
		t.Errorf("degenerate override: factor = %v, want 1", f)
	}
	if f := ev.calFactor(context.Background(), "C"); f != 1 {
		t.Errorf("unmeasured archetype: factor = %v, want 1", f)
	}
}

// A thin leg that didn't print in the last 30 minutes still has a real 4h volume:
// the conversion earns, and the volume check passes.
func TestComboThinLegCountsItsFourHourVolume(t *testing.T) {
	src := &fakeSource{snaps: map[int]*Snap{
		139:  {Low: i64(6900), High: i64(7000), LowAgeS: iptr(7200), HighAgeS: iptr(7200), Vol30m: 0, Vol4h: 30000, HighTicks24h: 30, LowTicks24h: 30},
		2434: {Low: i64(9300), High: i64(9500), LowAgeS: iptr(90), HighAgeS: iptr(90), Vol30m: 6000, Vol4h: 48000, HighTicks24h: 288, LowTicks24h: 288},
	}}
	e, checks, err := fixedEvaluator(src, tue1200).computeCombo(context.Background(), comboStrategy())
	if err != nil {
		t.Fatal(err)
	}
	if !checks["vol_ok"] {
		t.Errorf("30000 traded in 4h covers 4 x 1000 conversions: %v", checks)
	}
	if !checks["legs_fresh"] {
		t.Errorf("a 2h quote on a leg with a 48m gap is its normal cadence: %v", checks)
	}
	if e.Verdict != "healthy" {
		t.Errorf("verdict %s", e.Verdict)
	}
	if e.RealizedPer1hGp == nil || *e.RealizedPer1hGp == 0 {
		t.Fatalf("a positive margin on a leg with 4h volume must not realize 0: %v", e.RealizedPer1hGp)
	}
}

func TestComboLegConversionsUseFourHourVolume(t *testing.T) {
	ev := &Evaluator{}
	// 15% of 400 is 60 units, 15 conversions at qty 4.
	if got := ev.legConversions(&Snap{Vol30m: 0, Vol4h: 400}, 4); got != 15 {
		t.Errorf("conversions %d", got)
	}
	if got := ev.legConversions(&Snap{Vol30m: 1000, Vol4h: 0}, 1); got != 0 {
		t.Errorf("no 4h volume, no conversions: %d", got)
	}
}

func TestLegFreshUsesTheLegsOwnCadence(t *testing.T) {
	cases := []struct {
		name   string
		age    *int
		trades int64
		want   bool
	}{
		{"active leg at 15 min", iptr(900), 288, true},
		{"active leg past 15 min", iptr(901), 288, false},
		{"thin leg at 2h, 48m gap", iptr(7200), 30, true},
		{"thin leg at 2h, 12m gap", iptr(7200), 120, false},
		{"no trades in 24h", iptr(60), 0, false},
		{"never traded", nil, 40, false},
	}
	for _, c := range cases {
		if got := legFresh(c.age, c.trades); got != c.want {
			t.Errorf("%s: fresh=%v", c.name, got)
		}
	}
}
