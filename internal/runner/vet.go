package runner

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/osrs-ge/ge-orchestrator/internal/brief"
	"github.com/osrs-ge/ge-orchestrator/internal/eval"
	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

// Absolute-gp ship floors (flips-first redesign). These mirror ge-agent's
// validator constants; the vet is the mechanical backstop for a model that
// ships below them anyway.
const (
	floorFPerCycleGp = 400_000
	floorBPerCycleGp = 100_000
	// evSanityFactor caps how far a claimed per-cycle EV may exceed the
	// live recomputation (margin x fillable units) before it is vetoed.
	evSanityFactor = 2
	// persistenceGateMin is the lane-F ship gate on margin_persistence_24h:
	// the claimed post-tax margin must have held (at >= 50% of its width) in
	// at least this share of the last 24 hours. The instantaneous EV-sanity
	// check can't see time — the first fortnight shipped 559 F strategies
	// that all passed it, and their median live margin was at 40% of claim
	// 15 minutes later, 8% at 45. Backtest on those 546 closed ships: this
	// gate flips net realized -14.0M -> +1.44M gp (win rate 36% -> 52%).
	persistenceGateMin = 0.4
	// persistenceGateDenom is fixed at 24 wall-clock hours — an hour where
	// either side didn't trade counts as not-persistent, matching ge-mcp's
	// margin_persistence_24h definition exactly (the number the agent cites
	// must be the number the vetter enforces).
	persistenceGateDenom = 24.0
	// Lane-C no-arb cap (the moon-set incident, 2026-08-14): a GE-clerk set
	// or Bob Barter decant is free and reversible, so players close any real
	// sum-of-parts gap in minutes. A margin that is BOTH above this fraction
	// of the sell side AND above the absolute floor cannot be edge — it is
	// wrong recipe data. The Blood moon set's "5.35M premium" (28% of the
	// set) was exactly the un-bought fourth component, and it booked 197M
	// phantom realized before anyone smelled it. Legit set premiums run
	// 1-3%; cheap-potion decants can exceed 10% only far below the floor.
	noArbCapFraction = 0.10
	noArbCapMinGp    = 500_000
)

// vet applies the ship-time rules a strategy must pass to enter the book.
// The agent is told these rules in its directive; vetting is the mechanical
// backstop for when it ships one anyway. Order matters: the cheapest DB-only
// rules run before the price snapshot.
//
//  0. archetype weight — the brief said 0 ("do not pitch it at all"); a
//     lane the operator turned off does not enter the book on a model whim;
//  1. dedup — the live book already trades this item under this archetype;
//  2. kill pre-breach — the stop is already crossed at ship time (the
//     Dragon-dart failure: entry 1180, kill 1050, market already at 1028);
//  3. capital + floor — each strategy independently fits the research
//     budget (a per-opportunity sizing scale, NOT a shared pool the book
//     drains) and clears its lane's absolute-gp floor;
//  4. EV sanity (F/B) — the claimed per-cycle gp must be within
//     evSanityFactor of a live recomputation from the current margin and
//     fillable size. Projections are the denominator of every scoreboard
//     ratio; an inflated claim poisons the track record.
//  5. persistence (F) — the claimed post-tax margin held >= half its width
//     in >= 40% of the last 24 hours (Store.FlipPersistence24h — the same
//     statistic as ge-mcp's margin_persistence_24h). The one failure mode
//     the instantaneous checks can't see: a spike-top margin that is real
//     for the minute the agent quotes it and gone before the offers fill.
//
// Accepted F strategies also get the harness's own projection attached
// (eval.ProjectFlipPer1h -> strategies.projected_per_1h_gp), which becomes
// the confirm-ratio denominator (migration 009).
//
// Since the calibration release two record-fed rules join the list:
//
//  6. graveyard (docs/FEEDBACK-LOOP.md §3D) — the item×archetype pair has
//     >= 3 kills and net-negative realized in the trailing 30d, and the last
//     kill is inside the cooldown. A different archetype on the same item
//     passes: a materially new signal class is the designed escape hatch.
//  7. calibrated floor (§3A) — factor × claimed per-cycle gp must still
//     clear the lane floor. Runs in log-only mode until
//     GE_ORCH_CAL_VETO_MODE=enforce, so the first week measures how many
//     ships it WOULD kill before it kills any.
//  8. recipe integrity + no-arb (C) — the legs must be exactly the
//     item_relations recipe the strategy cites (every component, either
//     direction when reversible), and a free reversible conversion whose
//     margin clears noArbCapFraction AND noArbCapMinGp is vetoed as wrong
//     recipe data. The eval re-prices legs forever but can never notice a
//     leg that isn't there — this is the only gate that can.
//
// Vet errors fail open (accept + log): a flaky price lookup must not turn
// into a dropped research run. The same applies to the record-fed rules — a
// missing calibration table or graveyard query error just skips that rule.
func (r *Runner) vet(ctx context.Context, p brief.Params, list []store.SidecarStrategy) (accepted []store.SidecarStrategy, vetoed []store.Vetoed) {
	rec := r.loadRecord(ctx)
	for _, st := range list {
		reason, proj := r.vetOne(ctx, p, st, rec)
		if reason != "" {
			vetoed = append(vetoed, store.Vetoed{Strategy: st, Reason: reason})
			continue
		}
		st.HarnessProjectedPer1h = proj
		accepted = append(accepted, st)
	}
	return accepted, vetoed
}

