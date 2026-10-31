package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/osrs-ge/ge-orchestrator/internal/brief"

	"github.com/osrs-ge/ge-orchestrator/internal/eval"
	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

var packetDay = time.Date(2026, 10, 31, 4, 0, 0, 0, time.UTC)

func gp(v int64) *int64 { return &v }

func secs(v int) *int { return &v }

// A two-input combine: 2M + 1M in, 3.5M out less 70k tax, so a 430k margin. The sell
// leg's volume caps it: 15% of 40 is 6 conversions, under the buy limit of 8.
func comboFixture() (store.ComboCandidate, []store.RelationLeg, []store.RelationLeg, map[int]*eval.Snap, map[int]int64, map[int]string) {
	c := store.ComboCandidate{RelationID: 7, Relation: "Blade + hilt", Kind: "combine", Direction: "forward", PrimaryItemID: 3}
	buys := []store.RelationLeg{{ItemID: 1, Qty: 1}, {ItemID: 2, Qty: 1}}
	sells := []store.RelationLeg{{ItemID: 3, Qty: 1}}
	snaps := map[int]*eval.Snap{
		1: {Low: gp(2_000_000), LowAgeS: secs(600), Vol4h: 100, LowTicks24h: 144},
		2: {Low: gp(1_000_000), LowAgeS: secs(300), Vol4h: 200, LowTicks24h: 288},
		3: {High: gp(3_500_000), HighAgeS: secs(120), Vol4h: 40, HighTicks24h: 72},
	}
	limits := map[int]int64{1: 8, 2: 70, 3: 8}
	names := map[int]string{1: "Blade", 2: "Hilt", 3: "Godsword"}
	return c, buys, sells, snaps, limits, names
}

func TestSizeComboDraftPassesTheVetsArithmetic(t *testing.T) {
	c, buys, sells, snaps, limits, names := comboFixture()
	got, why := sizeCombo(c, buys, sells, snaps, limits, names, nil, 50_000_000, packetDay)
	if why != "" {
		t.Fatal(why)
	}
	var st store.SidecarStrategy
	if err := json.Unmarshal(got.draft, &st); err != nil {
		t.Fatal(err)
	}
	if st.EntryPrice != 3_000_000 || st.ExitPrice != 3_430_000 {
		t.Errorf("entry %d, exit %d", st.EntryPrice, st.ExitPrice)
	}
	if st.Size.BuyLimit != 8 || st.Size.VolConstrained != 6 || st.Size.UnitsUsed != 6 {
		t.Errorf("size %+v", st.Size)
	}
	if st.ExpectedValue.PerCycleGp != 2_580_000 || st.ExpectedValue.Per1hGp != 645_000 || st.CapitalRequired != 18_000_000 {
		t.Errorf("ev %+v, capital %d", st.ExpectedValue, st.CapitalRequired)
	}
	// items[0] is what the dedup and the signals key on: the output, not a buy leg.
	if st.PrimaryItemID() != 3 || len(st.Items) != 3 || st.Items[0].BuyLimit == nil {
		t.Errorf("items %+v", st.Items)
	}
	if reason := sizeConsistencyVeto(st, 50_000_000); reason != "" {
		t.Error(reason)
	}
	if reason := comboBuyLimitVeto(st, limits); reason != "" {
		t.Error(reason)
	}
	if reason := comboFloorVeto(st); reason != "" {
		t.Error(reason)
	}
	rel := &store.Relation{RelationID: 7, Kind: "combine", Inputs: buys, Outputs: sells}
	if reason := comboRecipeVeto(st, rel); reason != "" {
		t.Error(reason)
	}
	// The stalest leg against its own cadence: 600s at one trade every 600s is 1.0.
	if got.facts.WorstLegAgeRatio == nil || *got.facts.WorstLegAgeRatio != 1 {
		t.Errorf("worst ratio %v", got.facts.WorstLegAgeRatio)
	}
}

func TestSizeComboBudgetBounds(t *testing.T) {
	c, buys, sells, snaps, limits, names := comboFixture()
	got, why := sizeCombo(c, buys, sells, snaps, limits, names, nil, 9_000_000, packetDay)
	if why != "" {
		t.Fatal(why)
	}
	var st store.SidecarStrategy
	if err := json.Unmarshal(got.draft, &st); err != nil {
		t.Fatal(err)
	}
	if st.Size.UnitsUsed != 3 {
		t.Errorf("units %d, want 9M / 3M = 3", st.Size.UnitsUsed)
	}
}

