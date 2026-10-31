package store

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func queued(kinds ...string) []Signal {
	out := make([]Signal, len(kinds))
	for i, k := range kinds {
		out[i] = Signal{SignalID: int64(i + 1), Kind: k}
	}
	return out
}

func pickedKinds(sigs []Signal) string {
	kinds := make([]string, len(sigs))
	for i, s := range sigs {
		kinds[i] = s.Kind
	}
	return strings.Join(kinds, " ")
}

func TestPickSignalsSharesByLaneWeight(t *testing.T) {
	weights := map[string]float64{"C": 1.5, "F": 0.5, "B": 0, "V": 0.5, "U": 0.5}
	var pending []Signal
	// The noisy lens is first in the queue and by far the largest.
	for i := 0; i < 30; i++ {
		pending = append(pending, Signal{SignalID: int64(i), Kind: "volume"})
	}
	for i := 0; i < 10; i++ {
		pending = append(pending, Signal{SignalID: int64(100 + i), Kind: "hvflip"})
	}
	for i := 0; i < 10; i++ {
		pending = append(pending, Signal{SignalID: int64(200 + i), Kind: "combo"})
		pending = append(pending, Signal{SignalID: int64(300 + i), Kind: "vflip"})
	}
	got := pickSignals(pending, weights, 10)
	counts := map[string]int{}
	for _, s := range got {
		counts[s.Kind]++
	}
	if len(got) != 10 || counts["combo"] != 6 || counts["vflip"] != 2 || counts["volume"] != 2 || counts["hvflip"] != 0 {
		t.Fatalf("picked %v", counts)
	}
	// Oldest first within a kind.
	var combos []int64
	for _, s := range got {
		if s.Kind == "combo" {
			combos = append(combos, s.SignalID)
		}
	}
	for i, id := range combos {
		if id != int64(200+i) {
			t.Fatalf("combo order %v", combos)
		}
	}
}

func TestPickSignalsWatchFirstUpToHalf(t *testing.T) {
	weights := map[string]float64{"C": 1.5}
	got := pickSignals(queued("combo", "watch", "watch", "watch", "combo", "combo", "combo"), weights, 4)
	if s := pickedKinds(got); s != "watch watch combo combo" {
		t.Fatalf("picked %q", s)
	}
}

func TestPickSignalsDrainsThinKinds(t *testing.T) {
	weights := map[string]float64{"C": 1.5, "V": 0.5}
	// Fewer signals than slots: everything assignable goes out, nothing spins.
	got := pickSignals(queued("volume", "combo"), weights, 10)
	if len(got) != 2 {
		t.Fatalf("picked %q", pickedKinds(got))
	}
	if got := pickSignals(nil, weights, 10); len(got) != 0 {
		t.Fatalf("empty queue: %v", got)
	}
	// Every lane off: nothing to assign, and the loop still ends.
	if got := pickSignals(queued("hvflip"), map[string]float64{"B": 0}, 10); len(got) != 0 {
		t.Fatalf("off lane: %v", got)
	}
}

func TestLaneOff(t *testing.T) {
	weights := map[string]float64{"B": 0, "F": 0.5}
	if !LaneOff("hvflip", weights) {
		t.Error("B is off")
	}
	if LaneOff("vflip", weights) {
		t.Error("F is on")
	}
	if LaneOff("combo", weights) || LaneOff("watch", weights) || LaneOff("combo", nil) {
		t.Error("a lane with no weight stays on")
	}
}

func TestCalKey(t *testing.T) {
	cases := map[[2]string]string{
		{"C", "set"}: "C:set",
		{"C", ""}:    "C",
		{"F", "set"}: "F",
	}
	for in, want := range cases {
		if got := CalKey(in[0], in[1]); got != want {
			t.Errorf("CalKey(%v) = %q, want %q", in, got, want)
		}
	}
}

