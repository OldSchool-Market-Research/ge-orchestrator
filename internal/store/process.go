package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Processing recipes (ge-data init/04-06) for the dashboard's Processing page.
// Every margin and rate comes from ge-data's process_quotes_now /
// process_legs_now views and the ingester's hourly process_snapshots, the
// same source as ge-mcp's process_* tools; nothing here re-derives a margin.
// The orchestrator's grant on public is SELECT-only.

// ErrProcessUnavailable means the processing schema hasn't been applied to
// this database yet (it ships as a manual ge-data DDL step).
var ErrProcessUnavailable = errors.New("processing schema (ge-data init/04-06) is not applied")

func processErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return ErrProcessUnavailable
	}
	return err
}

// ProcessQuote is one process_quotes_now row. Pointers are NULLs kept as
// null: a nil margin is an unpriceable leg, not zero.
type ProcessQuote struct {
	RecipeID                int      `json:"recipe_id"`
	Name                    string   `json:"name"`
	Category                string   `json:"category"`
	Throughput              string   `json:"throughput"`
	MarginSlow              *float64 `json:"margin_slow"`
	MarginInsta             *float64 `json:"margin_insta"`
	GpHr                    *float64 `json:"gp_hr"`
	GpHrInsta               *float64 `json:"gp_hr_insta"`
	GpHrTheoretical         *float64 `json:"gp_hr_theoretical"`
	ActionsPerHour          *float64 `json:"actions_per_hour"`
	EffectiveActionsPerHour *float64 `json:"effective_actions_per_hour"`
	LimitActionsPerHour     *float64 `json:"limit_actions_per_hour"`
	MarketActionsPerHour    *float64 `json:"market_actions_per_hour"`
	FeeGp                   float64  `json:"fee_gp"`
	MissingLegs             int64    `json:"missing_legs"`
	MaxLegAgeS              *int64   `json:"max_leg_age_s"`
}

const processQuoteCols = `recipe_id, name, category, throughput, margin_slow, margin_insta, gp_hr,
  gp_hr_insta, gp_hr_theoretical, actions_per_hour, effective_actions_per_hour,
  limit_actions_per_hour, market_actions_per_hour, fee_gp, missing_legs, max_leg_age_s`

func scanProcessQuote(row pgx.Row) (ProcessQuote, error) {
	var q ProcessQuote
	err := row.Scan(&q.RecipeID, &q.Name, &q.Category, &q.Throughput, &q.MarginSlow, &q.MarginInsta,
		&q.GpHr, &q.GpHrInsta, &q.GpHrTheoretical, &q.ActionsPerHour, &q.EffectiveActionsPerHour,
		&q.LimitActionsPerHour, &q.MarketActionsPerHour, &q.FeeGp, &q.MissingLegs, &q.MaxLegAgeS)
	return q, err
}

