package eval

import (
	"context"

	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

// ProjectComboPer1h is the harness's own ship-time projection of a lane-C
// strategy: the same haircut arithmetic computeCombo realizes with (slippage
// per leg, per-leg sell tax, participation-capped conversions, one 4h
// batch), applied to the strategy's own legs at the ship-time snapshots.
// Stored as strategies.projected_per_1h_gp it becomes the confirm-ratio
// denominator and the ping gate's number — C previously fell through to the
// agent's raw claim. Returns nil when any leg is unpriced or the haircut
// margin doesn't survive (a conversion is only as tradeable as its least
// liquid side). Keep the arithmetic in lockstep with computeCombo.
func ProjectComboPer1h(legs []store.Leg, unitsUsed int64, snaps map[int]*Snap) *int64 {
	if len(legs) == 0 || unitsUsed <= 0 {
		return nil
	}
	ev := &Evaluator{}
	var marginHaircut int64
	var minLegConversions *int64
	for _, l := range legs {
		snap := snaps[l.ItemID]
		if snap == nil {
			return nil
		}
		var price *int64
		if l.Side == "buy" {
			price = snap.Low
		} else {
			price = snap.High
		}
		if price == nil {
			return nil
		}
		if l.Side == "buy" {
			marginHaircut -= ev.slipBuy(*price) * l.Qty
		} else {
			sp := ev.slipSell(*price)
			marginHaircut += (sp - sellTax(sp)) * l.Qty
		}
		legConv := ev.legConversions(snap, l.Qty)
		if minLegConversions == nil || legConv < *minLegConversions {
			minLegConversions = &legConv
		}
	}
	capped := unitsUsed
	if minLegConversions != nil && *minLegConversions < capped {
		capped = *minLegConversions
	}
	if marginHaircut <= 0 || capped <= 0 {
		return nil
	}
	v := marginHaircut * capped / 4
	return &v
}

const (
	// legFreshGaps is how many of its own typical trade gaps a leg's quote may age
	// before it counts as stale (ge-mcp combo_quote's worst_leg_age_ratio ~3).
	legFreshGaps = 3
	// legFreshMaxAgeS caps that: a leg printing a few times a day would otherwise
	// pass at any age, and a 24h-old price isn't a live margin.
	legFreshMaxAgeS = 6 * 3600
)

// RecipeMargin prices a recipe at the latest quotes, buying inputs at the low and
// selling outputs at the high after tax. ok is false when a leg has no price.
func RecipeMargin(inputs, outputs []store.RelationLeg, snaps map[int]*Snap) (margin int64, ok bool) {
	for _, l := range inputs {
		snap := snaps[l.ItemID]
		if snap == nil || snap.Low == nil {
			return 0, false
		}
		margin -= *snap.Low * l.Qty
	}
	for _, l := range outputs {
		snap := snaps[l.ItemID]
		if snap == nil || snap.High == nil {
			return 0, false
		}
		margin += (*snap.High - sellTax(*snap.High)) * l.Qty
	}
	return margin, true
}

// legConversions is how many conversions per 4h a leg supports at participation,
// from its real trailing 4h volume. A 30-minute window x 8 reads 0 for any leg that
// didn't print in the last half hour, which rounded conversions to 0 and zeroed
// the realized gp of 59% of combine ticks (35% of decants, 17% of sets) over Oct
// 16-30, on margins that were positive 99% of the time. Backtest on the 176 C closes
// of that fortnight: +299M becomes +462M, and the median pace of combines goes from
// 0.00 to 0.24 (still against the old projections).
func (ev *Evaluator) legConversions(snap *Snap, qty int64) int64 {
	return int64(ev.participation() * float64(snap.Vol4h) / float64(qty))
}

// legFresh judges a quote's age against the leg's own trade cadence, like
// combo_quote and the directive: wall-clock age is not staleness. trades24h counts
// the 5m buckets where that side printed in the last 24h, so a leg printing in every
// bucket keeps a 15-minute bar. The old fixed 15 minutes marked 83% of combine ticks
// (54% of sets) stale, and with 80% healthy ticks needed to confirm, only 3 of 176
// C closes ever did.
func legFresh(ageS *int, trades24h int64) bool {
	if ageS == nil || trades24h <= 0 {
		return false
	}
	allowed := legFreshGaps * 86400 / trades24h
	if allowed > legFreshMaxAgeS {
		allowed = legFreshMaxAgeS
	}
	return int64(*ageS) <= allowed
}

// computeCombo evaluates a C strategy: re-price every leg at the latest
// quotes (one round trip), compose the post-tax conversion margin, and judge
// it against the projected one. The WORST leg governs freshness and volume —
// a conversion is only as tradeable as its least liquid side.
func (ev *Evaluator) computeCombo(ctx context.Context, st store.Strategy) (store.Evaluation, map[string]bool, error) {
	now := ev.now()
	ids := make([]int, 0, len(st.Legs))
	for _, l := range st.Legs {
		ids = append(ids, l.ItemID)
	}
	snaps, err := ev.source().SnapshotMany(ctx, ids)
	if err != nil {
		return store.Evaluation{}, nil, err
	}

	checks := map[string]bool{}
	allFresh, allPriced := true, true
	volOK := true
	var marginNowRaw, marginNowHaircut int64
	var minLegConversions *int64 // conversions/4h the thinnest leg supports at participation
	legDetails := make([]map[string]any, 0, len(st.Legs))

	units := int64(0)
	if st.UnitsUsed != nil {
		units = *st.UnitsUsed
	}

	for _, l := range st.Legs {
		snap := snaps[l.ItemID]
		d := map[string]any{"item_id": l.ItemID, "name": l.Name, "side": l.Side, "qty": l.Qty}
		if snap == nil {
			allPriced, allFresh = false, false
			legDetails = append(legDetails, d)
			continue
		}
		var price *int64
		var ageS *int
		var trades24h int64
		if l.Side == "buy" {
			price, ageS, trades24h = snap.Low, snap.LowAgeS, snap.LowTicks24h
		} else {
			price, ageS, trades24h = snap.High, snap.HighAgeS, snap.HighTicks24h
		}
		d["price"], d["age_s"], d["vol_30m"] = price, ageS, snap.Vol30m
		d["vol_4h"], d["trades_24h"] = snap.Vol4h, trades24h
		legDetails = append(legDetails, d)

		if price == nil {
			allPriced = false
			continue
		}
		if !legFresh(ageS, trades24h) {
			allFresh = false
		}
		if l.Side == "buy" {
			marginNowRaw -= *price * l.Qty
			marginNowHaircut -= ev.slipBuy(*price) * l.Qty
		} else {
			marginNowRaw += (*price - sellTax(*price)) * l.Qty
			sp := ev.slipSell(*price)
			marginNowHaircut += (sp - sellTax(sp)) * l.Qty
		}
		legConv := ev.legConversions(snap, l.Qty)
		if minLegConversions == nil || legConv < *minLegConversions {
			minLegConversions = &legConv
		}
		if units > 0 && snap.Vol4h < units*l.Qty {
			volOK = false
		}
	}

	checks["legs_priced"] = allPriced
	checks["legs_fresh"] = allFresh
	checks["vol_ok"] = volOK

	// The kill bar is absolute, like F's and B's: the live margin at the
	// shipped size must still earn half the lane floor per batch. Measuring
	// against the pitch re-litigates the pitch instead of the position, and
	// the calibrated pitch it replaced was circular: a low factor loosened the
	// kill test, which raised the survival that feeds the factor. Backtest on
	// the 176 C closes of Oct 16-30, alive at 6h: 130 here, 136 under the old
	// bar, 71 at half the raw pitch; sets gain (70 vs 67), decants lose (30
	// vs 36). A strategy with no recorded size can only fail on a non-positive
	// margin.
	projected := st.ExitPrice - st.EntryPrice // per conversion, post-tax by contract
	killBar := comboKillBar(units)
	batchNow := marginNowRaw
	if units > 0 {
		batchNow = marginNowRaw * units
	}
	marginOK := allPriced && float64(batchNow) >= killBar
	checks["margin_ok"] = marginOK

	var realizedPer1h, rawPer1h *int64
	if allPriced {
		capped := units
		if minLegConversions != nil && *minLegConversions < capped {
			capped = *minLegConversions
		}
		raw := marginNowRaw * units / 4
		hc := marginNowHaircut * capped / 4
		rawPer1h, realizedPer1h = &raw, &hc
	}

	verdict := "healthy"
	switch {
	case allPriced && !marginOK:
		verdict = "kill_signal"
	case !allPriced || !allFresh || !volOK:
		verdict = "degraded"
	}

	// Scalar snapshot columns carry the primary (first sell) leg.
	e := baseEvaluation(st, now, snaps[st.PrimaryItemID])
	e.RealizedPer1hGp = realizedPer1h
	finishEvaluation(&e, checks, verdict, map[string]any{
		"legs":             legDetails,
		"combo_margin_now": marginNowRaw, "combo_margin_haircut": marginNowHaircut,
		"projected_margin": projected, "kill_bar_gp": killBar,
		"realized_raw_per_1h": rawPer1h, "realized_haircut_per_1h": realizedPer1h,
	})
	return e, checks, nil
}

// comboKillBar is the batch gp below which a conversion is failing: half the
// lane floor, or a positive margin when the strategy records no size.
func comboKillBar(units int64) float64 {
	if units <= 0 {
		return 1
	}
	return comboKillFloorFraction * floorCBatchGp
}
