package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osrs-ge/ge-orchestrator/internal/brief"
	"github.com/osrs-ge/ge-orchestrator/internal/eval"
	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

const testBudget = 50_000_000

// goodRecipe pays 408,240 gp/hr at 5,760 actions per 4h.
func goodRecipe() *eval.ProcessSnap {
	f := func(v float64) *float64 { return &v }
	output := 8782
	return &eval.ProcessSnap{MarginSlow: f(300), GrossIn: f(1_500), GrossOut: f(1_800), ActionsPerHour: f(1_440),
		LimitRate: f(2_750), MarketRate: f(160_000), Throughput: "fixed", OutputItem: &output}
}

func TestJudgeRecipeShips(t *testing.T) {
	row := store.PacketRow{RecipeID: 26, Hours24h: 24, HoursAtFloor24h: 24}
	judgeRecipe(&row, goodRecipe(), 0, testBudget)
	if !row.Ship || row.Why != "" {
		t.Fatalf("ship %v, why %q", row.Ship, row.Why)
	}
	if row.UnitsPer4h == nil || *row.UnitsPer4h != 5_760 || row.GpHr == nil || *row.GpHr != 408_240 {
		t.Errorf("units %v, gp/hr %v", row.UnitsPer4h, row.GpHr)
	}
	if row.OutputItem == nil || *row.OutputItem != 8782 {
		t.Errorf("output %v", row.OutputItem)
	}
}

// A budget-bound recipe whose fee pushes units x entry over the budget: the bound on
// units leaves the fee out, and the entry_price a strategy copies includes it. The vet
// would let 10% through, but the agent's validator rejects the report at a gp over.
func TestJudgeRecipeHoldsCapitalToTheBudget(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	s := goodRecipe()
	// 100k input at 50M budget: 500 actions per 4h, 125/hr, 1.4M margin an action.
	s.GrossIn, s.GrossOut, s.MarginSlow = f(100_000), f(2_000_000), f(1_400_000)
	for _, c := range []struct {
		name string
		fee  float64
		ship bool
	}{
		{"no fee", 0, true},
		{"fee inside the vet's tolerance", 10_000, false},
		{"fee over it", 10_001, false},
	} {
		row := store.PacketRow{Hours24h: 24, HoursAtFloor24h: 24}
		judgeRecipe(&row, s, c.fee, testBudget)
		if row.Ship != c.ship {
			t.Errorf("%s: ship %v, why %q", c.name, row.Ship, row.Why)
		}
		if !c.ship && !strings.Contains(row.Why, "over the 50000000 research budget") {
			t.Errorf("%s: why %q", c.name, row.Why)
		}
	}
}

func TestJudgeRecipeBlocks(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	age := int64(6*3600 + 1)
	cases := []struct {
		name       string
		change     func(*eval.ProcessSnap)
		hours, at  int
		want       string
		wantNilGp  bool
		wantNoSize bool
	}{
		{name: "under the floor", change: func(s *eval.ProcessSnap) { s.MarginSlow = f(100) }, hours: 24, at: 24, want: "lane-P floor"},
		{name: "linked to C", change: func(s *eval.ProcessSnap) { s.Linked = true }, hours: 24, at: 24, want: "lane C"},
		{name: "no known rate", change: func(s *eval.ProcessSnap) { s.Throughput = "unknown" }, hours: 24, at: 24, want: "no known rate"},
		{name: "unknown limit", change: func(s *eval.ProcessSnap) { s.UnknownLimitLegs = 1 }, hours: 24, at: 24, want: "buy limit"},
		{name: "stale leg", change: func(s *eval.ProcessSnap) { s.MaxLegAgeS = &age }, hours: 24, at: 24, want: "6h"},
		{name: "loses money", change: func(s *eval.ProcessSnap) { s.MarginSlow = f(-5) }, hours: 24, at: 24, want: "doesn't earn", wantNilGp: true},
		{name: "unpriceable", change: func(s *eval.ProcessSnap) { s.MarginSlow = nil }, hours: 24, at: 24, want: "doesn't earn", wantNilGp: true},
		{name: "no size", change: func(s *eval.ProcessSnap) { s.GrossIn = nil }, hours: 24, at: 24, want: "doesn't earn", wantNilGp: true, wantNoSize: true},
		{name: "too little history", change: func(*eval.ProcessSnap) {}, hours: 11, at: 11, want: "hourly snapshots"},
		{name: "a spike, not a habit", change: func(*eval.ProcessSnap) {}, hours: 24, at: 19, want: "gate of 80%"},
		{name: "no output", change: func(s *eval.ProcessSnap) { s.OutputItem = nil }, hours: 24, at: 24, want: "no main output"},
	}
	for _, c := range cases {
		s := goodRecipe()
		c.change(s)
		row := store.PacketRow{Hours24h: c.hours, HoursAtFloor24h: c.at}
		judgeRecipe(&row, s, 0, testBudget)
		if row.Ship {
			t.Errorf("%s: shipped", c.name)
		}
		if !strings.Contains(row.Why, c.want) {
			t.Errorf("%s: why %q, want %q", c.name, row.Why, c.want)
		}
		if strings.HasPrefix(row.Why, "vetoed") {
			t.Errorf("%s: why keeps the vet's prefix: %q", c.name, row.Why)
		}
		if c.wantNilGp && row.GpHr != nil {
			t.Errorf("%s: gp/hr %d, want null", c.name, *row.GpHr)
		}
		if c.wantNoSize && row.UnitsPer4h != nil {
			t.Errorf("%s: units %d, want null", c.name, *row.UnitsPer4h)
		}
	}
}

func packetRow(recipe, item int, gp int64) store.PacketRow {
	return store.PacketRow{RecipeID: recipe, OutputItem: &item, GpHr: &gp, Ship: true}
}

