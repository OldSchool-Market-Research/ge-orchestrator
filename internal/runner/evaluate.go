package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/osrs-ge/ge-orchestrator/internal/brief"
	"github.com/osrs-ge/ge-orchestrator/internal/eval"
	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

// Evaluate mode: a script finds, prices and sizes a run's candidates with the vet's own
// rules, and the model judges the result with no data tools. Research runs search; these
// answer the hourly signals, which took 88% of input tokens before this (11.7M of 13.3M
// over the 24 runs to Oct 31).

const (
	ModeResearch = "research"
	ModeEvaluate = "evaluate"

	// evaluateMaxTurns is a report, a few fixes and the closing message.
	evaluateMaxTurns = 6
	// comboScan is how many sweep rows a packet looks through for ones that clear the floor.
	comboScan = 100
	// comboMaxCandidates caps what the model reads. Signals already queued always make the list.
	comboMaxCandidates = 15
	// signalBatch is how many of the queue's C and P signals one evaluate run answers.
	signalBatch = 20
	// lookupFailed starts a fate that says nothing about the item: its signal stays queued.
	lookupFailed = "lookup failed: "
	// marginHistoryDays is the window behind a candidate's margin_days_positive.
	marginHistoryDays = 14
)

// ErrNothingToEvaluate means nothing new cleared the rules, so no run started and nothing
// was spent. The next collector cycle tries again.
var ErrNothingToEvaluate = errors.New("nothing to evaluate")

// evaluateKinds are the signals a packet can answer. Every other kind stays queued for
// the next research run, which has the tools to look into it.
var evaluateKinds = []string{"combo", "process"}

// candidate is one line of a packet: what the model judges, and the fields it copies.
type candidate struct {
	Lane  string          `json:"lane"`
	Facts any             `json:"facts"`
	Draft json.RawMessage `json:"draft"`
}

// strategyDraft is a strategy's machine fields under the sidecar's JSON names. The model
// copies it unchanged and adds the words, so a number it ships is one the vet passed.
type strategyDraft struct {
	ID              string      `json:"id"`
	Archetype       string      `json:"archetype"`
	Items           []draftItem `json:"items"`
	EntryPrice      int64       `json:"entry_price"`
	ExitPrice       int64       `json:"exit_price"`
	CapitalRequired int64       `json:"capital_required"`
	Size            draftSize   `json:"size"`
	ExpectedValue   draftEV     `json:"expected_value"`
	Legs            []store.Leg `json:"legs,omitempty"`
	RelationID      *int        `json:"relation_id,omitempty"`
	RecipeID        *int        `json:"recipe_id,omitempty"`
}

type draftItem struct {
	Name     string `json:"name"`
	ID       int    `json:"id"`
	BuyLimit *int64 `json:"buy_limit"`
}

type draftSize struct {
	BuyLimit       int64 `json:"buy_limit"`
	VolConstrained int64 `json:"vol_constrained"`
	UnitsUsed      int64 `json:"units_used"`
}

type draftEV struct {
	PerCycleGp int64   `json:"per_cycle_gp"`
	Per1hGp    int64   `json:"per_1h_gp"`
	PerDayGp   int64   `json:"per_day_gp"`
	RoiPct     float64 `json:"roi_pct"`
}

// comboLegFacts is what a conversion's legs add to its draft's prices.
type comboLegFacts struct {
	ItemID    int   `json:"item_id"`
	Vol4h     int64 `json:"vol_4h"`
	AgeS      *int  `json:"age_s"`
	Trades24h int64 `json:"trades_24h"`
}

// comboFacts is a conversion as the model judges it. Nil numbers are nulls kept as
// nulls: a margin history that couldn't be read, never a zero. The script refuses a
// null harness_gp_per_1h, because that means no margin survives slippage.
type comboFacts struct {
	Relation           string          `json:"relation"`
	Kind               string          `json:"kind"`
	Family             *string         `json:"family,omitempty"`
	Direction          string          `json:"direction"`
	Notes              *string         `json:"notes,omitempty"`
	Legs               []comboLegFacts `json:"legs"`
	WorstLegAgeRatio   *float64        `json:"worst_leg_age_ratio"`
	HarnessGpPer1h     *int64          `json:"harness_gp_per_1h"`
	MarginNow          int64           `json:"margin_now"`
	MarginMedian14d    *int64          `json:"margin_median_14d"`
	MarginDaysPositive *int            `json:"margin_days_positive"`
	MarginDays         *int            `json:"margin_days"`
	OutputChange7dPct  *float64        `json:"output_change_7d_pct"`
}

