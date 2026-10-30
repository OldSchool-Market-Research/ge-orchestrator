package eval

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

// Lane P: processing recipes, judged in gp per hour of play. ge-data's views are the
// one place the recipe math lives; what this adds is the same self-impact haircut as
// every other lane. The views' gp_hr assumes all of the market's volume at exact
// prices (Making Anguish Uncut: 3.2M gp/hr there, about 163k here). ge-mcp's
// process_quote mirrors this arithmetic: change them in the same PR train.
const (
	// FloorPGpHr matches the operator's ping bar: what an hour of their time must pay.
	FloorPGpHr = 250_000
	// processKillFraction of the floor is what keeps a recipe alive, as C's half floor.
	processKillFraction = 0.5
)

// ProcessSnap is one recipe's live read. Nil numbers are NULLs kept as NULLs: an
// unpriceable recipe, never a zero margin.
type ProcessSnap struct {
	MarginSlow       *float64
	GrossIn          *float64 // buy legs at the patient price
	GrossOut         *float64 // sell legs at the patient price, before tax
	ActionsPerHour   *float64 // player speed; nil for volume-bound recipes
	LimitRate        *float64 // buy limits
	MarketRate       *float64 // the market's 24h volume
	Throughput       string
	UnknownLimitLegs int64
	Linked           bool // the same conversion as a C relation
}

// ErrRecipeNotFound means no process_recipes row has that id.
var ErrRecipeNotFound = errors.New("no such recipe")

// processHonest is what an hour of play pays after the haircut: slippage both sides,
// at most participation of the market, and the strategy's size per 4h when it has
// one. ok is false when the recipe can't be priced right now.
func (ev *Evaluator) processHonest(s *ProcessSnap, unitsUsed int64) (margin, rate, gpHr float64, ok bool) {
	if s == nil || s.MarginSlow == nil || s.GrossIn == nil || s.GrossOut == nil || s.MarketRate == nil {
		return 0, 0, 0, false
	}
	margin = *s.MarginSlow - ev.slippage()*(*s.GrossIn+*s.GrossOut)
	rate = ev.participation() * *s.MarketRate
	for _, bound := range []*float64{s.ActionsPerHour, s.LimitRate} {
		if bound != nil && *bound < rate {
			rate = *bound
		}
	}
	if unitsUsed > 0 {
		if perHour := float64(unitsUsed) / flipCycleHours; perHour < rate {
			rate = perHour
		}
	}
	return margin, rate, margin * rate, true
}

// ProjectProcessPer1h is the harness's projection of a P strategy at ship time, nil
// when the recipe can't be priced or doesn't earn. It becomes the confirm ratio's
// denominator, like F's and C's. Uses the default haircut, like the other projections.
func ProjectProcessPer1h(s *ProcessSnap, unitsUsed int64) *int64 {
	_, _, gp, ok := (&Evaluator{}).processHonest(s, unitsUsed)
	if !ok || gp <= 0 {
		return nil
	}
	v := int64(gp)
	return &v
}

// computeProcess evaluates a P strategy. Realized is per hour of PLAY, not per hour
// the strategy is open: nobody clicks for 48 hours straight, so it paces the recipe
// against its projection and never adds up as gp (store.PnL leaves P out).
func (ev *Evaluator) computeProcess(ctx context.Context, st store.Strategy) (store.Evaluation, map[string]bool, error) {
	if st.RecipeID == nil {
		return store.Evaluation{}, nil, fmt.Errorf("strategy %s: no recipe_id", st.Sid)
	}
	snap, err := ev.source().ProcessSnapshot(ctx, *st.RecipeID)
	if err != nil {
		return store.Evaluation{}, nil, err
	}
	units := int64(0)
	if st.UnitsUsed != nil {
		units = *st.UnitsUsed
	}
	margin, rate, gpHr, ok := ev.processHonest(snap, units)
	killBar := processKillFraction * FloorPGpHr

	checks := map[string]bool{"legs_priced": ok}
	// A missing leg is a freshness problem, not a floor breach: only a live margin
	// can fail this check, as in computeFlip.
	checks["floor_ok"] = !ok || gpHr >= killBar

	verdict := "healthy"
	switch {
	case !checks["floor_ok"]:
		verdict = "kill_signal"
	case !ok:
		verdict = "degraded"
	}
	e := baseEvaluation(st, ev.now(), nil)
	detail := map[string]any{"kill_bar_gp_hr": killBar}
	if ok {
		realized := int64(gpHr)
		e.RealizedPer1hGp = &realized
		detail["margin_haircut"], detail["actions_per_hour"], detail["gp_hr"] = margin, rate, gpHr
	}
	finishEvaluation(&e, checks, verdict, detail)
	return e, checks, nil
}

const processSnapshotSQL = `
WITH l AS (
  SELECT sum(CASE WHEN side = 'buy' THEN qty * low END)::float8 AS gross_in,
         sum(CASE WHEN side = 'sell' THEN qty * high END)::float8 AS gross_out
  FROM process_legs_now WHERE recipe_id = $1
)
SELECT q.margin_slow::float8, l.gross_in, l.gross_out, q.actions_per_hour::float8,
       q.limit_actions_per_hour::float8, q.market_actions_per_hour::float8,
       q.throughput, q.unknown_limit_legs, r.relation_name IS NOT NULL
FROM process_quotes_now q
JOIN process_recipes r USING (recipe_id)
CROSS JOIN l
WHERE q.recipe_id = $1`

func (p *pgSource) ProcessSnapshot(ctx context.Context, recipeID int) (*ProcessSnap, error) {
	var s ProcessSnap
	err := p.pool.QueryRow(ctx, processSnapshotSQL, recipeID).Scan(&s.MarginSlow, &s.GrossIn, &s.GrossOut,
		&s.ActionsPerHour, &s.LimitRate, &s.MarketRate, &s.Throughput, &s.UnknownLimitLegs, &s.Linked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRecipeNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}