func TestKeepBestPerOutput(t *testing.T) {
	blocked := store.PacketRow{RecipeID: 9, Why: "under the floor"}
	rows := []store.PacketRow{
		packetRow(1, 100, 300_000),
		packetRow(2, 100, 500_000), // the best of item 100, after a worse one
		packetRow(3, 100, 400_000),
		packetRow(4, 200, 260_000),
		blocked,
		packetRow(6, 300, 300_000),
		packetRow(5, 300, 300_000), // a tie goes to the lowest recipe id
	}
	keepBestPerOutput(rows)
	ship := []int{}
	for _, r := range rows {
		if r.Ship {
			ship = append(ship, r.RecipeID)
		}
	}
	if !reflect.DeepEqual(ship, []int{2, 4, 5}) {
		t.Errorf("shipping %v", ship)
	}
	if !strings.Contains(rows[0].Why, "recipe 2") || !strings.Contains(rows[2].Why, "recipe 2") || !strings.Contains(rows[5].Why, "recipe 5") {
		t.Errorf("why: %q, %q, %q", rows[0].Why, rows[2].Why, rows[5].Why)
	}
	if rows[4].Why != "under the floor" {
		t.Errorf("a blocked row changed: %q", rows[4].Why)
	}
}

func TestSortPacket(t *testing.T) {
	gp := func(v int64) *int64 { return &v }
	rows := []store.PacketRow{
		{RecipeID: 5},
		{RecipeID: 1, GpHr: gp(100)},
		{RecipeID: 4},
		{RecipeID: 3, GpHr: gp(300)},
		{RecipeID: 2, GpHr: gp(100)},
	}
	sortPacket(rows)
	got := []int{}
	for _, r := range rows {
		got = append(got, r.RecipeID)
	}
	if !reflect.DeepEqual(got, []int{3, 1, 2, 4, 5}) {
		t.Errorf("order %v", got)
	}
}

// With no price source or the lane off, a build stops before touching the store, which
// is nil here: reaching it would panic.
func TestLogProcessPacketSkips(t *testing.T) {
	(&Runner{}).logProcessPacket(1, brief.Params{})
	r := &Runner{Prices: scriptedPrices{}}
	r.logProcessPacket(1, brief.Params{Archetypes: map[string]float64{"P": 0}})
}

// scriptedPrices answers ProcessSnapshot only: the packet reads nothing else from it.
type scriptedPrices struct {
	eval.PriceSource
	snap func(int) (*eval.ProcessSnap, error)
}

func (s scriptedPrices) ProcessSnapshot(_ context.Context, id int) (*eval.ProcessSnap, error) {
	return s.snap(id)
}

// TestBuildProcessPacket walks the build over the seeded recipes: a failed lookup is
// that recipe's verdict, the deadline fails the whole packet, and an open strategy
// blocks its item. Needs a migrated scratch DB: set STORE_TEST_DSN; skipped otherwise.
func TestBuildProcessPacket(t *testing.T) {
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

	// A lookup failure blocks that recipe and only that recipe.
	failing := recipes[0].RecipeID
	r := &Runner{Store: s, Cfg: Config{GraveyardCooldown: time.Hour}, Prices: scriptedPrices{snap: func(id int) (*eval.ProcessSnap, error) {
		if id == failing {
			return nil, errors.New("boom")
		}
		return goodRecipe(), nil
	}}}
	p := brief.Defaults()
	rows, err := r.buildProcessPacket(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(recipes) {
		t.Fatalf("%d rows for %d recipes", len(rows), len(recipes))
	}
	for _, row := range rows {
		if row.RecipeID == failing && !strings.Contains(row.Why, "boom") {
			t.Errorf("failed lookup: %+v", row)
		}
		if row.RecipeID != failing && strings.Contains(row.Why, "boom") {
			t.Errorf("recipe %d took another's failure: %q", row.RecipeID, row.Why)
		}
	}

	// A deadline fails the packet, rather than storing every recipe as "lookup failed".
	done, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.buildProcessPacket(done, p); err == nil {
		t.Error("a cancelled build returned rows")
	}

	// An open P strategy blocks its item; another item still ships.
	const item, other = 876571, 876572
	if _, err := s.OrphanRunningRuns(ctx); err != nil {
		t.Fatal(err)
	}
	runID, err := s.CreateRun(ctx, json.RawMessage(`{}`), "test brief", "manual")
	if err != nil {
		t.Fatal(err)
	}
	defer s.OrphanRunningRuns(ctx)
	defer s.Pool.Exec(ctx, `DELETE FROM orchestrator.strategies WHERE run_id=$1`, runID)
	var st store.SidecarStrategy
	raw := fmt.Sprintf(`{"id":"P-packet-dedup","archetype":"P","title":"t","thesis":"t",
		"items":[{"name":"x","id":%d}],"entry":"e","exit":"x","entry_price":1,"exit_price":2,
		"horizon":"h","capital_required":1,"size":{"buy_limit":1,"vol_constrained":1,"units_used":1},
		"expected_value":{"per_cycle_gp":1,"per_1h_gp":1,"per_day_gp":1,"roi_pct":1},
		"confidence":"medium","invalidation":"i"}`, item)
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertStrategies(ctx, runID, time.Now().UTC(), []store.SidecarStrategy{st}, nil); err != nil {
		t.Fatal(err)
	}
	book := []store.PacketRow{packetRow(1, item, 300_000), packetRow(2, other, 300_000)}
	r.blockOnRecord(ctx, book)
	if book[0].Ship || !strings.Contains(book[0].Why, "already has an open P strategy") {
		t.Errorf("open item: %+v", book[0])
	}
	if !book[1].Ship {
		t.Errorf("other item: %+v", book[1])
	}
}