// processLegFacts is one leg of a recipe per action.
type processLegFacts struct {
	Side  string   `json:"side"`
	Item  *string  `json:"item"`
	Qty   *float64 `json:"qty"`
	Price *int64   `json:"price"`
	AgeS  *int64   `json:"age_s"`
}

// processFacts is a recipe as the model judges it.
type processFacts struct {
	Recipe          string            `json:"recipe"`
	Category        string            `json:"category"`
	Notes           *string           `json:"notes,omitempty"`
	GpHr            *int64            `json:"gp_hr"`
	ActionsPerHour  float64           `json:"actions_per_hour"`
	Throughput      string            `json:"throughput"`
	PlayerRate      *float64          `json:"player_actions_per_hour"`
	LimitRate       *float64          `json:"limit_actions_per_hour"`
	MarketRate      *float64          `json:"market_actions_per_hour"`
	Hours24h        int               `json:"hours_24h"`
	HoursAtFloor24h int               `json:"hours_at_floor_24h"`
	MaxLegAgeS      *int64            `json:"max_leg_age_s"`
	Legs            []processLegFacts `json:"legs"`
}

// packetBuild is what cleared the rules, before admit picks what the model reads. fate
// maps a lane and item to the empty string for a pass, otherwise why the script refused
// it. unread marks a lane whose silence on an item proves nothing.
type packetBuild struct {
	combos   []*comboBuilt // best first, one per output item
	recipes  []recipeBuilt // best paid first
	liveRows []store.PacketRow
	fate     map[packetKey]string
	unread   map[string]bool
}

// refuse records why an item was turned down. A failed lookup outranks a rule: the row
// it hid might have passed.
func (b *packetBuild) refuse(key packetKey, why string) {
	if cur, seen := b.fate[key]; seen && strings.HasPrefix(cur, lookupFailed) {
		return
	}
	b.fate[key] = why
}

type packetKey struct {
	lane string
	item int
}

// startEvaluate builds the packet and starts the run that judges it. Called holding r.mu.
func (r *Runner) startEvaluate(ctx context.Context, p brief.Params, source string) (int64, error) {
	buildCtx, cancel := context.WithTimeout(ctx, packetTimeout)
	defer cancel()
	at := time.Now().UTC()
	signals, err := r.Store.PendingSignals(buildCtx, evaluateKinds, signalBatch)
	if err != nil {
		return 0, err
	}
	build, err := r.buildPacket(buildCtx, p, at)
	if err != nil {
		return 0, fmt.Errorf("packet: %w", err)
	}
	assigned, dismiss := splitSignals(signals, build.fate, build.unread)
	for id, reason := range dismiss {
		if err := r.Store.ResolveSignal(ctx, id, "dismissed", reason); err != nil {
			log.Printf("evaluate: resolve signal %d: %v", id, err)
		}
	}
	candidates, added := r.admit(buildCtx, build, signals)
	assigned = append(assigned, added...)
	if len(candidates) == 0 {
		return 0, ErrNothingToEvaluate
	}
	lines := make([]string, 0, len(candidates))
	for _, c := range candidates {
		raw, err := json.Marshal(c)
		if err != nil {
			return 0, err
		}
		lines = append(lines, string(raw))
	}
	briefText, err := brief.RenderEvaluate(ctx, r.Store, p, at, assigned, lines)
	if err != nil {
		return 0, err
	}
	runID, err := r.launch(ctx, p, source, ModeEvaluate, briefText, assigned, build.liveRows)
	if err != nil {
		return 0, err
	}
	log.Printf("run %d: evaluate: %d candidates, %d signals assigned, %d dismissed by the script (%d byte brief, built in %s)",
		runID, len(candidates), len(assigned), len(dismiss), len(briefText), time.Since(at).Round(time.Millisecond))
	return runID, nil
}

