package runner

import (
	"context"
	"fmt"
	"log"

	"github.com/osrs-ge/ge-orchestrator/internal/brief"
	"github.com/osrs-ge/ge-orchestrator/internal/eval"
	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

// Absolute-gp ship floors (flips-first redesign). These mirror ge-agent's
// validator constants; the vet is the mechanical backstop for a model that
// ships below them anyway.
const (
	floorFPerCycleGp = 200_000
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
// Vet errors fail open (accept + log): a flaky price lookup must not turn
// into a dropped research run.
func (r *Runner) vet(ctx context.Context, p brief.Params, list []store.SidecarStrategy) (accepted []store.SidecarStrategy, vetoed []store.Vetoed) {
	for _, st := range list {
		reason, proj := r.vetOne(ctx, p, st)
		if reason != "" {
			vetoed = append(vetoed, store.Vetoed{Strategy: st, Reason: reason})
			continue
		}
		st.HarnessProjectedPer1h = proj
		accepted = append(accepted, st)
	}
	return accepted, vetoed
}

func (r *Runner) vetOne(ctx context.Context, p brief.Params, st store.SidecarStrategy) (string, *int64) {
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
	return "", proj
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
