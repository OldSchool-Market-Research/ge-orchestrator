package store

import (
	"context"
	"time"
)

// ComboCandidate is one conversion direction from the whole-market sweep, sized by the
// tightest of its buy limits, 15% of its thinnest leg's volume and the budget. The
// collector stores these as trend metrics, so the JSON names are a record.
type ComboCandidate struct {
	RelationID    int     `json:"relation_id"`
	Relation      string  `json:"relation"`
	Kind          string  `json:"kind"`
	Family        *string `json:"family,omitempty"`
	Direction     string  `json:"direction"`
	PrimaryItemID int     `json:"primary_item_id"`
	PrimaryName   string  `json:"primary_item_name"`
	ComboMargin   int64   `json:"combo_margin"`
	InputCost     int64   `json:"input_cost"`
	Conversions4h int64   `json:"conversions_4h"`
	BudgetCycleGp int64   `json:"budget_cycle_gp"`
}

// ComboCandidates ranks every relation, both directions, by budget-sized gp per 4h.
func (s *Store) ComboCandidates(ctx context.Context, budgetGp int64, limit int) ([]ComboCandidate, error) {
	rows, err := s.Pool.Query(ctx, comboSweepSQL, budgetGp, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ComboCandidate
	for rows.Next() {
		var r ComboCandidate
		if err := rows.Scan(&r.RelationID, &r.Relation, &r.Kind, &r.Family, &r.Direction,
			&r.PrimaryItemID, &r.PrimaryName, &r.ComboMargin, &r.InputCost,
			&r.Conversions4h, &r.BudgetCycleGp); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DailyClose is an item's last quote of a UTC day. Nil is a side that didn't trade.
type DailyClose struct {
	Day       time.Time
	High, Low *int64
}

// DailyCloses returns each item's closes over the last days days, oldest first.
func (s *Store) DailyCloses(ctx context.Context, ids []int, days int) (map[int][]DailyClose, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT item_id, time_bucket('1 day', ts) AS day,
		       last(high, ts) FILTER (WHERE high IS NOT NULL),
		       last(low, ts) FILTER (WHERE low IS NOT NULL)
		FROM prices_1m
		WHERE item_id = ANY($1) AND ts >= now() - make_interval(days => $2::int)
		GROUP BY 1, 2 ORDER BY 1, 2`, ids, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int][]DailyClose{}
	for rows.Next() {
		var id int
		var d DailyClose
		if err := rows.Scan(&id, &d.Day, &d.High, &d.Low); err != nil {
			return nil, err
		}
		out[id] = append(out[id], d)
	}
	return out, rows.Err()
}

// OpenComboRelations returns the relations with an open or armed C strategy. Older
// strategies name a buy leg first, so the vet's item dedup can miss them.
func (s *Store) OpenComboRelations(ctx context.Context) (map[int]bool, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT relation_id FROM orchestrator.strategies
		WHERE archetype = 'C' AND state IN ('open','armed') AND relation_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// PendingSignals is the queue's oldest pending signals of the given kinds.
func (s *Store) PendingSignals(ctx context.Context, kinds []string, limit int) ([]Signal, error) {
	return s.collectSignals(ctx, `SELECT signal_id, kind, item_id, item_name, metrics,
		status, run_id, created_at, resolved_at, reason
		FROM orchestrator.signals WHERE status='pending' AND kind = ANY($1)
		ORDER BY created_at LIMIT $2`, kinds, limit)
}

// comboSweepSQL prices each direction at the latest quotes. Nulls stay nulls: an
// unpriced leg drops its relation rather than zero-filling it.
const comboSweepSQL = `
WITH dirs AS (
  SELECT relation_id, name, kind, family, 'forward' AS direction, inputs AS buys, outputs AS sells
  FROM item_relations
  UNION ALL
  SELECT relation_id, name, kind, family, 'reverse', outputs, inputs
  FROM item_relations WHERE reversible
),
legs AS (
  SELECT d.relation_id, d.direction, 'buy' AS side,
         (l->>'item_id')::int AS item_id, (l->>'qty')::bigint AS qty
  FROM dirs d, jsonb_array_elements(d.buys) l
  UNION ALL
  SELECT d.relation_id, d.direction, 'sell',
         (l->>'item_id')::int, (l->>'qty')::bigint
  FROM dirs d, jsonb_array_elements(d.sells) l
),
latest AS (
  SELECT DISTINCT ON (item_id) item_id, high, low
  FROM prices_1m WHERE item_id IN (SELECT DISTINCT item_id FROM legs)
  ORDER BY item_id, ts DESC
),
vol4 AS (
  SELECT item_id, sum(coalesce(high_volume,0)+coalesce(low_volume,0)) AS vol4h
  FROM prices_5m
  WHERE ts >= now() - interval '4 hours'
    AND item_id IN (SELECT DISTINCT item_id FROM legs)
  GROUP BY 1
),
priced AS (
  SELECT lg.relation_id, lg.direction,
         bool_and(CASE WHEN lg.side='buy' THEN l.low IS NOT NULL ELSE l.high IS NOT NULL END) AS all_priced,
         sum(CASE WHEN lg.side='buy' THEN -(l.low * lg.qty)
                  ELSE (l.high - least(l.high/50, 5000000)) * lg.qty END)::bigint AS combo_margin,
         sum(CASE WHEN lg.side='buy' THEN l.low * lg.qty END)::bigint AS input_cost,
         min(CASE WHEN lg.side='buy' AND i.buy_limit > 0 THEN i.buy_limit / lg.qty END)::bigint AS units_bound,
         min((coalesce(v.vol4h,0) * 0.15 / lg.qty))::bigint AS conv_vol_bound
  FROM legs lg
  JOIN items i USING (item_id)
  LEFT JOIN latest l USING (item_id)
  LEFT JOIN vol4 v USING (item_id)
  GROUP BY 1, 2
)
SELECT d.relation_id, d.name, d.kind, d.family, d.direction,
       (d.sells->0->>'item_id')::int AS primary_item_id,
       i.name AS primary_item_name,
       p.combo_margin, p.input_cost,
       least(coalesce(p.units_bound, 0), coalesce(p.conv_vol_bound, 0),
             CASE WHEN p.input_cost > 0 THEN $1::bigint / p.input_cost ELSE 0 END) AS conversions_4h,
       p.combo_margin * least(coalesce(p.units_bound, 0), coalesce(p.conv_vol_bound, 0),
             CASE WHEN p.input_cost > 0 THEN $1::bigint / p.input_cost ELSE 0 END) AS budget_cycle_gp
FROM priced p
JOIN dirs d USING (relation_id, direction)
JOIN items i ON i.item_id = (d.sells->0->>'item_id')::int
WHERE p.all_priced AND p.combo_margin > 0
ORDER BY budget_cycle_gp DESC
LIMIT $2`
