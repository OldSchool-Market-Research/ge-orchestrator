package store

import (
	"context"
	"sort"
	"time"
)

// RegimeSnapshot is one collector sweep's read of the market (migration 019):
// the conditions lane yields may later be conditioned on. Log only.
type RegimeSnapshot struct {
	AsOf                  time.Time `json:"as_of"`
	FlipClearFloor        *int      `json:"flip_clear_floor"`
	FlipMedianGp          *int64    `json:"flip_median_gp"`
	FlipPersistenceMedian *float64  `json:"flip_persistence_median"`
	ComboClearFloor       *int      `json:"combo_clear_floor"`
	ComboMedianGp         *int64    `json:"combo_median_gp"`
	VolumeAnomalies       *int      `json:"volume_anomalies"`
	MarketVolRatio        *float64  `json:"market_vol_ratio"` // last hour's volume / the trailing 7d's hourly mean
}

// RecordRegime derives one sweep's snapshot from the trend rows it just stored.
// A lens with no rows leaves its columns null (nulls are signal).
func (s *Store) RecordRegime(ctx context.Context, asOf time.Time, flipFloor, comboFloor int64, volZMin float64) (*RegimeSnapshot, error) {
	r := RegimeSnapshot{AsOf: asOf}
	err := s.Pool.QueryRow(ctx, `SELECT
		CASE WHEN count(*) FILTER (WHERE lens='vflip') > 0
		  THEN count(*) FILTER (WHERE lens='vflip' AND (metrics->>'gp_cycle')::bigint >= $2) END,
		(percentile_cont(0.5) WITHIN GROUP (ORDER BY (metrics->>'gp_cycle')::bigint)
		  FILTER (WHERE lens='vflip'))::bigint,
		CASE WHEN count(*) FILTER (WHERE lens='combo') > 0
		  THEN count(*) FILTER (WHERE lens='combo' AND (metrics->>'budget_cycle_gp')::bigint >= $3) END,
		(percentile_cont(0.5) WITHIN GROUP (ORDER BY (metrics->>'budget_cycle_gp')::bigint)
		  FILTER (WHERE lens='combo'))::bigint,
		CASE WHEN count(*) FILTER (WHERE lens='volume') > 0
		  THEN count(*) FILTER (WHERE lens='volume' AND abs((metrics->>'z')::float8) >= $4) END
		FROM orchestrator.trend_snapshots
		WHERE as_of=$1 AND lens IN ('vflip','combo','volume')`, asOf, flipFloor, comboFloor, volZMin).
		Scan(&r.FlipClearFloor, &r.FlipMedianGp, &r.ComboClearFloor, &r.ComboMedianGp, &r.VolumeAnomalies)
	if err != nil {
		return nil, err
	}
	persistence, err := s.flipPersistenceMedian(ctx, asOf)
	if err != nil {
		return nil, err
	}
	r.FlipPersistenceMedian = persistence
	err = s.Pool.QueryRow(ctx, `WITH h AS (
			SELECT date_trunc('hour', ts) AS hb,
			       sum(coalesce(high_volume,0)+coalesce(low_volume,0)) AS v
			FROM prices_5m WHERE ts >= now() - interval '7 days' GROUP BY 1
		)
		SELECT ((SELECT sum(coalesce(high_volume,0)+coalesce(low_volume,0))
		         FROM prices_5m WHERE ts >= now() - interval '1 hour')
		        / nullif((SELECT avg(v) FROM h), 0))::float8`).Scan(&r.MarketVolRatio)
	if err != nil {
		return nil, err
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO orchestrator.regime_snapshots
		(as_of, flip_clear_floor, flip_median_gp, flip_persistence_median,
		 combo_clear_floor, combo_median_gp, volume_anomalies, market_vol_ratio)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (as_of) DO NOTHING`,
		r.AsOf, r.FlipClearFloor, r.FlipMedianGp, r.FlipPersistenceMedian,
		r.ComboClearFloor, r.ComboMedianGp, r.VolumeAnomalies, r.MarketVolRatio)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// flipPersistenceMedian is the median, over the sweep's flip rows, of each
// row's margin_persistence_24h against its own margin: are the screen's flips
// standing spreads right now, or spikes?
func (s *Store) flipPersistenceMedian(ctx context.Context, asOf time.Time) (*float64, error) {
	rows, err := s.Pool.Query(ctx, `SELECT item_id, (metrics->>'margin')::bigint
		FROM orchestrator.trend_snapshots WHERE as_of=$1 AND lens='vflip'`, asOf)
	if err != nil {
		return nil, err
	}
	type flip struct {
		itemID int
		margin int64
	}
	var flips []flip
	for rows.Next() {
		var f flip
		if err := rows.Scan(&f.itemID, &f.margin); err != nil {
			rows.Close()
			return nil, err
		}
		flips = append(flips, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var ratios []float64
	for _, f := range flips {
		okHours, _, err := s.FlipPersistence24h(ctx, f.itemID, f.margin)
		if err != nil {
			return nil, err
		}
		ratios = append(ratios, float64(okHours)/24)
	}
	return medianOf(ratios), nil
}

// medianOf returns the median, nil for no values.
func medianOf(v []float64) *float64 {
	if len(v) == 0 {
		return nil
	}
	sorted := append([]float64(nil), v...)
	sort.Float64s(sorted)
	m := sorted[len(sorted)/2]
	if len(sorted)%2 == 0 {
		m = (sorted[len(sorted)/2-1] + m) / 2
	}
	return &m
}

// RegimeHistory returns the trailing-days snapshots, oldest first.
func (s *Store) RegimeHistory(ctx context.Context, days int) ([]RegimeSnapshot, error) {
	rows, err := s.Pool.Query(ctx, `SELECT as_of, flip_clear_floor, flip_median_gp, flip_persistence_median,
		combo_clear_floor, combo_median_gp, volume_anomalies, market_vol_ratio
		FROM orchestrator.regime_snapshots
		WHERE as_of > now() - make_interval(days => $1) ORDER BY as_of`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RegimeSnapshot{}
	for rows.Next() {
		var r RegimeSnapshot
		if err := rows.Scan(&r.AsOf, &r.FlipClearFloor, &r.FlipMedianGp, &r.FlipPersistenceMedian,
			&r.ComboClearFloor, &r.ComboMedianGp, &r.VolumeAnomalies, &r.MarketVolRatio); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
