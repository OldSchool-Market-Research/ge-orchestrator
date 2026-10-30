package eval

import "testing"

func TestProcessUnitsPer4h(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	const budget = 50_000_000
	cases := []struct {
		name string
		snap *ProcessSnap
		want int64
	}{
		// 15% of the market is 1,500/hr, so the player's 900/hr binds.
		{"player speed", &ProcessSnap{GrossIn: f(100), MarketRate: f(10_000), ActionsPerHour: f(900), LimitRate: f(5_000)}, 3_600},
		{"buy limit", &ProcessSnap{GrossIn: f(100), MarketRate: f(10_000), ActionsPerHour: f(900), LimitRate: f(300)}, 1_200},
		{"market cap", &ProcessSnap{GrossIn: f(100), MarketRate: f(1_000), ActionsPerHour: f(900), LimitRate: f(5_000)}, 600},
		// 50M buys 500 actions of a 100k recipe per cycle, under every rate here.
		{"budget", &ProcessSnap{GrossIn: f(100_000), MarketRate: f(10_000), ActionsPerHour: f(900), LimitRate: f(5_000)}, 500},
		{"volume-bound recipe has no player rate", &ProcessSnap{GrossIn: f(100), MarketRate: f(1_000)}, 600},
		{"whole actions only", &ProcessSnap{GrossIn: f(100), MarketRate: f(1_000), ActionsPerHour: f(0.3)}, 1},
		{"nil snap", nil, 0},
		{"no input price", &ProcessSnap{MarketRate: f(1_000)}, 0},
		{"free input", &ProcessSnap{GrossIn: f(0), MarketRate: f(1_000)}, 0},
		{"no market", &ProcessSnap{GrossIn: f(100)}, 0},
	}
	for _, c := range cases {
		if got := ProcessUnitsPer4h(c.snap, budget); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// The size a strategy copies must project to the same number whether or not it's
// passed: at its own size, a recipe's projection is its honest rate.
func TestProcessUnitsMatchProjection(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	s := &ProcessSnap{MarginSlow: f(300), GrossIn: f(1_500), GrossOut: f(1_800),
		ActionsPerHour: f(1_440), LimitRate: f(2_750), MarketRate: f(160_000)}
	units := ProcessUnitsPer4h(s, 50_000_000)
	if units != 5_760 {
		t.Fatalf("units %d", units)
	}
	got := ProjectProcessPer1h(s, units)
	if got == nil {
		t.Fatal("no projection")
	}
	// margin 300 - 0.5% of (1,500 + 1,800) = 283.5, at 1,440 actions/hr
	if *got != 408_240 {
		t.Errorf("projection %d", *got)
	}
}
