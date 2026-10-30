package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestRecomputeCalibrationByRecipeKind checks that a CalKey row counts only its
// recipe kind's closes, next to the lane's blended row. Needs a migrated scratch
// DB with public.item_relations seeded: set STORE_TEST_DSN; skipped otherwise.
func TestRecomputeCalibrationByRecipeKind(t *testing.T) {
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

	if _, err := s.OrphanRunningRuns(ctx); err != nil { // one running run at a time, in-DB
		t.Fatal(err)
	}
	relation := func(kind string) int {
		t.Helper()
		var id int
		if err := s.Pool.QueryRow(ctx, `SELECT relation_id FROM public.item_relations
			WHERE kind=$1 ORDER BY relation_id LIMIT 1`, kind).Scan(&id); err != nil {
			t.Fatalf("no %s relation seeded: %v", kind, err)
		}
		return id
	}
	runID, err := s.CreateRun(ctx, json.RawMessage(`{}`), "test brief", "manual")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool.Exec(ctx, `DELETE FROM orchestrator.strategies WHERE run_id=$1`, runID)
	defer s.Pool.Exec(ctx, `DELETE FROM orchestrator.calibration WHERE archetype LIKE 'C%'`)
	defer s.OrphanRunningRuns(ctx)

	// Three sets (two live past 6h, one dies at 1h) and one decant.
	ship := func(n int, relID int, liveHours int) {
		t.Helper()
		sid := fmt.Sprintf("C-kind-test-%d", n)
		var st SidecarStrategy
		raw := fmt.Sprintf(`{"id":%q,"archetype":"C","title":"t","thesis":"t",
			"items":[{"name":"x","id":%d}],"entry":"e","exit":"x","entry_price":1,"exit_price":2,
			"horizon":"h","capital_required":1,"size":{"buy_limit":1,"vol_constrained":1,"units_used":1},
			"expected_value":{"per_cycle_gp":1,"per_1h_gp":1,"per_day_gp":1,"roi_pct":1},
			"confidence":"medium","invalidation":"i",
			"legs":[{"item_id":%d,"name":"x","side":"sell","qty":1,"price":2}],"relation_id":%d}`,
			sid, 800000+n, 800000+n, relID)
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			t.Fatal(err)
		}
		opened := time.Now().UTC().Add(-time.Duration(liveHours) * time.Hour)
		if err := s.InsertStrategies(ctx, runID, opened, []SidecarStrategy{st}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec(ctx, `UPDATE orchestrator.strategies
			SET state='killed', closed_at=now() WHERE run_id=$1 AND sid=$2`, runID, sid); err != nil {
			t.Fatal(err)
		}
	}
	set, decant := relation("set"), relation("decant")
	ship(1, set, 10)
	ship(2, set, 8)
	ship(3, set, 1)
	ship(4, decant, 1)

	check := func(key string, closed, survived int) {
		t.Helper()
		row, err := s.RecomputeCalibration(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if row.Archetype != key || row.NClosed != closed || row.NSurvived != survived {
			t.Errorf("%s: %+v, want closed %d survived %d", key, row, closed, survived)
		}
	}
	check("C", 4, 2)
	check("C:set", 3, 2)
	check("C:decant", 1, 0)
	check("C:combine", 0, 0)

	latest, err := s.CalibrationLatest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range latest {
		seen[r.Archetype] = true
	}
	for _, key := range []string{"C", "C:set", "C:decant", "C:combine"} {
		if !seen[key] {
			t.Errorf("CalibrationLatest is missing %s: %v", key, seen)
		}
	}
}

// TestScoreboardRatioReadsTheCurrentEpoch: an epoch cut hides the old ratio until the
// new epoch has a sample, without touching the all-time counts. Needs STORE_TEST_DSN.
func TestScoreboardRatioReadsTheCurrentEpoch(t *testing.T) {
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
	defer s.Pool.Exec(ctx, `DELETE FROM orchestrator.strategies WHERE run_id=$1`, runID)
	defer s.Pool.Exec(ctx, `DELETE FROM orchestrator.evaluations WHERE strategy_id IN (SELECT strategy_id FROM orchestrator.strategies WHERE run_id=$1)`, runID)

	// ship closes one U strategy pacing at the given share of its 1000 gp/hr plan.
	n := 0
	ship := func(share int64) {
		t.Helper()
		n++
		sid := fmt.Sprintf("U-ratio-test-%d", n)
		var st SidecarStrategy
		raw := fmt.Sprintf(`{"id":%q,"archetype":"U","title":"t","thesis":"t",
			"items":[{"name":"x","id":%d}],"entry":"e","exit":"x","entry_price":1,"exit_price":2,
			"horizon":"h","capital_required":1,"size":{"buy_limit":1,"vol_constrained":1,"units_used":1},
			"expected_value":{"per_cycle_gp":1,"per_1h_gp":1000,"per_day_gp":1,"roi_pct":1},
			"confidence":"medium","invalidation":"i"}`, sid, 870000+n)
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertStrategies(ctx, runID, time.Now().UTC().Add(-time.Hour), []SidecarStrategy{st}, nil); err != nil {
			t.Fatal(err)
		}
		var id int64
		if err := s.Pool.QueryRow(ctx, `UPDATE orchestrator.strategies SET state='killed', closed_at=now()
			WHERE run_id=$1 AND sid=$2 RETURNING strategy_id`, runID, sid).Scan(&id); err != nil {
			t.Fatal(err)
		}
		realized := share * 10
		if err := s.InsertEvaluation(ctx, Evaluation{StrategyID: id, At: time.Now().UTC(), Verdict: "healthy",
			Checks: json.RawMessage(`{}`), Detail: json.RawMessage(`{}`), RealizedPer1hGp: &realized}); err != nil {
			t.Fatal(err)
		}
	}
	ratio := func() (*float64, int) {
		t.Helper()
		rows, err := s.Scoreboard(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.Archetype == "U" {
				return r.RealizedVsProjected, r.N
			}
		}
		t.Fatal("no U row")
		return nil, 0
	}

	for i := 0; i < scoreboardMinCloses; i++ {
		ship(20) // the old epoch paces at 0.20
	}
	if got, _ := ratio(); got == nil || *got != 0.2 {
		t.Fatalf("epoch 1 ratio %v", got)
	}
	s.Epochs = map[string]int{"U": 2}
	if got, n := ratio(); got != nil || n != scoreboardMinCloses {
		t.Fatalf("a cut hides the ratio, not the counts: %v n=%d", got, n)
	}
	for i := 0; i < scoreboardMinCloses-1; i++ {
		ship(90)
	}
	if got, _ := ratio(); got != nil {
		t.Fatalf("under the sample, no ratio: %v", *got)
	}
	ship(90)
	if got, n := ratio(); got == nil || *got != 0.9 || n != 2*scoreboardMinCloses {
		t.Fatalf("the new epoch's ratio alone: %v n=%d", got, n)
	}
}
