package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestProvenanceAllocationRegime walks one run through provenance, the signal
// link, the allocation compiler and the regime snapshot. Needs a migrated
// scratch DB: set STORE_TEST_DSN; skipped otherwise.
func TestProvenanceAllocationRegime(t *testing.T) {
	dsn := os.Getenv("STORE_TEST_DSN")
	if dsn == "" {
		t.Skip("STORE_TEST_DSN not set")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool.Close()
	if _, err := s.OrphanRunningRuns(ctx); err != nil {
		t.Fatal(err)
	}
	const item, screenItem, freeItem, deepItem = 876541, 876542, 876543, 876545
	cleanup := func() {
		s.Pool.Exec(ctx, `DELETE FROM orchestrator.signals WHERE item_id = ANY($1)`, []int{item, screenItem, freeItem, deepItem})
		s.Pool.Exec(ctx, `DELETE FROM orchestrator.trend_snapshots WHERE item_id = ANY($1)`, []int{item, screenItem, freeItem, deepItem})
		s.Pool.Exec(ctx, `DELETE FROM orchestrator.strategies WHERE primary_item_id = ANY($1)`, []int{item, screenItem, freeItem, deepItem})
		s.Pool.Exec(ctx, `DELETE FROM orchestrator.lane_allocation`)
		s.Pool.Exec(ctx, `DELETE FROM orchestrator.regime_snapshots`)
	}
	cleanup()
	defer cleanup()
	defer s.OrphanRunningRuns(ctx)

	// The collector's sweep and queue, as a run would find them.
	sweep := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	if err := s.InsertTrendSnapshots(ctx, sweep, "vflip", []TrendRow{
		{AsOf: sweep, Lens: "vflip", ItemID: screenItem, Rank: 1, Metrics: json.RawMessage(`{"gp_cycle":900000,"margin":90}`)},
		{AsOf: sweep, Lens: "vflip", ItemID: freeItem, Rank: 2, Metrics: json.RawMessage(`{"gp_cycle":300000,"margin":30}`)},
		{AsOf: sweep, Lens: "vflip", ItemID: deepItem, Rank: 6, Metrics: json.RawMessage(`{"gp_cycle":250000,"margin":25}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTrendSnapshots(ctx, sweep, "volume", []TrendRow{
		{AsOf: sweep, Lens: "volume", ItemID: freeItem, Rank: 1, Metrics: json.RawMessage(`{"z":-12.5}`)},
	}); err != nil {
		t.Fatal(err)
	}
	// Ranked rows come back in rank order, not item order.
	latest, err := s.LatestTrends(ctx, "vflip", 5)
	if err != nil || len(latest) != 3 || latest[0].ItemID != screenItem {
		t.Fatalf("latest %+v: %v", latest, err)
	}
	if _, err := s.UpsertSignal(ctx, "vflip", item, "Signal item", map[string]any{"gp_cycle": 900000}, 0); err != nil {
		t.Fatal(err)
	}
	runID, err := s.CreateRun(ctx, json.RawMessage(`{}`), "test brief", "manual")
	if err != nil {
		t.Fatal(err)
	}
	queue, err := s.NextSignals(ctx, map[string]float64{"F": 1}, 500)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, sig := range queue {
		if sig.ItemID == item {
			ids = append(ids, sig.SignalID)
		}
	}
	if err := s.AssignSignals(ctx, runID, ids); err != nil {
		t.Fatal(err)
	}

	sc := func(sid string, itemID int) SidecarStrategy {
		var st SidecarStrategy
		raw := fmt.Sprintf(`{"id":%q,"archetype":"F","title":"t","thesis":"t",
			"items":[{"name":"x","id":%d}],"entry":"e","exit":"x","entry_price":1,"exit_price":2,
			"horizon":"h","capital_required":1,"size":{"buy_limit":1,"vol_constrained":1,"units_used":1},
			"expected_value":{"per_cycle_gp":1,"per_1h_gp":1,"per_day_gp":1,"roi_pct":1},
			"confidence":"medium","invalidation":"i"}`, sid, itemID)
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	list := []SidecarStrategy{sc("F-signal", item), sc("F-screen", screenItem), sc("F-free", 876544), sc("F-deep", deepItem)}
	if err := s.AttachProvenance(ctx, runID, time.Now().UTC(), list, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertStrategies(ctx, runID, time.Now().UTC(), list, nil); err != nil {
		t.Fatal(err)
	}
	defer s.Pool.Exec(ctx, `DELETE FROM orchestrator.strategies WHERE run_id=$1`, runID)
	if err := s.LinkSignalStrategies(ctx, runID); err != nil {
		t.Fatal(err)
	}
	got := map[string]struct {
		source string
		hits   []string
	}{}
	rows, err := s.Pool.Query(ctx, `SELECT sid, source_lens, screen_hits FROM orchestrator.strategies WHERE run_id=$1`, runID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var sid, source string
		var hits []string
		if err := rows.Scan(&sid, &source, &hits); err != nil {
			t.Fatal(err)
		}
		got[sid] = struct {
			source string
			hits   []string
		}{source, hits}
	}
	rows.Close()
	if got["F-signal"].source != "vflip" || got["F-screen"].source != "sweep:vflip" || got["F-free"].source != "free" {
		t.Fatalf("sources %+v", got)
	}
	if len(got["F-screen"].hits) != 1 || got["F-screen"].hits[0] != "vflip" {
		t.Errorf("screen hits %+v", got["F-screen"])
	}
	// Rank 6 is below what the brief showed: no credit, but the hit is kept.
	if got["F-deep"].source != "free" || len(got["F-deep"].hits) != 1 {
		t.Errorf("a row the brief never showed: %+v", got["F-deep"])
	}
	var linked int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM orchestrator.signals sig
		JOIN orchestrator.strategies st ON st.strategy_id = sig.strategy_id
		WHERE sig.signal_id = ANY($1) AND st.sid='F-signal'`, ids).Scan(&linked); err != nil || linked != 1 {
		t.Fatalf("signal link: %d %v", linked, err)
	}

	// Close one, then compile.
	if _, err := s.Pool.Exec(ctx, `UPDATE orchestrator.strategies SET state='killed', closed_at=now(),
		opened_at=now() - interval '7 hours' WHERE run_id=$1 AND sid='F-signal'`, runID); err != nil {
		t.Fatal(err)
	}
	base := map[string]float64{"C": 1.5, "F": 0.5, "B": 0}
	first, err := s.CompileAllocation(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if first.Mode != "log" || first.Weights["F"] != 0.5 || first.Weights["B"] != 0 {
		t.Errorf("first row %+v", first)
	}
	var f *LaneYield
	for i := range first.Lanes {
		if first.Lanes[i].Lane == "F" {
			f = &first.Lanes[i]
		}
	}
	if f == nil || f.Pitched < 4 || f.Closed < 1 || f.Survived < 1 || f.Target != nil {
		t.Errorf("F yield %+v (the sample gate holds its target back)", f)
	}
	if first.Families == nil {
		t.Error("families must be an empty list, not null")
	}
	var sawSource bool
	for _, src := range first.Sources {
		if src.Lane == "F" && src.Source == "vflip" && src.Closed >= 1 {
			sawSource = true
		}
	}
	if !sawSource {
		t.Errorf("sources %+v", first.Sources)
	}
	latestAlloc, err := s.LatestAllocation(ctx)
	if err != nil || latestAlloc == nil || latestAlloc.AllocationID != first.AllocationID || latestAlloc.Weights["C"] != 1.5 {
		t.Fatalf("latest %+v: %v", latestAlloc, err)
	}
	if hist, err := s.AllocationHistory(ctx, 1); err != nil || len(hist) != 1 {
		t.Fatalf("history %+v: %v", hist, err)
	}

	r, err := s.RecordRegime(ctx, sweep, 400_000, 200_000, 8)
	if err != nil {
		t.Fatal(err)
	}
	if r.FlipClearFloor == nil || *r.FlipClearFloor != 1 || r.FlipMedianGp == nil || *r.FlipMedianGp != 300000 {
		t.Errorf("flip regime %+v", r)
	}
	if r.VolumeAnomalies == nil || *r.VolumeAnomalies != 1 {
		t.Errorf("a negative z is an anomaly too: %+v", r)
	}
	if r.ComboClearFloor != nil || r.ComboMedianGp != nil {
		t.Errorf("no combo rows must read as null, not zero: %+v", r)
	}
	if hist, err := s.RegimeHistory(ctx, 1); err != nil || len(hist) != 1 {
		t.Fatalf("regime history %+v: %v", hist, err)
	}
}

// TestAllocationFamilies checks C's family yields, including a recipe ge-data hasn't
// labelled. Needs a scratch DB with ge-data's init/07 applied: set STORE_TEST_DSN.
func TestAllocationFamilies(t *testing.T) {
	dsn := os.Getenv("STORE_TEST_DSN")
	if dsn == "" {
		t.Skip("STORE_TEST_DSN not set")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool.Close()
	if _, err := s.OrphanRunningRuns(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.OrphanRunningRuns(ctx)
	runID, err := s.CreateRun(ctx, json.RawMessage(`{}`), "test brief", "manual")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool.Exec(ctx, `DELETE FROM orchestrator.lane_allocation`)
	defer s.Pool.Exec(ctx, `DELETE FROM orchestrator.strategies WHERE run_id=$1`, runID)
	var relID int
	var family string
	if err := s.Pool.QueryRow(ctx, `SELECT relation_id, family FROM public.item_relations WHERE family='moon' LIMIT 1`).Scan(&relID, &family); err != nil {
		t.Skipf("no labelled recipes in this DB: %v", err)
	}
	var st SidecarStrategy
	raw := fmt.Sprintf(`{"id":"C-family-test","archetype":"C","title":"t","thesis":"t",
		"items":[{"name":"x","id":876590}],"entry":"e","exit":"x","entry_price":1,"exit_price":2,
		"horizon":"h","capital_required":1,"size":{"buy_limit":1,"vol_constrained":1,"units_used":1},
		"expected_value":{"per_cycle_gp":1,"per_1h_gp":1,"per_day_gp":1,"roi_pct":1},
		"confidence":"medium","invalidation":"i",
		"legs":[{"item_id":876590,"name":"x","side":"sell","qty":1,"price":2}],"relation_id":%d}`, relID)
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertStrategies(ctx, runID, time.Now().UTC().Add(-7*time.Hour), []SidecarStrategy{st}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE orchestrator.strategies SET state='killed', closed_at=now() WHERE run_id=$1`, runID); err != nil {
		t.Fatal(err)
	}
	a, err := s.CompileAllocation(ctx, map[string]float64{"C": 1.5})
	if err != nil {
		t.Fatal(err)
	}
	var got *FamilyYield
	for i := range a.Families {
		if a.Families[i].Family == family {
			got = &a.Families[i]
		}
	}
	if got == nil || got.Kind != "set" || got.Closed < 1 || got.Survived < 1 || got.PerShipGp == nil {
		t.Fatalf("families %+v", a.Families)
	}
	latest, err := s.LatestAllocation(ctx)
	if err != nil || latest == nil || len(latest.Families) != len(a.Families) {
		t.Fatalf("families must round-trip: %+v %v", latest, err)
	}
}

func TestCalibrationLatestDropsStaleFamilyRows(t *testing.T) {
	dsn := os.Getenv("STORE_TEST_DSN")
	if dsn == "" {
		t.Skip("STORE_TEST_DSN not set")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool.Close()
	if err := s.Preflight(ctx); err != nil {
		t.Fatalf("preflight on a current schema: %v", err)
	}
	// family, fresh family, and a kind row: only the family row ages out
	for _, row := range []struct {
		key  string
		days int
	}{{"C:teststale", CalWindowDays + 1}, {"C:testfresh", 1}, {"C:set", CalWindowDays + 1}} {
		if _, err := s.Pool.Exec(ctx, `INSERT INTO orchestrator.calibration
			(computed_at, archetype, window_days, survive_hours, n_closed, n_survived, n_pace, factor, epoch)
			VALUES (now() - make_interval(days => $1), $2, 14, 6, 10, 5, 0, 0.5, 1)`, row.days, row.key); err != nil {
			t.Fatal(err)
		}
	}
	defer s.Pool.Exec(ctx, `DELETE FROM orchestrator.calibration
		WHERE archetype IN ('C:teststale','C:testfresh') OR (archetype = 'C:set' AND factor = 0.5 AND n_closed = 10)`)
	keys := func() map[string]bool {
		rows, err := s.CalibrationLatest(ctx)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, r := range rows {
			seen[r.Archetype] = true
		}
		return seen
	}
	if seen := keys(); seen["C:teststale"] || !seen["C:testfresh"] || !seen["C:set"] {
		t.Errorf("latest keys %v: want the fresh family and the old kind row, not the stale family", seen)
	}
	// After a cut, a fresh family row from the old epoch must not outrank the new
	// regime's kind row. Those rows are at epoch 1, so epoch 2 is the cut here.
	s.Epochs = map[string]int{"C": 2}
	if seen := keys(); seen["C:testfresh"] {
		t.Errorf("latest keys %v: a family row from before the cut must not survive it", seen)
	}
}