// TestUpsertSignalGuards runs the queue's dismissal memory against a real
// database. Needs a migrated scratch DB: set STORE_TEST_DSN; skipped otherwise.
func TestUpsertSignalGuards(t *testing.T) {
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

	const item, cooldown = 987654, 48 * time.Hour
	cleanup := func() {
		s.Pool.Exec(ctx, `DELETE FROM orchestrator.signals WHERE item_id=$1`, item)
	}
	cleanup()
	defer cleanup()

	metrics := func(gp int64) map[string]any { return map[string]any{"gp_cycle": gp} }
	upsert := func(gp int64) bool {
		t.Helper()
		isNew, err := s.UpsertSignal(ctx, "vflip", item, "Test item", metrics(gp), cooldown)
		if err != nil {
			t.Fatal(err)
		}
		return isNew
	}
	// dismiss closes the live signal with a verdict, backdated past the cooldown.
	dismiss := func(reason string, ago time.Duration) {
		t.Helper()
		var id int64
		if err := s.Pool.QueryRow(ctx, `SELECT signal_id FROM orchestrator.signals
			WHERE item_id=$1 AND status IN ('pending','assigned')`, item).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if err := s.ResolveSignal(ctx, id, "dismissed", reason); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec(ctx, `UPDATE orchestrator.signals
			SET resolved_at = now() - $2::interval WHERE signal_id=$1`, id, ago.String()); err != nil {
			t.Fatal(err)
		}
	}

	if !upsert(300_000) {
		t.Fatal("a first detection queues")
	}
	if upsert(310_000) {
		t.Fatal("a live signal only refreshes")
	}
	dismiss("margin is a spike", time.Hour)
	if upsert(300_000) {
		t.Fatal("inside the cooldown nothing re-queues")
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE orchestrator.signals
		SET resolved_at = now() - interval '3 days' WHERE item_id=$1`, item); err != nil {
		t.Fatal(err)
	}
	if !upsert(300_000) {
		t.Fatal("one old dismissal re-queues after the cooldown")
	}
	dismiss("still a spike", 3*24*time.Hour)

	// Two verdicts in 14 days: held out, until the edge doubles.
	if upsert(500_000) {
		t.Fatal("two recent dismissals hold the item out")
	}
	if !upsert(600_000) {
		t.Fatal("twice the last dismissed score (300k) re-opens it")
	}
	dismiss("the bigger margin was a spike too", 3*24*time.Hour)
	if upsert(1_100_000) {
		t.Fatal("the bar moves to the latest dismissal (600k)")
	}

	// Watch revalidation (cooldown 0) ignores both guards.
	if isNew, err := s.UpsertSignal(ctx, "vflip", item, "Test item", metrics(1), 0); err != nil || !isNew {
		t.Fatalf("cooldown 0 bypasses the guards: new=%v err=%v", isNew, err)
	}
	cleanup()

	// A harness close is not a falsification: no cooldown, no count, no obituary.
	if !upsert(300_000) {
		t.Fatal("first detection")
	}
	if n, err := s.Pool.Exec(ctx, `UPDATE orchestrator.signals
		SET created_at = now() - interval '4 days' WHERE item_id=$1`, item); err != nil || n.RowsAffected() != 1 {
		t.Fatalf("backdate: %v", err)
	}
	if n, err := s.ExpireStaleSignals(ctx, 72*time.Hour); err != nil || n < 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	at, reason, err := s.LatestDismissal(ctx, "vflip", item)
	if err != nil || at != nil || reason != nil {
		t.Fatalf("an expiry leaves no obituary: %v %v %v", at, reason, err)
	}
	if !upsert(300_000) {
		t.Fatal("an expired signal re-queues at once")
	}
	var raw json.RawMessage
	if err := s.Pool.QueryRow(ctx, `SELECT metrics FROM orchestrator.signals
		WHERE item_id=$1 AND status='pending'`, item).Scan(&raw); err != nil || !strings.Contains(string(raw), "300000") {
		t.Fatalf("metrics %s: %v", raw, err)
	}
}

// A dismissal with no score (a re-armed signal) must not block the escape: the
// bar is the latest dismissal that has one. Needs STORE_TEST_DSN.
func TestUpsertSignalEscapeSkipsNullScores(t *testing.T) {
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
	const item, cooldown = 987655, 48 * time.Hour
	cleanup := func() { s.Pool.Exec(ctx, `DELETE FROM orchestrator.signals WHERE item_id=$1`, item) }
	cleanup()
	defer cleanup()
	seed := func(metrics string, ago string) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, `INSERT INTO orchestrator.signals
			(kind, item_id, item_name, metrics, status, reason, resolved_at)
			VALUES ('combo', $1, 'x', $2::jsonb, 'dismissed', 'dead', now() - $3::interval)`, item, metrics, ago); err != nil {
			t.Fatal(err)
		}
	}
	seed(`{"budget_cycle_gp":300000}`, "4 days")
	seed(`{"relation_id":7,"source":"confirm_rearm"}`, "3 days") // the latest has no score
	isNew, err := s.UpsertSignal(ctx, "combo", item, "x", map[string]any{"budget_cycle_gp": 500000}, cooldown)
	if err != nil || isNew {
		t.Fatalf("under 2x the last scored dismissal stays out: new=%v err=%v", isNew, err)
	}
	isNew, err = s.UpsertSignal(ctx, "combo", item, "x", map[string]any{"budget_cycle_gp": 600000}, cooldown)
	if err != nil || !isNew {
		t.Fatalf("2x the last scored dismissal re-opens it: new=%v err=%v", isNew, err)
	}
}

// The script's refusals cool the collector's re-queue but falsify nothing: no repeat
// count, no prior verdict in a brief, and a candidate that clears the rules comes back.
// Needs STORE_TEST_DSN.
func TestScriptDismissalsAreNotVerdicts(t *testing.T) {
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
	const item, cooldown = 987661, 48 * time.Hour
	cleanup := func() { s.Pool.Exec(ctx, `DELETE FROM orchestrator.signals WHERE item_id=$1`, item) }
	cleanup()
	defer cleanup()
	seed := func(ago string) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, `INSERT INTO orchestrator.signals
			(kind, item_id, item_name, metrics, status, reason, resolved_at)
			VALUES ('combo', $1, 'x', '{"budget_cycle_gp":300000}'::jsonb, 'dismissed', 'script: lookup', now() - $2::interval)`, item, ago); err != nil {
			t.Fatal(err)
		}
	}
	metrics := map[string]any{"budget_cycle_gp": 300000}

	seed("1 hour")
	if isNew, err := s.UpsertSignal(ctx, "combo", item, "x", metrics, cooldown); err != nil || isNew {
		t.Errorf("inside the cooldown the collector stays out: new=%v err=%v", isNew, err)
	}
	if at, reason, err := s.LatestDismissal(ctx, "combo", item); err != nil || at != nil || reason != nil {
		t.Errorf("a script refusal reads as a prior verdict: %v %v %v", at, reason, err)
	}
	id, isNew, err := s.UpsertCandidateSignal(ctx, "combo", item, "x", metrics, cooldown)
	if err != nil || !isNew || id == 0 {
		t.Fatalf("a candidate passing now comes back: id=%d new=%v err=%v", id, isNew, err)
	}
	if _, isNew, err := s.UpsertCandidateSignal(ctx, "combo", item, "x", metrics, cooldown); err != nil || isNew {
		t.Errorf("a live signal only refreshes: new=%v err=%v", isNew, err)
	}
	cleanup()

	// Past the cooldown, two refusals don't trip the repeat guard.
	seed("4 days")
	seed("3 days")
	if isNew, err := s.UpsertSignal(ctx, "combo", item, "x", metrics, cooldown); err != nil || !isNew {
		t.Errorf("script refusals tripped the repeat guard: new=%v err=%v", isNew, err)
	}
	cleanup()

	// A run's verdict holds a candidate out, the same as it holds out the collector.
	if _, err := s.Pool.Exec(ctx, `INSERT INTO orchestrator.signals
		(kind, item_id, item_name, metrics, status, reason, resolved_at)
		VALUES ('combo', $1, 'x', '{"budget_cycle_gp":300000}'::jsonb, 'dismissed', 'margin_collapse', now() - interval '1 hour')`, item); err != nil {
		t.Fatal(err)
	}
	if _, isNew, err := s.UpsertCandidateSignal(ctx, "combo", item, "x", metrics, cooldown); err != nil || isNew {
		t.Errorf("a verdict didn't hold the candidate out: new=%v err=%v", isNew, err)
	}
}

// An evaluate run's silence is a verdict that holds the candidate out. A signal it
// shipped without answering stays out of that. Needs STORE_TEST_DSN.
func TestDismissUnanswered(t *testing.T) {
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
	const passed, shipped, cooldown = 987662, 987663, 48 * time.Hour
	cleanup := func() {
		s.Pool.Exec(ctx, `DELETE FROM orchestrator.signals WHERE item_id = ANY($1)`, []int{passed, shipped})
	}
	cleanup()
	defer cleanup()
	metrics := map[string]any{"budget_cycle_gp": 300000}
	passedID, _, err := s.UpsertCandidateSignal(ctx, "combo", passed, "x", metrics, cooldown)
	if err != nil {
		t.Fatal(err)
	}
	shippedID, _, err := s.UpsertCandidateSignal(ctx, "combo", shipped, "y", metrics, cooldown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.OrphanRunningRuns(ctx); err != nil {
		t.Fatal(err)
	}
	runID, err := s.CreateRun(ctx, json.RawMessage(`{}`), "test brief", "signal")
	if err != nil {
		t.Fatal(err)
	}
	defer s.OrphanRunningRuns(ctx)
	if err := s.AssignSignals(ctx, runID, []int64{passedID, shippedID}); err != nil {
		t.Fatal(err)
	}
	var st SidecarStrategy
	if err := json.Unmarshal([]byte(`{"id":"C-y","archetype":"C","title":"t","thesis":"t",
		"items":[{"name":"y","id":987663}],"entry":"e","exit":"x","entry_price":1,"exit_price":2,
		"horizon":"h","capital_required":1,"size":{"buy_limit":1,"vol_constrained":1,"units_used":1},
		"expected_value":{"per_cycle_gp":1,"per_1h_gp":1,"per_day_gp":1,"roi_pct":1},
		"confidence":"medium","invalidation":"i"}`), &st); err != nil {
		t.Fatal(err)
	}
	list := []SidecarStrategy{st}
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
	if err := s.DismissUnanswered(ctx, runID, "no verdict: run saw it"); err != nil {
		t.Fatal(err)
	}
	var status, reason string
	if err := s.Pool.QueryRow(ctx, `SELECT status, reason FROM orchestrator.signals WHERE signal_id=$1`, passedID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "dismissed" || reason != "no verdict: run saw it" {
		t.Errorf("unanswered signal %s: %q", status, reason)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT status FROM orchestrator.signals WHERE signal_id=$1`, shippedID).Scan(&status); err != nil || status != "assigned" {
		t.Errorf("shipped signal %q: %v", status, err)
	}
	if err := s.ReleaseRunSignals(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if _, isNew, err := s.UpsertCandidateSignal(ctx, "combo", passed, "x", metrics, cooldown); err != nil || isNew {
		t.Errorf("silence didn't hold the candidate out: new=%v err=%v", isNew, err)
	}
}

// A run orphaned by a restart gives its assigned signals back, or their items
// could never queue again. Needs STORE_TEST_DSN.
func TestOrphanRunningRunsReleasesSignals(t *testing.T) {
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
	const item = 987656
	cleanup := func() { s.Pool.Exec(ctx, `DELETE FROM orchestrator.signals WHERE item_id=$1`, item) }
	cleanup()
	defer cleanup()
	if _, err := s.OrphanRunningRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertSignal(ctx, "vflip", item, "x", map[string]any{"gp_cycle": 900000}, 0); err != nil {
		t.Fatal(err)
	}
	runID, err := s.CreateRun(ctx, json.RawMessage(`{}`), "test brief", "manual")
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := s.Pool.QueryRow(ctx, `SELECT signal_id FROM orchestrator.signals WHERE item_id=$1`, item).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignSignals(ctx, runID, []int64{id}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.OrphanRunningRuns(ctx); err != nil || n != 1 {
		t.Fatalf("orphaned %d: %v", n, err)
	}
	var status string
	var run *int64
	if err := s.Pool.QueryRow(ctx, `SELECT status, run_id FROM orchestrator.signals WHERE signal_id=$1`, id).Scan(&status, &run); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || run != nil {
		t.Fatalf("status %s run %v", status, run)
	}
}

func TestIsFamilyKey(t *testing.T) {
	for key, want := range map[string]bool{"C": false, "F": false, "C:set": false, "C:decant": false, "C:moon": true, "C:godsword": true} {
		if got := IsFamilyKey(key); got != want {
			t.Errorf("IsFamilyKey(%q) = %v", key, got)
		}
	}
}