// admit picks what the model reads, best first. A signal already queued always gets a
// look, and anything else needs a new signal of its own, so every candidate owes a
// verdict and a recent dismissal (UpsertCandidateSignal's guards) keeps it out. Without
// this each hourly run re-judges the same list with no memory of refusing it, and each
// is a fresh chance to ship what the last one turned down.
func (r *Runner) admit(ctx context.Context, b *packetBuild, signals []store.Signal) ([]candidate, []store.Signal) {
	queued := map[packetKey]bool{}
	for _, sig := range signals {
		queued[packetKey{laneOfKind(sig.Kind), sig.ItemID}] = true
	}
	var added []store.Signal
	signal := func(kind string, item int, name string, metrics any) bool {
		id, isNew, err := r.Store.UpsertCandidateSignal(ctx, kind, item, name, metrics, r.Cfg.SignalDismissCooldown)
		if err != nil {
			log.Printf("evaluate: signal %s %d: %v", kind, item, err)
			return false
		}
		if isNew {
			added = append(added, store.Signal{SignalID: id, Kind: kind, ItemID: item, ItemName: name})
		}
		return isNew
	}
	var kept []*comboBuilt
	for _, c := range b.combos {
		item := c.row.PrimaryItemID
		if queued[packetKey{"C", item}] || (len(kept) < comboMaxCandidates && signal("combo", item, c.row.PrimaryName, c.row)) {
			kept = append(kept, c)
		}
	}
	r.addMarginHistory(ctx, kept)
	var out []candidate
	for _, c := range kept {
		out = append(out, candidate{Lane: "C", Facts: c.facts, Draft: c.draft})
	}
	for _, rb := range b.recipes {
		item := *rb.row.OutputItem
		if queued[packetKey{"P", item}] || signal("process", item, rb.row.Name, rb.metrics()) {
			out = append(out, rb.cand)
			continue
		}
		rb.row.Ship, rb.row.Why = false, "held out by a recent verdict"
	}
	return out, added
}

// splitSignals sends the signals with a candidate to the model and answers the rest
// from the script's own reason. A signal whose item couldn't be read stays queued.
func splitSignals(signals []store.Signal, fate map[packetKey]string, unread map[string]bool) (assigned []store.Signal, dismiss map[int64]string) {
	dismiss = map[int64]string{}
	for _, sig := range signals {
		lane := laneOfKind(sig.Kind)
		why, known := fate[packetKey{lane, sig.ItemID}]
		switch {
		case strings.HasPrefix(why, lookupFailed), !known && unread[lane]:
			continue
		case !known:
			dismiss[sig.SignalID] = "script: no longer clears the floor at a fillable size"
		case why != "":
			dismiss[sig.SignalID] = "script: " + why
		default:
			assigned = append(assigned, sig)
		}
	}
	return assigned, dismiss
}

func laneOfKind(kind string) string {
	if kind == "process" {
		return "P"
	}
	return "C"
}

// buildPacket judges both lanes. A lane's whole-lane failure fails the build: half a
// packet would dismiss the other lane's signals for no reason. A lane that's switched
// off proves nothing either, so research runs keep its signals, as they always have.
func (r *Runner) buildPacket(ctx context.Context, p brief.Params, at time.Time) (*packetBuild, error) {
	if r.Prices == nil {
		return nil, errors.New("no price source")
	}
	b := &packetBuild{fate: map[packetKey]string{}, unread: map[string]bool{}}
	if w, ok := p.Archetypes["C"]; !ok || w > 0 {
		if err := r.addCombo(ctx, b, p, at); err != nil {
			return nil, fmt.Errorf("lane C: %w", err)
		}
	} else {
		b.unread["C"] = true
	}
	if w, ok := p.Archetypes["P"]; !ok || w > 0 {
		if err := r.addProcess(ctx, b, p, at); err != nil {
			return nil, fmt.Errorf("lane P: %w", err)
		}
	} else {
		b.unread["P"] = true
	}
	return b, nil
}

// recipeBuilt is one recipe that cleared the rules. row points into the packet's rows.
type recipeBuilt struct {
	row  *store.PacketRow
	cand candidate
}

// metrics is the recipe's signal, under the collector's JSON names.
func (rb recipeBuilt) metrics() map[string]any {
	return map[string]any{"recipe_id": rb.row.RecipeID, "name": rb.row.Name, "category": rb.row.Category,
		"output_item": *rb.row.OutputItem, "gp_hr": *rb.row.GpHr}
}

