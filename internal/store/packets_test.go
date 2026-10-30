package store

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestComparePacket(t *testing.T) {
	id := func(v int) *int { return &v }
	reason := "vetoed at ship time: item 9 already has an open P strategy"
	rows := []PacketRow{
		{RecipeID: 1, Ship: true},
		{RecipeID: 2, Ship: true},
		{RecipeID: 3, Why: "under the floor"},
		{RecipeID: 4, Why: "stale leg"},
	}
	picks := []PacketPick{
		{RecipeID: id(1), Sid: "P-both", State: "open"},
		{RecipeID: id(3), Sid: "P-model", State: "killed"},
		{RecipeID: id(77), Sid: "P-unknown", State: "open"},
		{RecipeID: id(4), Sid: "P-vetoed", State: "vetoed", VetoReason: &reason},
		{RecipeID: nil, Sid: "P-no-recipe", State: "vetoed"},
	}
	var run PacketRun
	comparePacket(&run, rows, picks)
	if run.Judged != 4 || len(run.Script) != 2 {
		t.Errorf("judged %d, script %d", run.Judged, len(run.Script))
	}
	if !reflect.DeepEqual(run.Both, []int{1}) || !reflect.DeepEqual(run.ScriptOnly, []int{2}) {
		t.Errorf("both %v, script only %v", run.Both, run.ScriptOnly)
	}
	if len(run.ModelOnly) != 2 || run.ModelOnly[0].ScriptWhy != "under the floor" || run.ModelOnly[1].ScriptWhy != "not in the packet" {
		t.Errorf("model only %+v", run.ModelOnly)
	}
	if len(run.Vetoed) != 2 || run.Vetoed[0].ScriptWhy != "stale leg" {
		t.Errorf("vetoed %+v", run.Vetoed)
	}
}

// A vetoed pitch of a recipe the script would ship isn't agreement: the model's
// strategy never entered the book.
func TestComparePacketVetoedPickIsNotBoth(t *testing.T) {
	id := 1
	var run PacketRun
	comparePacket(&run, []PacketRow{{RecipeID: 1, Ship: true}}, []PacketPick{{RecipeID: &id, Sid: "P-x", State: "vetoed"}})
	if len(run.Both) != 0 || !reflect.DeepEqual(run.ScriptOnly, []int{1}) || len(run.Vetoed) != 1 {
		t.Errorf("both %v, script only %v, vetoed %v", run.Both, run.ScriptOnly, run.Vetoed)
	}
}

// No pitches still serializes as empty lists, not nulls.
func TestComparePacketEmpty(t *testing.T) {
	var run PacketRun
	comparePacket(&run, nil, nil)
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"script", "both", "script_only", "model_only", "model_vetoed"} {
		if _, ok := got[k].([]any); !ok {
			t.Errorf("%s = %v, want an empty list", k, got[k])
		}
	}
}

func TestSummarizePacketsCountsSucceededRunsOnly(t *testing.T) {
	ok := PacketRun{RunStatus: "succeeded", Script: []PacketRow{{RecipeID: 1}, {RecipeID: 2}}, Both: []int{1}, ScriptOnly: []int{2},
		ModelOnly: []PacketPick{{Sid: "a"}}, Vetoed: []PacketPick{{Sid: "b"}}}
	failed := PacketRun{RunStatus: "failed", Script: []PacketRow{{RecipeID: 3}}, ScriptOnly: []int{3}}
	got := summarizePackets([]PacketRun{ok, failed})
	want := PacketSummary{Runs: 1, Script: 2, Model: 2, Both: 1, ScriptOnly: 1, ModelOnly: 1, Vetoed: 1}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestLanePacketRoundTrip stores a packet and reads it against a run's real pitches.
// Needs a migrated scratch DB: set STORE_TEST_DSN; skipped otherwise.
func TestLanePacketRoundTrip(t *testing.T) {
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
	const item = 876561
	runID, err := s.CreateRun(ctx, json.RawMessage(`{}`), "test brief", "manual")
	if err != nil {
		t.Fatal(err)
	}
	defer s.OrphanRunningRuns(ctx)
	defer s.Pool.Exec(ctx, `DELETE FROM orchestrator.lane_packets WHERE run_id=$1`, runID)
	defer s.Pool.Exec(ctx, `DELETE FROM orchestrator.strategies WHERE run_id=$1`, runID)

	gp := int64(400_000)
	if err := s.InsertLanePacket(ctx, runID, []PacketRow{
		{RecipeID: 26, Name: "Mahogany Plank", OutputItem: intPtr(8782), GpHr: &gp, UnitsPer4h: 5760, Hours24h: 24, HoursAtFloor24h: 24, Ship: true},
		{RecipeID: 31, Name: "Thin one", Why: "under the floor"},
	}); err != nil {
		t.Fatal(err)
	}
	var st SidecarStrategy
	raw := `{"id":"P-packet-test","archetype":"P","recipe_id":31,"title":"t","thesis":"t",
		"items":[{"name":"x","id":876561}],"entry":"e","exit":"x","entry_price":1,"exit_price":2,
		"horizon":"h","capital_required":1,"size":{"buy_limit":1,"vol_constrained":1,"units_used":1},
		"expected_value":{"per_cycle_gp":1,"per_1h_gp":1,"per_day_gp":1,"roi_pct":1},
		"confidence":"medium","invalidation":"i"}`
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	if st.PrimaryItemID() != item {
		t.Fatalf("primary item %d", st.PrimaryItemID())
	}
	if err := s.InsertStrategies(ctx, runID, time.Now().UTC(), []SidecarStrategy{st}, nil); err != nil {
		t.Fatal(err)
	}

	runs, _, err := s.PacketRuns(ctx, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs", len(runs))
	}
	r := runs[0]
	if r.RunID != runID || r.Judged != 2 || len(r.Script) != 1 || r.Script[0].GpHr == nil || *r.Script[0].GpHr != gp {
		t.Errorf("run %+v", r)
	}
	if !reflect.DeepEqual(r.ScriptOnly, []int{26}) || len(r.ModelOnly) != 1 || r.ModelOnly[0].ScriptWhy != "under the floor" {
		t.Errorf("script only %v, model only %+v", r.ScriptOnly, r.ModelOnly)
	}
	// One packet a run: the primary key holds it.
	if err := s.InsertLanePacket(ctx, runID, nil); err == nil {
		t.Error("second packet for a run was accepted")
	}
}

func intPtr(v int) *int { return &v }