// ProcessQuotes returns every recipe's live quote, best gp/hr first. The
// universe is a few hundred rows, so the dashboard filters and sorts client-side.
func (s *Store) ProcessQuotes(ctx context.Context) ([]ProcessQuote, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+processQuoteCols+` FROM process_quotes_now
		ORDER BY gp_hr DESC NULLS LAST, recipe_id`)
	if err != nil {
		return nil, processErr(err)
	}
	defer rows.Close()
	out := []ProcessQuote{}
	for rows.Next() {
		q, err := scanProcessQuote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// ProcessLeg is one process_legs_now row: the price a patient (slow) and an
// instant (insta) trader would get on that leg, and the 24h volume on the
// side a patient offer fills against.
type ProcessLeg struct {
	Side          string   `json:"side"`
	ItemID        int      `json:"item_id"`
	Name          *string  `json:"name"`
	Qty           float64  `json:"qty"`
	BuyLimit      *int64   `json:"buy_limit"`
	PriceSlow     *int64   `json:"price_slow"`
	PriceInsta    *int64   `json:"price_insta"`
	AgeS          *int64   `json:"age_s"`
	FillVolume24h *float64 `json:"fill_volume_24h"`
}

// ProcessDetail is one recipe: its curation fields, live quote and legs.
type ProcessDetail struct {
	Quote             ProcessQuote `json:"quote"`
	Source            string       `json:"source"`
	RelationName      *string      `json:"relation_name"`
	RelationDirection *string      `json:"relation_direction"`
	Notes             *string      `json:"notes"`
	Legs              []ProcessLeg `json:"legs"`
}

// ProcessDetailByID returns (nil, nil) for an unknown recipe_id.
func (s *Store) ProcessDetailByID(ctx context.Context, id int) (*ProcessDetail, error) {
	var d ProcessDetail
	err := s.Pool.QueryRow(ctx, `SELECT source, relation_name, relation_direction, notes
		FROM process_recipes WHERE recipe_id = $1`, id).Scan(&d.Source, &d.RelationName, &d.RelationDirection, &d.Notes)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, processErr(err)
	}
	if d.Quote, err = scanProcessQuote(s.Pool.QueryRow(ctx,
		`SELECT `+processQuoteCols+` FROM process_quotes_now WHERE recipe_id = $1`, id)); err != nil {
		return nil, processErr(err)
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT side, item_id, name, qty, buy_limit,
		       CASE side WHEN 'buy' THEN low ELSE high END,
		       CASE side WHEN 'buy' THEN high ELSE low END,
		       extract(epoch FROM now() - CASE side WHEN 'buy' THEN low_time ELSE high_time END)::bigint,
		       CASE side WHEN 'buy' THEN low_vol_24h ELSE high_vol_24h END
		FROM process_legs_now WHERE recipe_id = $1 ORDER BY side, item_id`, id)
	if err != nil {
		return nil, processErr(err)
	}
	defer rows.Close()
	d.Legs = []ProcessLeg{}
	for rows.Next() {
		var l ProcessLeg
		if err := rows.Scan(&l.Side, &l.ItemID, &l.Name, &l.Qty, &l.BuyLimit, &l.PriceSlow, &l.PriceInsta, &l.AgeS, &l.FillVolume24h); err != nil {
			return nil, err
		}
		d.Legs = append(d.Legs, l)
	}
	return &d, rows.Err()
}

// ProcessHistoryPoint is one time bucket of a recipe's snapshots. Averages
// cover priced hours only; PricedHours < Hours marks unpriceable stretches.
type ProcessHistoryPoint struct {
	Bucket                  time.Time `json:"bucket"`
	MarginSlow              *float64  `json:"margin_slow"`
	MarginInsta             *float64  `json:"margin_insta"`
	GpHr                    *float64  `json:"gp_hr"`
	GpHrInsta               *float64  `json:"gp_hr_insta"`
	EffectiveActionsPerHour *float64  `json:"effective_actions_per_hour"`
	Hours                   int64     `json:"hours"`
	PricedHours             int64     `json:"priced_hours"`
}

// ProcessHistory buckets one recipe's snapshots since `since`. bucket is a
// Postgres interval literal from the caller's closed set, bound as a param.
func (s *Store) ProcessHistory(ctx context.Context, id int, since time.Time, bucket string) ([]ProcessHistoryPoint, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT time_bucket($3::interval, ts),
		       round(avg(margin_slow)), round(avg(margin_insta)), round(avg(gp_hr)), round(avg(gp_hr_insta)),
		       round(avg(effective_actions_per_hour), 2), count(*), count(margin_slow)
		FROM process_snapshots WHERE recipe_id = $1 AND ts >= $2
		GROUP BY 1 ORDER BY 1`, id, since, bucket)
	if err != nil {
		return nil, processErr(err)
	}
	defer rows.Close()
	out := []ProcessHistoryPoint{}
	for rows.Next() {
		var p ProcessHistoryPoint
		if err := rows.Scan(&p.Bucket, &p.MarginSlow, &p.MarginInsta, &p.GpHr, &p.GpHrInsta, &p.EffectiveActionsPerHour, &p.Hours, &p.PricedHours); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProcessTopRow is one recipe's consistency over a window of hourly snapshots.
type ProcessTopRow struct {
	RecipeID         int      `json:"recipe_id"`
	Name             string   `json:"name"`
	Category         string   `json:"category"`
	Hours            int64    `json:"hours"`
	PricedHours      int64    `json:"priced_hours"`
	Avg              *float64 `json:"avg"`
	Median           *float64 `json:"median"`
	P25              *float64 `json:"p25"`
	PctHoursPositive *float64 `json:"pct_hours_positive"`
	HoursInTop10     int64    `json:"hours_in_top10"`
	AvgLast24h       *float64 `json:"avg_last_24h"`
}

// processTopMetrics is the closed set of snapshot columns ProcessTop may rank
// on; the column name is spliced into SQL, so it must never come from input
// unchecked.
var processTopMetrics = map[string]bool{"gp_hr": true, "gp_hr_insta": true, "margin_slow": true}

// ProcessTop ranks recipes by the median of metric over hourly snapshots since
// `since`. hours_in_top10 ranks per hour across all recipes.
func (s *Store) ProcessTop(ctx context.Context, metric string, since time.Time, limit int) ([]ProcessTopRow, error) {
	if !processTopMetrics[metric] {
		return nil, errors.New("unknown process metric " + metric)
	}
	rows, err := s.Pool.Query(ctx, `
		WITH s AS (
		  SELECT recipe_id, ts, `+metric+` AS v,
		         rank() OVER (PARTITION BY ts ORDER BY `+metric+` DESC NULLS LAST) AS rk
		  FROM process_snapshots WHERE ts >= $1
		)
		SELECT s.recipe_id, p.name, p.category, count(*), count(s.v),
		       round(avg(s.v)),
		       round(percentile_cont(0.5)  WITHIN GROUP (ORDER BY s.v)::numeric),
		       round(percentile_cont(0.25) WITHIN GROUP (ORDER BY s.v)::numeric),
		       round(100.0 * count(*) FILTER (WHERE s.v > 0) / count(*), 1),
		       count(*) FILTER (WHERE s.rk <= 10 AND s.v IS NOT NULL),
		       round(avg(s.v) FILTER (WHERE s.ts >= now() - interval '24 hours'))
		FROM s JOIN process_recipes p USING (recipe_id)
		GROUP BY s.recipe_id, p.name, p.category
		ORDER BY 7 DESC NULLS LAST, s.recipe_id
		LIMIT $2`, since, limit)
	if err != nil {
		return nil, processErr(err)
	}
	defer rows.Close()
	out := []ProcessTopRow{}
	for rows.Next() {
		var r ProcessTopRow
		if err := rows.Scan(&r.RecipeID, &r.Name, &r.Category, &r.Hours, &r.PricedHours, &r.Avg, &r.Median,
			&r.P25, &r.PctHoursPositive, &r.HoursInTop10, &r.AvgLast24h); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
