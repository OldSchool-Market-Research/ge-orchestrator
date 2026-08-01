package runner

import (
	"testing"

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