// record is the paper record's contribution to one ingest's vetting: the
// latest calibration factor per archetype and the current graveyard, fetched
// once per run, nil-safe throughout.
type record struct {
	factors map[string]float64
	cal     map[string]store.CalibrationRow
	grave   map[graveKey]store.GraveyardRow
}

// CalRow returns the archetype's latest calibration row, nil when none has
// been computed — the ping gate needs the full row to tell a measured
// factor from the conservative default.
func (rec *record) CalRow(archetype string) *store.CalibrationRow {
	if rec == nil || rec.cal == nil {
		return nil
	}
	if row, ok := rec.cal[archetype]; ok {
		return &row
	}
	return nil
}

type graveKey struct {
	itemID    int
	archetype string
}

// Factor returns the archetype's calibration factor, or 1 (no correction)
// when none has been computed — absence of measurement must not veto.
func (rec *record) Factor(archetype string) float64 {
	if rec == nil || rec.factors == nil {
		return 1
	}
	f, ok := rec.factors[archetype]
	if !ok || f <= 0 {
		return 1
	}
	return f
}

func (r *Runner) loadRecord(ctx context.Context) *record {
	rec := &record{}
	if cal, err := r.Store.CalibrationLatest(ctx); err != nil {
		log.Printf("vet: calibration lookup: %v", err)
	} else {
		rec.factors = make(map[string]float64, len(cal))
		rec.cal = make(map[string]store.CalibrationRow, len(cal))
		for _, row := range cal {
			rec.factors[row.Archetype] = row.Factor
			rec.cal[row.Archetype] = row
		}
	}
	if grave, err := r.Store.Graveyard(ctx); err != nil {
		log.Printf("vet: graveyard lookup: %v", err)
	} else {
		rec.grave = make(map[graveKey]store.GraveyardRow, len(grave))
		for _, g := range grave {
			rec.grave[graveKey{g.ItemID, g.Archetype}] = g
		}
	}
	return rec
}