func TestSizeComboRefusals(t *testing.T) {
	cases := []struct {
		name    string
		breakIt func(snaps map[int]*eval.Snap, limits map[int]int64)
		want    string
	}{
		// Nulls are signal: an untraded leg isn't a free one.
		{"no buy price", func(s map[int]*eval.Snap, _ map[int]int64) { s[1].Low = nil }, "no buy price"},
		{"no sell price", func(s map[int]*eval.Snap, _ map[int]int64) { s[3].High = nil }, "no sell price"},
		{"no snapshot", func(s map[int]*eval.Snap, _ map[int]int64) { delete(s, 2) }, "no snapshot"},
		{"no limits", func(_ map[int]*eval.Snap, l map[int]int64) { delete(l, 1); delete(l, 2) }, "known limit"},
		{"no margin", func(s map[int]*eval.Snap, _ map[int]int64) { *s[3].High = 3_000_000 }, "no margin"},
		{"too thin", func(s map[int]*eval.Snap, _ map[int]int64) { s[3].Vol4h = 5 }, "can't fill"},
		// One conversion at 15% of 10: 430k would clear, so shrink the margin to fail on the floor.
		{"under the floor", func(s map[int]*eval.Snap, _ map[int]int64) {
			s[3].Vol4h = 10
			*s[3].High = 3_100_000
		}, "under the C floor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, buys, sells, snaps, limits, names := comboFixture()
			tc.breakIt(snaps, limits)
			got, why := sizeCombo(c, buys, sells, snaps, limits, names, nil, 50_000_000, packetDay)
			if got != nil || !strings.Contains(why, tc.want) {
				t.Errorf("built %v, why %q", got != nil, why)
			}
		})
	}
}

func TestSizeComboReverseNamesItself(t *testing.T) {
	c, buys, sells, snaps, limits, names := comboFixture()
	c.Direction = "reverse"
	got, why := sizeCombo(c, buys, sells, snaps, limits, names, nil, 50_000_000, packetDay)
	if why != "" {
		t.Fatal(why)
	}
	var st store.SidecarStrategy
	if err := json.Unmarshal(got.draft, &st); err != nil {
		t.Fatal(err)
	}
	if st.ID != "C-blade-hilt-reverse-20261031" {
		t.Errorf("id %s", st.ID)
	}
}

// The agent's validator rejects any other shape, which would fail the whole report.
func TestDraftIDMatchesTheAgentsRule(t *testing.T) {
	re := regexp.MustCompile(`^[FBSVCUHP]-[a-z0-9-]+-\d{8}$`)
	for _, name := range []string{"Verac's armour set reverse", "Super restore 3<->4", "Armadyl helmet -> armadylean plate", "Masori mask (f)"} {
		if id := draftID("C", name, packetDay); !re.MatchString(id) {
			t.Errorf("%q gave %q", name, id)
		}
	}
}

func TestProcessDraftCopiesTheHarnessNumbers(t *testing.T) {
	in, margin := 15830.0, 1010.0
	output, units, gpHr := 2509, int64(2036), int64(430079)
	row := &store.PacketRow{RecipeID: 17, OutputItem: &output, UnitsPer4h: &units, GpHr: &gpHr}
	raw, err := processDraft(row, &eval.ProcessSnap{GrossIn: &in, MarginSlow: &margin}, 0, "Black dragon leather", packetDay)
	if err != nil {
		t.Fatal(err)
	}
	var st store.SidecarStrategy
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.ID != "P-black-dragon-leather-20261031" || st.EntryPrice != 15830 || st.ExitPrice != 16840 || st.PrimaryItemID() != 2509 {
		t.Errorf("%+v", st)
	}
	if st.Size.UnitsUsed != units || st.Size.BuyLimit != units || st.Size.VolConstrained != units {
		t.Errorf("size %+v", st.Size)
	}
	if st.ExpectedValue.Per1hGp != gpHr || st.ExpectedValue.PerCycleGp != gpHr*4 || st.ExpectedValue.PerDayGp != gpHr*4 {
		t.Errorf("ev %+v", st.ExpectedValue)
	}
	if st.CapitalRequired != units*15830 || sizeConsistencyVeto(st, 50_000_000) != "" {
		t.Errorf("capital %d", st.CapitalRequired)
	}
	if st.RecipeID == nil || *st.RecipeID != 17 || st.AttentionSpec != nil || st.KillPrice != nil {
		t.Errorf("lane fields %+v", st)
	}
}

func TestProcessDraftRefusesALostPrice(t *testing.T) {
	output, units, gpHr := 2509, int64(2036), int64(430079)
	row := &store.PacketRow{RecipeID: 17, OutputItem: &output, UnitsPer4h: &units, GpHr: &gpHr}
	if _, err := processDraft(row, &eval.ProcessSnap{}, 0, "x", packetDay); err == nil {
		t.Error("a recipe with no prices got a draft")
	}
}