// addProcess turns the lane's existing packet rows into candidates.
func (r *Runner) addProcess(ctx context.Context, b *packetBuild, p brief.Params, at time.Time) error {
	rows, err := r.buildProcessPacket(ctx, p)
	if err != nil {
		return err
	}
	b.liveRows = rows
	for i := range rows {
		row := &rows[i]
		if row.OutputItem == nil {
			// A failed read can't name its output, so any recipe's signal could be this one.
			if strings.HasPrefix(row.Why, lookupFailed) {
				b.unread["P"] = true
			}
			continue
		}
		key := packetKey{"P", *row.OutputItem}
		// Rows arrive best paid first, and a shippable row beats a refused one for its item.
		if why, seen := b.fate[key]; seen && (why == "" || !row.Ship) {
			continue
		}
		b.fate[key] = row.Why
		if !row.Ship {
			continue
		}
		c, err := r.processCandidate(ctx, row, p.CapitalGp, at)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			row.Ship, row.Why = false, lookupFailed+err.Error()
			b.fate[key] = row.Why
			continue
		}
		b.recipes = append(b.recipes, recipeBuilt{row, *c})
	}
	return nil
}

func (r *Runner) processCandidate(ctx context.Context, row *store.PacketRow, budgetGp int64, at time.Time) (*candidate, error) {
	detail, err := r.Store.ProcessDetailByID(ctx, row.RecipeID)
	if err != nil {
		return nil, err
	}
	if detail == nil {
		return nil, fmt.Errorf("recipe %d is gone", row.RecipeID)
	}
	snap, err := r.Prices.ProcessSnapshot(ctx, row.RecipeID)
	if err != nil {
		return nil, err
	}
	names, err := r.Store.ItemNames(ctx, []int{*row.OutputItem})
	if err != nil {
		return nil, err
	}
	name := names[*row.OutputItem]
	if name == "" {
		return nil, fmt.Errorf("item %d has no name", *row.OutputItem)
	}
	raw, err := processDraft(row, snap, detail.Quote.FeeGp, name, budgetGp, at)
	if err != nil {
		return nil, err
	}
	facts := processFacts{
		Recipe: row.Name, Category: row.Category, Notes: detail.Notes, GpHr: row.GpHr,
		ActionsPerHour: float64(*row.UnitsPer4h) / 4,
		Throughput:     snap.Throughput, PlayerRate: snap.ActionsPerHour, LimitRate: snap.LimitRate, MarketRate: snap.MarketRate,
		Hours24h: row.Hours24h, HoursAtFloor24h: row.HoursAtFloor24h, MaxLegAgeS: row.MaxLegAgeS,
	}
	for _, l := range detail.Legs {
		facts.Legs = append(facts.Legs, processLegFacts{Side: l.Side, Item: l.Name, Qty: l.Qty, Price: l.PriceSlow, AgeS: l.SlowAgeS})
	}
	return &candidate{Lane: "P", Facts: facts, Draft: raw}, nil
}

// processDraft fills in what the directive's lane P spec has a strategy copy from
// process_quote's harness numbers. This snapshot is newer than the one the recipe was
// judged on, so a price that moved can break what the agent's validator holds it to, and
// one bad draft rejects the whole report.
func processDraft(row *store.PacketRow, snap *eval.ProcessSnap, feeGp float64, outputName string, budgetGp int64, at time.Time) (json.RawMessage, error) {
	if snap.GrossIn == nil || snap.MarginSlow == nil || row.UnitsPer4h == nil || row.GpHr == nil {
		return nil, errors.New("the recipe lost a price")
	}
	rawEntry := *snap.GrossIn + feeGp
	entry := int64(math.Round(rawEntry))
	exit := int64(math.Round(rawEntry + *snap.MarginSlow))
	units := *row.UnitsPer4h
	if exit <= entry || units*entry > budgetGp {
		return nil, errors.New("the price moved since the recipe was judged")
	}
	recipe := row.RecipeID
	d := strategyDraft{
		ID:              draftID("P", fmt.Sprintf("%s r%d", outputName, recipe), at),
		Archetype:       "P",
		Items:           []draftItem{{Name: outputName, ID: *row.OutputItem}},
		EntryPrice:      entry,
		ExitPrice:       exit,
		CapitalRequired: units * entry,
		Size:            draftSize{BuyLimit: units, VolConstrained: units, UnitsUsed: units},
		ExpectedValue:   draftEV{PerCycleGp: *row.GpHr * 4, Per1hGp: *row.GpHr, PerDayGp: *row.GpHr * 4, RoiPct: roiPct(entry, exit)},
		RecipeID:        &recipe,
	}
	return json.Marshal(d)
}