func (r *Runner) vetOne(ctx context.Context, p brief.Params, st store.SidecarStrategy, rec *record) (string, *int64) {
	if len(st.Items) == 0 {
		return "", nil // InsertStrategies rejects the whole sidecar with a real error
	}
	if w, ok := p.Archetypes[st.Archetype]; ok && w == 0 {
		return fmt.Sprintf("vetoed at ship time: archetype %s has weight 0 in the run brief (do not pitch)", st.Archetype), nil
	}
	itemID := st.PrimaryItemID()

	dup, err := r.Store.HasOpenStrategyForItem(ctx, itemID, st.Archetype)
	if err != nil {
		log.Printf("vet %s: dedup lookup: %v", st.ID, err)
	} else if dup {
		return fmt.Sprintf("vetoed at ship time: item %d already has an open %s strategy", itemID, st.Archetype), nil
	}

	if g, ok := rec.graveEntry(itemID, st.Archetype); ok {
		if reason := graveyardVeto(g, r.Cfg.GraveyardCooldown, time.Now().UTC()); reason != "" {
			return reason, nil
		}
	}

	if st.Archetype == "C" {
		var rel *store.Relation
		lookupOK := true
		if st.RelationID != nil {
			if rel, err = r.Store.RelationByID(ctx, *st.RelationID); err != nil {
				log.Printf("vet %s: relation %d lookup: %v", st.ID, *st.RelationID, err)
				lookupOK = false // fail open, like every record-fed rule
			}
		}
		if lookupOK {
			if reason := comboRecipeVeto(st, rel); reason != "" {
				return reason, nil
			}
		}
	}

	var snap *eval.Snap
	if r.Prices != nil {
		if snap, err = r.Prices.Snapshot(ctx, itemID); err != nil {
			log.Printf("vet %s: snapshot item %d: %v", st.ID, itemID, err)
			snap = nil
		}
	}

	if st.KillPrice != nil {
		if ref := refPrice(snap); ref != nil && stopCrossed(st, *ref) {
			return fmt.Sprintf("vetoed at ship time: kill_price %d already breached (live price %d, entry %d)",
				*st.KillPrice, *ref, st.EntryPrice), nil
		}
	}

	// Per-opportunity capital: every strategy is sized against the full
	// research budget on its own — no shared committed-capital remainder.
	if st.CapitalRequired > p.CapitalGp {
		return fmt.Sprintf("vetoed at ship time: capital_required %d exceeds the %d research budget",
			st.CapitalRequired, p.CapitalGp), nil
	}

	switch st.Archetype {
	case "F":
		if st.ExpectedValue.PerCycleGp < floorFPerCycleGp {
			return fmt.Sprintf("vetoed at ship time: per_cycle_gp %d below the %d lane-F floor",
				st.ExpectedValue.PerCycleGp, floorFPerCycleGp), nil
		}
	case "B":
		if st.ExpectedValue.PerCycleGp < floorBPerCycleGp {
			return fmt.Sprintf("vetoed at ship time: per_cycle_gp %d below the %d lane-B floor",
				st.ExpectedValue.PerCycleGp, floorBPerCycleGp), nil
		}
	}

	if reason := evSanity(st, snap); reason != "" {
		return reason, nil
	}

	if reason := calibratedFloorVeto(st, rec.Factor(st.Archetype)); reason != "" {
		if r.Cfg.CalVetoEnforce {
			return reason, nil
		}
		// Log-only week: count what the rule WOULD kill before letting it.
		log.Printf("vet %s: would veto (cal-floor log-only): %s", st.ID, reason)
	}

	if st.Archetype == "F" {
		// The gate's reference is the strategy's OWN claimed post-tax margin
		// (schema tax rule via eval.SellTax) — the claim being tested, and
		// the same reference the backtest validated.
		refMargin := st.ExitPrice - eval.SellTax(st.ExitPrice) - st.EntryPrice
		if refMargin > 0 {
			okHours, obsHours, err := r.Store.FlipPersistence24h(ctx, itemID, refMargin)
			if err != nil {
				log.Printf("vet %s: persistence item %d: %v", st.ID, itemID, err)
			} else if reason := flipPersistenceVeto(okHours, obsHours, refMargin); reason != "" {
				return reason, nil
			}
		}
	}

	var proj *int64
	if st.Archetype == "F" && snap != nil {
		proj = eval.ProjectFlipPer1h(st.EntryPrice, st.ExitPrice, st.Size.UnitsUsed, snap.Vol30m)
	}
	if st.Archetype == "C" && r.Prices != nil && len(st.Legs) > 0 {
		// C gets a harness projection like F: the ping gate and the confirm
		// ratio should judge against the harness's haircut arithmetic, not
		// the agent's raw claim.
		ids := make([]int, 0, len(st.Legs))
		for _, l := range st.Legs {
			ids = append(ids, l.ItemID)
		}
		if snaps, err := r.Prices.SnapshotMany(ctx, ids); err != nil {
			log.Printf("vet %s: combo snapshots: %v", st.ID, err)
		} else {
			proj = eval.ProjectComboPer1h(st.Legs, st.Size.UnitsUsed, snaps)
		}
	}
	return "", proj
}