func TestSplitSignals(t *testing.T) {
	signals := []store.Signal{
		{SignalID: 1, Kind: "combo", ItemID: 10},
		{SignalID: 2, Kind: "combo", ItemID: 20},
		{SignalID: 3, Kind: "process", ItemID: 10},
		{SignalID: 4, Kind: "process", ItemID: 30},
	}
	fate := map[packetKey]string{
		{"C", 10}: "",
		{"C", 20}: "under the C floor",
		{"P", 10}: "already open",
	}
	assigned, dismiss := splitSignals(signals, fate)
	if len(assigned) != 1 || assigned[0].SignalID != 1 {
		t.Errorf("assigned %+v", assigned)
	}
	if dismiss[2] != "script: under the C floor" || dismiss[3] != "script: already open" {
		t.Errorf("dismiss %v", dismiss)
	}
	// Item 10 is a C candidate, which says nothing of the recipe that makes it.
	if !strings.Contains(dismiss[4], "no longer clears") || len(dismiss) != 3 {
		t.Errorf("dismiss %v", dismiss)
	}
}

func day(n int) time.Time { return time.Date(2026, 10, n, 0, 0, 0, 0, time.UTC) }

func TestMarginHistory(t *testing.T) {
	buys := []store.RelationLeg{{ItemID: 1, Qty: 2}}
	sells := []store.RelationLeg{{ItemID: 2, Qty: 1}}
	closes := map[int][]store.DailyClose{
		1: {{Day: day(1), Low: gp(100)}, {Day: day(2), Low: gp(100)}, {Day: day(3), Low: nil}, {Day: day(8), Low: gp(100)}},
		2: {{Day: day(1), High: gp(1000)}, {Day: day(2), High: gp(150)}, {Day: day(3), High: gp(9000)}, {Day: day(8), High: gp(1100)}},
	}
	var f comboFacts
	marginHistory(&f, buys, sells, closes)
	// Oct 3 has no buy close, so its margin is unknown rather than a free 8820 gp.
	if f.MarginDays == nil || *f.MarginDays != 3 || f.MarginDaysPositive == nil || *f.MarginDaysPositive != 2 {
		t.Errorf("days %v positive %v", f.MarginDays, f.MarginDaysPositive)
	}
	if f.MarginMedian14d == nil || *f.MarginMedian14d != 780 {
		t.Errorf("median %v", f.MarginMedian14d)
	}
	if f.OutputChange7dPct == nil || math.Abs(*f.OutputChange7dPct-10) > 0.01 {
		t.Errorf("change %v", f.OutputChange7dPct)
	}
}

// No history stays null: the model must not read an unknown as a bad margin.
func TestMarginHistoryWithoutCloses(t *testing.T) {
	var f comboFacts
	marginHistory(&f, []store.RelationLeg{{ItemID: 1, Qty: 1}}, []store.RelationLeg{{ItemID: 2, Qty: 1}}, nil)
	if f.MarginDays != nil || f.MarginMedian14d != nil || f.MarginDaysPositive != nil || f.OutputChange7dPct != nil {
		t.Errorf("%+v", f)
	}
}

func TestChange7dNeedsAWeekOfHistory(t *testing.T) {
	if got := change7d([]store.DailyClose{{Day: day(5), High: gp(100)}, {Day: day(8), High: gp(120)}}); got != nil {
		t.Errorf("got %v from 3 days", *got)
	}
}