// addCombo vets each conversion that clears the floor at a fillable size, then keeps the
// biggest per output item.
func (r *Runner) addCombo(ctx context.Context, b *packetBuild, p brief.Params, at time.Time) error {
	rows, err := r.Store.ComboCandidates(ctx, p.CapitalGp, comboScan)
	if err != nil {
		return err
	}
	open, err := r.Store.OpenComboRelations(ctx)
	if err != nil {
		return err
	}
	rec := r.loadRecord(ctx)
	var ships []*comboBuilt
	for _, row := range rows {
		key := packetKey{"C", row.PrimaryItemID}
		if open[row.RelationID] {
			if _, seen := b.fate[key]; !seen {
				b.fate[key] = fmt.Sprintf("relation %d already has an open C strategy", row.RelationID)
			}
			continue
		}
		built, why := r.buildCombo(ctx, row, p, at)
		if why == "" {
			var st store.SidecarStrategy
			if err := json.Unmarshal(built.draft, &st); err != nil {
				return err
			}
			var rel *store.Relation
			reason, proj := r.vetOne(ctx, p, st, rec, &rel)
			why = strings.TrimPrefix(reason, "vetoed at ship time: ")
			built.facts.HarnessGpPer1h = proj
			if why == "" && proj == nil { // every leg is priced by now, so it's the haircut
				why = "no margin survives the harness's slippage"
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if why != "" {
			b.refuse(key, why)
			continue
		}
		built.row = row
		ships = append(ships, built)
	}
	sort.SliceStable(ships, func(i, j int) bool { return ships[i].perCycle > ships[j].perCycle })
	taken := map[int]bool{}
	for _, s := range ships {
		if taken[s.row.PrimaryItemID] {
			continue // the vet's dedup keys on the item, so one conversion per output
		}
		taken[s.row.PrimaryItemID] = true
		b.fate[packetKey{"C", s.row.PrimaryItemID}] = ""
		b.combos = append(b.combos, s)
	}
	return nil
}

// comboBuilt is one priced conversion on its way to a candidate.
type comboBuilt struct {
	row      store.ComboCandidate
	draft    json.RawMessage
	facts    comboFacts
	perCycle int64
	buys     []store.RelationLeg
	sells    []store.RelationLeg
}

// buildCombo prices one direction from the same snapshots the vet projects with, and
// sizes it the way the directive tells a strategy to. The second return is why it can't
// ship: a leg with no price is refused, never counted as free.
func (r *Runner) buildCombo(ctx context.Context, c store.ComboCandidate, p brief.Params, at time.Time) (*comboBuilt, string) {
	rel, err := r.Store.RelationByID(ctx, c.RelationID)
	if err != nil {
		return nil, lookupFailed + err.Error()
	}
	if rel == nil {
		return nil, fmt.Sprintf("relation %d is gone", c.RelationID)
	}
	buys, sells := rel.Inputs, rel.Outputs
	if c.Direction == "reverse" {
		buys, sells = rel.Outputs, rel.Inputs
	}
	ids := make([]int, 0, len(buys)+len(sells))
	for _, l := range append(append([]store.RelationLeg{}, sells...), buys...) {
		ids = append(ids, l.ItemID)
	}
	snaps, err := r.Prices.SnapshotMany(ctx, ids)
	if err != nil {
		return nil, lookupFailed + err.Error()
	}
	limits, err := r.Store.BuyLimits(ctx, ids)
	if err != nil {
		return nil, lookupFailed + err.Error()
	}
	names, err := r.Store.ItemNames(ctx, ids)
	if err != nil {
		return nil, lookupFailed + err.Error()
	}
	notes, err := r.Store.RelationNotes(ctx, c.RelationID)
	if err != nil {
		return nil, lookupFailed + err.Error()
	}
	return sizeCombo(c, buys, sells, snaps, limits, names, notes, p.CapitalGp, at)
}

// sizeCombo is buildCombo's arithmetic, without the database. Sell legs come first in
// items: items[0] is what the dedup, the graveyard and the signals key on.
func sizeCombo(c store.ComboCandidate, buys, sells []store.RelationLeg, snaps map[int]*eval.Snap, limits map[int]int64, names map[int]string, notes *string, budgetGp int64, at time.Time) (*comboBuilt, string) {
	var entry, exit int64
	var legs []store.Leg
	var items []draftItem
	var legFacts []comboLegFacts
	var worst *float64
	vol, bound := int64(-1), int64(-1)
	seen := map[int]bool{}
	addLeg := func(l store.RelationLeg, side string) string {
		if names[l.ItemID] == "" {
			return fmt.Sprintf("item %d has no name", l.ItemID) // the validator rejects a report with one
		}
		snap := snaps[l.ItemID]
		if snap == nil {
			return fmt.Sprintf("%s has no snapshot", names[l.ItemID])
		}
		price, age, trades := snap.High, snap.HighAgeS, snap.HighTicks24h
		if side == "buy" {
			price, age, trades = snap.Low, snap.LowAgeS, snap.LowTicks24h
		}
		if price == nil {
			return fmt.Sprintf("%s has no %s price right now", names[l.ItemID], side)
		}
		// An old high inflates the margin, and the evaluator would call it stale from
		// the first tick. The model can't re-quote, so the script holds the same bar.
		if !eval.LegFresh(age, trades) {
			return fmt.Sprintf("%s's %s quote is stale for how often it trades", names[l.ItemID], side)
		}
		if side == "buy" {
			entry += *price * l.Qty
			if limit := limits[l.ItemID]; limit > 0 && (bound < 0 || limit/l.Qty < bound) {
				bound = limit / l.Qty
			}
		} else {
			exit += (*price - eval.SellTax(*price)) * l.Qty
		}
		if legVol := eval.LegConversions(snap, l.Qty); vol < 0 || legVol < vol {
			vol = legVol
		}
		if ratio := float64(*age) * float64(trades) / 86400; worst == nil || ratio > *worst {
			worst = &ratio
		}
		legs = append(legs, store.Leg{ItemID: l.ItemID, Name: names[l.ItemID], Side: side, Qty: l.Qty, Price: *price})
		if !seen[l.ItemID] {
			seen[l.ItemID] = true
			var limit *int64
			if v, ok := limits[l.ItemID]; ok {
				limit = &v
			}
			items = append(items, draftItem{Name: names[l.ItemID], ID: l.ItemID, BuyLimit: limit})
		}
		legFacts = append(legFacts, comboLegFacts{ItemID: l.ItemID, Vol4h: snap.Vol4h, AgeS: age, Trades24h: trades})
		return ""
	}
	for _, l := range sells {
		if why := addLeg(l, "sell"); why != "" {
			return nil, why
		}
	}
	for _, l := range buys {
		if why := addLeg(l, "buy"); why != "" {
			return nil, why
		}
	}
	margin := exit - entry
	switch {
	case bound < 0:
		return nil, "no buy leg has a known limit"
	case entry <= 0:
		return nil, "no input cost"
	case margin <= 0:
		return nil, "no margin at the latest quotes"
	}
	units := min(bound, vol, budgetGp/entry)
	if units <= 0 {
		return nil, "can't fill one conversion in 4h at 15% of its thinnest leg"
	}
	perCycle := margin * units
	if perCycle < floorCPerCycleGp {
		return nil, fmt.Sprintf("%d gp per 4h batch at a fillable %d conversions, under the C floor of %d", perCycle, units, floorCPerCycleGp)
	}
	relation := c.RelationID
	name := c.Relation
	if c.Direction == "reverse" {
		name += " reverse"
	}
	raw, err := json.Marshal(strategyDraft{
		ID:              draftID("C", fmt.Sprintf("%s r%d", name, relation), at),
		Archetype:       "C",
		Items:           items,
		EntryPrice:      entry,
		ExitPrice:       exit,
		CapitalRequired: units * entry,
		Size:            draftSize{BuyLimit: bound, VolConstrained: vol, UnitsUsed: units},
		ExpectedValue:   draftEV{PerCycleGp: perCycle, Per1hGp: perCycle / 4, PerDayGp: perCycle * 6, RoiPct: roiPct(entry, exit)},
		Legs:            legs,
		RelationID:      &relation,
	})
	if err != nil {
		return nil, err.Error()
	}
	if worst != nil {
		v := math.Round(*worst*100) / 100
		worst = &v
	}
	return &comboBuilt{
		draft: raw, perCycle: perCycle, buys: buys, sells: sells,
		facts: comboFacts{Relation: c.Relation, Kind: c.Kind, Family: c.Family, Direction: c.Direction, Notes: notes,
			Legs: legFacts, WorstLegAgeRatio: worst, MarginNow: margin},
	}, ""
}

// addMarginHistory fills in how each conversion's margin has read over the window. A
// failed read leaves the numbers null: the model judges without them, and says so.
func (r *Runner) addMarginHistory(ctx context.Context, list []*comboBuilt) {
	var ids []int
	for _, b := range list {
		for _, l := range append(append([]store.RelationLeg{}, b.buys...), b.sells...) {
			ids = append(ids, l.ItemID)
		}
	}
	if len(ids) == 0 {
		return
	}
	closes, err := r.Store.DailyCloses(ctx, ids, marginHistoryDays)
	if err != nil {
		log.Printf("evaluate: margin history: %v", err)
		return
	}
	for _, b := range list {
		marginHistory(&b.facts, b.buys, b.sells, closes)
	}
}

// marginHistory reads a conversion's daily margin from its legs' closes. A day counts
// only when every leg printed its side: a missing leg is an unknown margin, not a free one.
func marginHistory(f *comboFacts, buys, sells []store.RelationLeg, closes map[int][]store.DailyClose) {
	byDay := map[time.Time]map[int]store.DailyClose{}
	for _, l := range append(append([]store.RelationLeg{}, buys...), sells...) {
		for _, d := range closes[l.ItemID] {
			if byDay[d.Day] == nil {
				byDay[d.Day] = map[int]store.DailyClose{}
			}
			byDay[d.Day][l.ItemID] = d
		}
	}
	var margins []int64
	for _, day := range byDay {
		if m, ok := dayMargin(day, buys, sells); ok {
			margins = append(margins, m)
		}
	}
	if len(margins) > 0 {
		positive := 0
		for _, m := range margins {
			if m > 0 {
				positive++
			}
		}
		sort.Slice(margins, func(i, j int) bool { return margins[i] < margins[j] })
		median := margins[len(margins)/2]
		days := len(margins)
		f.MarginMedian14d, f.MarginDaysPositive, f.MarginDays = &median, &positive, &days
	}
	if len(sells) > 0 {
		f.OutputChange7dPct = change7d(closes[sells[0].ItemID])
	}
}

func dayMargin(day map[int]store.DailyClose, buys, sells []store.RelationLeg) (int64, bool) {
	var m int64
	for _, l := range buys {
		d, ok := day[l.ItemID]
		if !ok || d.Low == nil {
			return 0, false
		}
		m -= *d.Low * l.Qty
	}
	for _, l := range sells {
		d, ok := day[l.ItemID]
		if !ok || d.High == nil {
			return 0, false
		}
		m += (*d.High - eval.SellTax(*d.High)) * l.Qty
	}
	return m, true
}

// change7d compares the latest high close with the one nearest seven days before it.
func change7d(closes []store.DailyClose) *float64 {
	var last *store.DailyClose
	for i := len(closes) - 1; i >= 0; i-- {
		if closes[i].High != nil {
			last = &closes[i]
			break
		}
	}
	if last == nil {
		return nil
	}
	target := last.Day.AddDate(0, 0, -7)
	var then *store.DailyClose
	for i := range closes {
		d := &closes[i]
		if d.High != nil && !d.Day.After(target) {
			then = d
		}
	}
	if then == nil || *then.High <= 0 {
		return nil
	}
	v := math.Round(float64(*last.High-*then.High)/float64(*then.High)*1000) / 10
	return &v
}

func roiPct(entry, exit int64) float64 {
	return math.Round(float64(exit-entry)/float64(entry)*10000) / 100
}

var draftSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// draftID follows the agent's id rule: <lane>-<item-slug>-<yyyymmdd>. Callers put the
// relation or recipe id in name: the slug drops the "+" between Anti-venom 3<->4 and
// Anti-venom+ 3<->4, and two ships with one id fail the whole run at ingest.
func draftID(lane, name string, at time.Time) string {
	slug := strings.Trim(draftSlugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	return fmt.Sprintf("%s-%s-%s", lane, slug, at.Format("20060102"))
}