// graveEntry looks up the graveyard row for an item×archetype, nil-safe.
func (rec *record) graveEntry(itemID int, archetype string) (store.GraveyardRow, bool) {
	if rec == nil || rec.grave == nil {
		return store.GraveyardRow{}, false
	}
	g, ok := rec.grave[graveKey{itemID, archetype}]
	return g, ok
}

// graveyardVeto rejects a graveyarded item×archetype while its last kill is
// inside the cooldown. Expiry is automatic: past the cooldown the pair may
// be pitched again (and the trailing window eventually drops it entirely).
func graveyardVeto(g store.GraveyardRow, cooldown time.Duration, now time.Time) string {
	if cooldown <= 0 || now.Sub(g.LastKilledAt) >= cooldown {
		return ""
	}
	return fmt.Sprintf("vetoed at ship time: item %d [%s] is graveyarded — %d kills, %d gp realized in the trailing %dd, last killed %s (cooldown %s; a materially new signal class means a different archetype)",
		g.ItemID, g.Archetype, g.Kills, g.EstRealizedGp, store.GraveyardWindowDays,
		g.LastKilledAt.Format("2006-01-02"), cooldown)
}

// calibratedFloorVeto applies the record's correction to the claim before
// the lane floor (docs/FEEDBACK-LOOP.md §3A): factor × claimed per-cycle gp
// must still clear it. factor 1 (no measurement) makes this a no-op beyond
// the raw floor check that already ran.
func calibratedFloorVeto(st store.SidecarStrategy, factor float64) string {
	var floor int64
	switch st.Archetype {
	case "F":
		floor = floorFPerCycleGp
	case "B":
		floor = floorBPerCycleGp
	default:
		return ""
	}
	calibrated := int64(factor * float64(st.ExpectedValue.PerCycleGp))
	if calibrated >= floor {
		return ""
	}
	return fmt.Sprintf("vetoed at ship time: calibrated per_cycle_gp %d (factor %.2f x claimed %d) below the %d lane-%s floor",
		calibrated, factor, st.ExpectedValue.PerCycleGp, floor, st.Archetype)
}

// flipPersistenceVeto applies the lane-F persistence gate. okHours comes from
// Store.FlipPersistence24h; the fixed /24 denominator matches ge-mcp's
// margin_persistence_24h field, so the number the agent cited is the number
// enforced here.
func flipPersistenceVeto(okHours, obsHours int, refMargin int64) string {
	ratio := float64(okHours) / persistenceGateDenom
	if ratio >= persistenceGateMin {
		return ""
	}
	return fmt.Sprintf("vetoed at ship time: margin_persistence_24h %.2f below the %.2f lane-F gate (claimed post-tax margin %d held >= half its width in %d of 24 hours; both sides traded in %d)",
		ratio, persistenceGateMin, refMargin, okHours, obsHours)
}