// TestEvaluateRunWiring runs against a stand-in agent that records its environment: the
// mode, directive, tool switch and turn cap are the difference between a run costing
// thousands of tokens and a million. Needs STORE_TEST_DSN.
func TestEvaluateRunWiring(t *testing.T) {
	dsn := os.Getenv("STORE_TEST_DSN")
	if dsn == "" {
		t.Skip("STORE_TEST_DSN not set")
	}
	ctx := context.Background()
	s, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool.Close()
	recipes, err := s.ProcessQuotes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recipes) == 0 {
		t.Skip("no recipes seeded")
	}
	if _, err := s.OrphanRunningRuns(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.OrphanRunningRuns(ctx)

	dir := t.TempDir()
	agent := filepath.Join(dir, "agent.sh")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\nset > env.txt\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{
		Store: s, Hub: NewHub(),
		Cfg: Config{
			AgentPath: agent, Directive: "/research.md", EvaluateDirective: "/evaluate.md", StateDir: dir,
			SignalRunMode: ModeEvaluate, GraveyardCooldown: time.Hour,
			ExtraEnv: []string{"GE_AGENT_MAX_TURNS=80"},
		},
		Prices: scriptedPrices{snap: func(int) (*eval.ProcessSnap, error) { return goodRecipe(), nil }},
	}

	// Nothing clears the rules on this data (no snapshot history), so no run starts. The
	// C signal gets the script's answer, and a kind the packet can't answer stays queued.
	const volumeItem, comboItem = 876581, 876582
	cleanup := func() {
		s.Pool.Exec(ctx, `DELETE FROM orchestrator.signals WHERE item_id = ANY($1)`, []int{volumeItem, comboItem})
	}
	cleanup()
	defer cleanup()
	if _, err := s.UpsertSignal(ctx, "volume", volumeItem, "x", map[string]any{"z": 9}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertSignal(ctx, "combo", comboItem, "x", map[string]any{"budget_cycle_gp": 900000}, 0); err != nil {
		t.Fatal(err)
	}
	before, err := s.LastRunStart(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Trigger(ctx, brief.Defaults(), "signal"); !errors.Is(err, ErrNothingToEvaluate) {
		t.Fatalf("trigger: %v", err)
	}
	if r.ActiveRunID() != 0 {
		t.Error("an empty packet started a run")
	}
	if after, err := s.LastRunStart(ctx); err != nil || (before != nil && !after.Equal(*before)) || (before == nil && after != nil) {
		t.Errorf("an empty packet created a run: %v, %v: %v", before, after, err)
	}
	var status, reason string
	if err := s.Pool.QueryRow(ctx, `SELECT status, coalesce(reason,'') FROM orchestrator.signals WHERE item_id=$1`, comboItem).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "dismissed" || !strings.HasPrefix(reason, "script: ") {
		t.Errorf("combo signal %s: %q", status, reason)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT status FROM orchestrator.signals WHERE item_id=$1`, volumeItem).Scan(&status); err != nil || status != "pending" {
		t.Errorf("volume signal %q: %v", status, err)
	}

	// A run with a packet: its mode, directive, tool switch and turn cap.
	p := brief.Defaults()
	text, err := brief.RenderEvaluate(ctx, s, p, time.Now().UTC(), nil, []string{`{"lane":"C"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "### Packet (candidates: 1)\n{\"lane\":\"C\"}\n") || strings.Contains(text, "Latest market sweep") {
		t.Errorf("brief %q", text)
	}
	r.mu.Lock()
	runID, err := r.launch(ctx, p, "signal", ModeEvaluate, text, nil, []store.PacketRow{{RecipeID: 1, Name: "x"}})
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool.Exec(ctx, `DELETE FROM orchestrator.lane_packets WHERE run_id=$1`, runID)
	waitForRun(t, r)
	run, err := s.Run(ctx, runID)
	if err != nil || run == nil || run.Mode != ModeEvaluate {
		t.Fatalf("run %+v: %v", run, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "runs", fmt.Sprint(runID), "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"GE_AGENT_EVALUATE=1", "GE_AGENT_DIRECTIVE=/evaluate.md", fmt.Sprintf("GE_AGENT_MAX_TURNS=%d", evaluateMaxTurns)} {
		if !strings.Contains(string(raw), want+"\n") {
			t.Errorf("env is missing %s:\n%s", want, raw)
		}
	}
	var mode string
	if err := s.Pool.QueryRow(ctx, `SELECT mode FROM orchestrator.lane_packets WHERE run_id=$1`, runID).Scan(&mode); err != nil || mode != "live" {
		t.Errorf("packet mode %q: %v", mode, err)
	}

	// Manual runs still research, even with evaluate on for signals.
	manualID, err := r.Trigger(ctx, brief.Defaults(), "manual")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool.Exec(ctx, `DELETE FROM orchestrator.lane_packets WHERE run_id=$1`, manualID)
	waitForRun(t, r)
	manual, err := s.Run(ctx, manualID)
	if err != nil || manual == nil || manual.Mode != ModeResearch {
		t.Errorf("manual run %+v: %v", manual, err)
	}
	raw, err = os.ReadFile(filepath.Join(dir, "runs", fmt.Sprint(manualID), "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if env := string(raw); strings.Contains(env, "GE_AGENT_EVALUATE") || !strings.Contains(env, "GE_AGENT_DIRECTIVE=/research.md\n") {
		t.Errorf("research env:\n%s", env)
	}
}

func waitForRun(t *testing.T, r *Runner) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for r.ActiveRunID() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the run didn't finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
