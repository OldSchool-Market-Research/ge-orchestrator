package runner

import (
	"reflect"
	"strings"
	"testing"

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
	judgeRecipe(&row, goodRecipe(), testBudget)
	if !row.Ship || row.Why != "" {
		t.Fatalf("ship %v, why %q", row.Ship, row.Why)
	}
	if row.UnitsPer4h != 5_760 || row.GpHr == nil || *row.GpHr != 408_240 {
		t.Errorf("units %d, gp/hr %v", row.UnitsPer4h, row.GpHr)
	}
	if row.OutputItem == nil || *row.OutputItem != 8782 {
		t.Errorf("output %v", row.OutputItem)
	}
}

// A shipped verdict must be one the vet's lane P rules would accept: same snapshot,
// the size the row says, the same projection.
func TestJudgeRecipeAgreesWithTheVet(t *testing.T) {
	snap := goodRecipe()
	row := store.PacketRow{Hours24h: 24, HoursAtFloor24h: 24}
	judgeRecipe(&row, snap, testBudget)
	if reason := processVeto(snap, eval.ProjectProcessPer1h(snap, row.UnitsPer4h)); reason != "" {
		t.Errorf("vet refused a shipped row: %s", reason)
	}
	if reason := processPersistenceVeto(row.HoursAtFloor24h, row.Hours24h); reason != "" {
		t.Errorf("vet refused a shipped row: %s", reason)
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
		judgeRecipe(&row, s, testBudget)
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
		if c.wantNoSize && row.UnitsPer4h != 0 {
			t.Errorf("%s: units %d", c.name, row.UnitsPer4h)
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