// comboRecipeVeto applies vet rule 8. rel == nil means the cited recipe does
// not exist (or none was cited). The legs must be the recipe exactly — buy
// the inputs and sell the outputs, or the reverse when the relation is
// reversible — because the evaluator prices whatever legs it is handed: a
// missing component doesn't degrade the margin, it BECOMES the margin.
func comboRecipeVeto(st store.SidecarStrategy, rel *store.Relation) string {
	if st.RelationID == nil {
		return "vetoed at ship time: C strategy cites no relation_id (legs must come from combo_quote)"
	}
	if rel == nil {
		return fmt.Sprintf("vetoed at ship time: relation_id %d not found in item_relations", *st.RelationID)
	}
	buys, sells := legQty(st.Legs, "buy"), legQty(st.Legs, "sell")
	forward := legsMatch(buys, rel.Inputs) && legsMatch(sells, rel.Outputs)
	reverse := rel.Reversible && legsMatch(buys, rel.Outputs) && legsMatch(sells, rel.Inputs)
	if !forward && !reverse {
		dir := "buy the inputs, sell the outputs"
		if rel.Reversible {
			dir += " (or the reverse)"
		}
		return fmt.Sprintf("vetoed at ship time: legs do not match relation %d (%s) — every component must be a leg: %s",
			rel.RelationID, rel.Name, dir)
	}
	// No-arb cap: sets and decants are free, requirement-less NPC exchanges;
	// combines can carry skill gates and fees (notes column) that let real
	// margins persist, so they are exempt.
	if rel.Kind != "combine" && rel.Reversible {
		margin := st.ExitPrice - st.EntryPrice // per conversion, post-tax by contract
		var gross int64
		for _, l := range st.Legs {
			if l.Side == "sell" {
				gross += l.Price * l.Qty
			}
		}
		if margin > noArbCapMinGp && gross > 0 && float64(margin) > noArbCapFraction*float64(gross) {
			return fmt.Sprintf("vetoed at ship time: %d gp margin is %.0f%% of the %d gp sell side on a free reversible %s conversion — a standing gap that size is wrong recipe data, not edge (relation %d, %s)",
				margin, 100*float64(margin)/float64(gross), gross, rel.Kind, rel.RelationID, rel.Name)
		}
	}
	return ""
}

// legQty collapses one side's legs to item_id -> total qty.
func legQty(legs []store.Leg, side string) map[int]int64 {
	m := map[int]int64{}
	for _, l := range legs {
		if l.Side == side {
			m[l.ItemID] += l.Qty
		}
	}
	return m
}

// legsMatch reports whether the legs are exactly the recipe side: same
// items, same per-conversion quantities, nothing missing, nothing extra.
func legsMatch(got map[int]int64, want []store.RelationLeg) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		if got[w.ItemID] != w.Qty {
			return false
		}
	}
	return true
}

// evSanity recomputes a flip strategy's per-cycle ceiling from the live
// snapshot: post-tax margin x min(units_used, 15% participation of a
// 4h-equivalent volume (Vol30m x 8)). A claim more than evSanityFactor above
// that ceiling is vetoed. Fail open when the snapshot is missing a leg or
// volume — freshness problems are the agent's quote-before-ship duty, and
// the paper-trader re-prices everything anyway.
func evSanity(st store.SidecarStrategy, snap *eval.Snap) string {
	if st.Archetype != "F" && st.Archetype != "B" {
		return ""
	}
	if snap == nil || snap.Margin == nil || snap.Vol30m <= 0 || st.Size.UnitsUsed <= 0 {
		return ""
	}
	fillable := int64(float64(snap.Vol30m*8) * 0.15)
	if fillable < 1 {
		fillable = 1
	}
	units := st.Size.UnitsUsed
	if fillable < units {
		units = fillable
	}
	ceiling := *snap.Margin * units
	if ceiling < 0 {
		ceiling = 0
	}
	if st.ExpectedValue.PerCycleGp > ceiling*evSanityFactor {
		return fmt.Sprintf("vetoed at ship time: claimed per_cycle_gp %d exceeds %dx the live recomputation %d (margin %d x %d fillable units)",
			st.ExpectedValue.PerCycleGp, evSanityFactor, ceiling, *snap.Margin, units)
	}
	return ""
}

// refPrice mirrors eval.killBreached's reference choice: the high leg, else
// the low leg.
func refPrice(snap *eval.Snap) *int64 {
	if snap == nil {
		return nil
	}
	if snap.High != nil {
		return snap.High
	}
	return snap.Low
}

// stopCrossed applies the same directional stop rule the evaluator uses: a
// kill above entry means "price rose too far", below means "fell too far".
func stopCrossed(st store.SidecarStrategy, ref int64) bool {
	if *st.KillPrice >= st.EntryPrice {
		return ref >= *st.KillPrice
	}
	return ref <= *st.KillPrice
}
