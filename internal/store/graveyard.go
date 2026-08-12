package store

import (
	"context"
	"time"
)

// Graveyard (docs/FEEDBACK-LOOP.md §3D): item × archetype pairs the record
// says to stop pitching — repeatedly killed AND net-negative over the
// trailing window. Computed, not stored: the trailing window is the expiry
// mechanism, so there is no table to groom and no stale cooldown to unstick.
// The brief lists these with their cumulative waste; the vetter (phase 2)
// enforces the cooldown from the last kill.
const (
	GraveyardWindowDays = 30
	GraveyardMinKills   = 3
)

type GraveyardRow struct {
	ItemID        int       `json:"item_id"`
	ItemName      string    `json:"item_name"`
	Archetype     string    `json:"archetype"`
	Kills         int       `json:"kills"`
	EstRealizedGp int64     `json:"est_realized_gp"`
	LastKilledAt  time.Time `json:"last_killed_at"`
}

// Graveyard returns the current do-not-pitch set, worst cumulative waste
// first. Realized uses the same estimate as PnL: per-strategy median
// realized_per_1h over its evaluated life × hours in that life.
func (s *Store) Graveyard(ctx context.Context) ([]GraveyardRow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT g.primary_item_id, g.item_name, g.archetype,
			g.kills, g.est_realized::bigint, g.last_killed
		FROM (
			SELECT s.primary_item_id,
			       max(coalesce(s.items->0->>'name', 'unknown item')) AS item_name,
			       s.archetype,
			       count(*) AS kills,
			       sum(coalesce(pos.final_equity_gp,
			           est.med_1h * greatest(extract(epoch from (s.closed_at - coalesce(s.triggered_at, s.opened_at)))/3600.0, 0))) AS est_realized,
			       max(s.closed_at) AS last_killed
			FROM orchestrator.strategies s
			JOIN LATERAL (
				SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY e.realized_per_1h_gp) AS med_1h
				FROM orchestrator.evaluations e
				WHERE e.strategy_id = s.strategy_id
				  AND (s.triggered_at IS NULL OR e.at >= s.triggered_at)
			) est ON true
			LEFT JOIN orchestrator.positions pos ON pos.strategy_id = s.strategy_id
			WHERE s.state = 'killed'
			  AND s.closed_at > now() - make_interval(days => $1)
			GROUP BY s.primary_item_id, s.archetype
		) g
		WHERE g.kills >= $2 AND g.est_realized < 0
		ORDER BY g.est_realized`, GraveyardWindowDays, GraveyardMinKills)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GraveyardRow
	for rows.Next() {
		var r GraveyardRow
		if err := rows.Scan(&r.ItemID, &r.ItemName, &r.Archetype, &r.Kills, &r.EstRealizedGp, &r.LastKilledAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
