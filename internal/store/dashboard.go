package store

import (
	"context"
	"encoding/json"
	"time"
)

// Read-side aggregates for ge-dashboard: per-strategy evaluation trails and
// verdict counts, item icons, and the strategies each run created. Every
// helper is one query for a whole list (strategy_id/item_id/run_id =
// ANY($1)), never one query per row.

// TrailPoint is one stored evaluation reduced to the fields a sparkline
// needs. Nulls stay null (no quote on a side = no margin, not zero).
//
// CurMargin is the scalar snapshot column, which for C strategies is the
// primary (first sell) item's own spread, not the conversion margin.
// ComboMargin is the C evaluator's raw per-conversion margin
// (detail.combo_margin_now); null for every other kind, and null when not
// every leg was priced (the stored value is then a partial sum, not a
// margin).
type TrailPoint struct {
	At              time.Time `json:"at"`
	CurMargin       *int64    `json:"cur_margin"`
	ComboMargin     *int64    `json:"combo_margin"`
	Verdict         string    `json:"verdict"`
	RealizedPer1hGp *int64    `json:"realized_per_1h_gp"`
}

// EvalSummary is a strategy's evaluation history in brief: the newest
// trailLen evaluations oldest-first, verdict counts over ALL evaluations,
// and the newest evaluation in full.
type EvalSummary struct {
	Trail         []TrailPoint
	VerdictCounts map[string]int
	NEvals        int
	LastEval      *Evaluation
}

func newEvalSummary() *EvalSummary {
	return &EvalSummary{
		Trail:         []TrailPoint{},
		VerdictCounts: map[string]int{"healthy": 0, "degraded": 0, "kill_signal": 0},
	}
}

// The trail is a LATERAL per strategy over the (strategy_id, at) primary key,
// read backwards and stopped after $2 rows, so its cost is bounded by the
// trail length, not by how long a strategy has been evaluated. checks/detail
// are only fetched for the newest row (rn = 1), which becomes last_eval.
const trailSQL = `
SELECT e.strategy_id, e.at, e.cur_high, e.cur_low, e.high_age_s, e.low_age_s,
       e.cur_margin, e.vol_30m, e.realized_per_1h_gp, e.verdict,
       CASE WHEN e.rn = 1 THEN e.checks END, CASE WHEN e.rn = 1 THEN e.detail END,
       e.rn, e.combo_margin
FROM unnest($1::bigint[]) AS ids(id)
CROSS JOIN LATERAL (
  SELECT ev.*, row_number() OVER (ORDER BY ev.at DESC) AS rn,
         CASE WHEN ev.checks->>'legs_priced' = 'false' THEN NULL
              ELSE round((ev.detail->>'combo_margin_now')::numeric)::bigint END AS combo_margin
  FROM orchestrator.evaluations ev
  WHERE ev.strategy_id = ids.id
  ORDER BY ev.at DESC
  LIMIT $2
) e
ORDER BY e.strategy_id, e.at`

// EvalSummaries returns one summary per distinct id, in two queries total.
// Strategies never evaluated get an empty trail, zero counts and no
// last_eval.
func (s *Store) EvalSummaries(ctx context.Context, ids []int64, trailLen int) (map[int64]*EvalSummary, error) {
	out := make(map[int64]*EvalSummary, len(ids))
	uniq := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, seen := out[id]; !seen {
			out[id] = newEvalSummary()
			uniq = append(uniq, id)
		}
	}
	if len(uniq) == 0 {
		return out, nil
	}

	rows, err := s.Pool.Query(ctx, trailSQL, uniq, trailLen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Evaluation
		var rn int64
		var comboMargin *int64
		if err := rows.Scan(&e.StrategyID, &e.At, &e.CurHigh, &e.CurLow, &e.HighAgeS, &e.LowAgeS,
			&e.CurMargin, &e.Vol30m, &e.RealizedPer1hGp, &e.Verdict, &e.Checks, &e.Detail, &rn,
			&comboMargin); err != nil {
			return nil, err
		}
		sum := out[e.StrategyID]
		sum.Trail = append(sum.Trail, TrailPoint{
			At:              e.At,
			CurMargin:       e.CurMargin,
			ComboMargin:     comboMargin,
			Verdict:         e.Verdict,
			RealizedPer1hGp: e.RealizedPer1hGp,
		})
		if rn == 1 {
			last := e
			sum.LastEval = &last
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.Pool.Query(ctx, `SELECT strategy_id, verdict, count(*)
		FROM orchestrator.evaluations WHERE strategy_id = ANY($1)
		GROUP BY strategy_id, verdict`, uniq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var verdict string
		var n int
		if err := rows.Scan(&id, &verdict, &n); err != nil {
			return nil, err
		}
		sum := out[id]
		sum.VerdictCounts[verdict] += n
		sum.NEvals += n
	}
	return out, rows.Err()
}

// ItemIDs returns every distinct item id the strategy references: its items
// list, its legs, and the primary item (always one of those two by
// construction, included for safety). Order: first appearance.
func (st Strategy) ItemIDs() []int {
	var ids []int
	seen := map[int]bool{}
	add := func(id int) {
		if id != 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	var items []struct {
		ID int `json:"id"`
	}
	if len(st.Items) > 0 {
		_ = json.Unmarshal(st.Items, &items) // shape is ours (insertStrategy); a bad row just yields fewer icons
	}
	for _, it := range items {
		add(it.ID)
	}
	for _, l := range st.Legs {
		add(l.ItemID)
	}
	add(st.PrimaryItemID)
	return ids
}

// ItemIcons maps item id to its Wiki icon file name (items.icon) for the
// given ids. Ids with no icon, or not in /mapping, are absent.
func (s *Store) ItemIcons(ctx context.Context, ids []int) (map[int]string, error) {
	out := map[int]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT item_id, icon FROM items
		WHERE item_id = ANY($1) AND icon IS NOT NULL AND icon <> ''`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var icon string
		if err := rows.Scan(&id, &icon); err != nil {
			return nil, err
		}
		out[id] = icon
	}
	return out, rows.Err()
}

// ShippedStrategy is one strategy a run created (any state, vetoed
// included), with its primary item's name and icon from /mapping (null when
// the item is not in the mapping table).
type ShippedStrategy struct {
	StrategyID      int64   `json:"strategy_id"`
	Archetype       string  `json:"archetype"`
	Title           string  `json:"title"`
	State           string  `json:"state"`
	PrimaryItemID   *int    `json:"primary_item_id"`
	PrimaryItemName *string `json:"primary_item_name"`
	Icon            *string `json:"icon"`
}

// ShippedByRun returns each run's strategies ordered by strategy_id, keyed by
// run_id; runs that created nothing are absent from the map.
func (s *Store) ShippedByRun(ctx context.Context, runIDs []int64) (map[int64][]ShippedStrategy, error) {
	out := map[int64][]ShippedStrategy{}
	if len(runIDs) == 0 {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT st.run_id, st.strategy_id, st.archetype, st.title, st.state,
		st.primary_item_id, i.name, i.icon
		FROM orchestrator.strategies st
		LEFT JOIN items i ON i.item_id = st.primary_item_id
		WHERE st.run_id = ANY($1)
		ORDER BY st.run_id, st.strategy_id`, runIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var runID int64
		var r ShippedStrategy
		if err := rows.Scan(&runID, &r.StrategyID, &r.Archetype, &r.Title, &r.State,
			&r.PrimaryItemID, &r.PrimaryItemName, &r.Icon); err != nil {
			return nil, err
		}
		out[runID] = append(out[runID], r)
	}
	return out, rows.Err()
}
