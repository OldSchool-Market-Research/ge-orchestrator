package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Item lookup for the dashboard's item browser (a prices.runescape.wiki-style
// table + per-item pages). Prices and the post-tax flip margin come straight
// from the ingester's rows: prices_1m.margin is computed at ingest
// (high - min(high/50, 5M) - low) on the same row as the high/low shown, so
// nothing here re-derives tax. The only arithmetic is two display ratios
// (ROI, margin x buy limit). Nulls stay null: a null price means nothing
// traded that side in the window, never zero.

// ItemRow is one item with its latest quote (last 14 days) and 24h volume.
type ItemRow struct {
	ItemID      int        `json:"item_id"`
	Name        string     `json:"name"`
	Members     *bool      `json:"members"`
	BuyLimit    *int64     `json:"buy_limit"`
	Icon        *string    `json:"icon"`
	High        *int64     `json:"high"` // latest insta-buy price
	HighTime    *time.Time `json:"high_time"`
	Low         *int64     `json:"low"` // latest insta-sell price
	LowTime     *time.Time `json:"low_time"`
	Margin      *int64     `json:"margin"`       // post-tax flip margin of that row
	ROIPct      *float64   `json:"roi_pct"`      // margin / low
	LimitProfit *int64     `json:"limit_profit"` // margin x buy limit (one 4h window)
	HighVol24h  int64      `json:"high_vol_24h"`
	LowVol24h   int64      `json:"low_vol_24h"`
}

// The latest quote is bounded to 14 days so an item that hasn't traded in a
// long time costs at most two chunk probes (prod: ~170 ms for all ~4.7k
// items); older items show no price rather than a stale one. LATERAL ...
// LIMIT 1 per item, never DISTINCT ON over the item list (the Aug 2026
// combo_screen timeout). Volume defaults to 0: /5m lists only items that
// traded in the block, so no rows in 24h means nothing traded.
const itemRowsSQL = `
WITH vol AS (
  SELECT item_id, sum(high_volume) AS hv, sum(low_volume) AS lv
  FROM prices_5m WHERE ts > now() - interval '24 hours'
  GROUP BY item_id
)
SELECT i.item_id, i.name, i.members, i.buy_limit, i.icon,
       p.high, p.high_time, p.low, p.low_time, p.margin,
       round(100.0 * p.margin / NULLIF(p.low, 0), 2),
       p.margin * i.buy_limit,
       coalesce(v.hv, 0), coalesce(v.lv, 0)
FROM items i
LEFT JOIN LATERAL (
  SELECT high, high_time, low, low_time, margin FROM prices_1m
  WHERE item_id = i.item_id AND ts > now() - interval '14 days'
  ORDER BY ts DESC LIMIT 1
) p ON true
LEFT JOIN vol v ON v.item_id = i.item_id`

func scanItemRow(row pgx.Row) (ItemRow, error) {
	var r ItemRow
	err := row.Scan(&r.ItemID, &r.Name, &r.Members, &r.BuyLimit, &r.Icon,
		&r.High, &r.HighTime, &r.Low, &r.LowTime, &r.Margin, &r.ROIPct, &r.LimitProfit,
		&r.HighVol24h, &r.LowVol24h)
	return r, err
}

// Items returns every item, alphabetically. A few thousand rows: the
// dashboard searches, filters and sorts client-side.
func (s *Store) Items(ctx context.Context) ([]ItemRow, error) {
	rows, err := s.Pool.Query(ctx, itemRowsSQL+` ORDER BY i.name, i.item_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ItemRow, 0, 4800)
	for rows.Next() {
		r, err := scanItemRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ItemRecipe is a processing recipe that uses the item on one side.
type ItemRecipe struct {
	RecipeID int      `json:"recipe_id"`
	Name     string   `json:"name"`
	Category string   `json:"category"`
	Side     string   `json:"side"` // buy = input, sell = output
	GpHr     *float64 `json:"gp_hr"`
}

// ItemDetail is one item's row plus its /mapping metadata and the processing
// recipes it appears in.
type ItemDetail struct {
	ItemRow
	Examine  *string      `json:"examine"`
	Value    *int64       `json:"value"`
	LowAlch  *int64       `json:"lowalch"`
	HighAlch *int64       `json:"highalch"`
	Recipes  []ItemRecipe `json:"recipes"`
}

// ItemByID returns (nil, nil) for an unknown item. The recipe list is empty,
// not an error, while the processing schema isn't applied.
func (s *Store) ItemByID(ctx context.Context, id int) (*ItemDetail, error) {
	var d ItemDetail
	row, err := scanItemRow(s.Pool.QueryRow(ctx, itemRowsSQL+` WHERE i.item_id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.ItemRow = row
	if err := s.Pool.QueryRow(ctx, `SELECT examine, value, lowalch, highalch FROM items WHERE item_id = $1`, id).
		Scan(&d.Examine, &d.Value, &d.LowAlch, &d.HighAlch); err != nil {
		return nil, err
	}

	d.Recipes = []ItemRecipe{}
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT q.recipe_id, q.name, q.category, l.side, q.gp_hr
		FROM process_legs_now l
		JOIN process_quotes_now q USING (recipe_id)
		WHERE l.item_id = $1
		ORDER BY q.gp_hr DESC NULLS LAST, q.recipe_id`, id)
	if err != nil {
		if errors.Is(processErr(err), ErrProcessUnavailable) {
			return &d, nil
		}
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r ItemRecipe
		if err := rows.Scan(&r.RecipeID, &r.Name, &r.Category, &r.Side, &r.GpHr); err != nil {
			return nil, err
		}
		d.Recipes = append(d.Recipes, r)
	}
	if err := rows.Err(); err != nil {
		if errors.Is(processErr(err), ErrProcessUnavailable) {
			d.Recipes = []ItemRecipe{}
			return &d, nil
		}
		return nil, err
	}
	return &d, nil
}

// ItemHistoryPoint is one time bucket of an item's 5-minute blocks:
// volume-weighted average prices (null when nothing traded that side in the
// bucket: a gap, not zero) and summed volumes.
type ItemHistoryPoint struct {
	Bucket  time.Time `json:"bucket"`
	AvgHigh *float64  `json:"avg_high"`
	AvgLow  *float64  `json:"avg_low"`
	HighVol int64     `json:"high_vol"`
	LowVol  int64     `json:"low_vol"`
}

// ItemHistory buckets an item's prices_5m rows since `since`. bucket is an
// interval literal from the caller's closed set, bound as a parameter.
func (s *Store) ItemHistory(ctx context.Context, id int, since time.Time, bucket string) ([]ItemHistoryPoint, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT time_bucket($3::interval, ts),
		       round(sum(avg_high_price * high_volume)::numeric / NULLIF(sum(high_volume) FILTER (WHERE avg_high_price IS NOT NULL), 0)),
		       round(sum(avg_low_price * low_volume)::numeric / NULLIF(sum(low_volume) FILTER (WHERE avg_low_price IS NOT NULL), 0)),
		       coalesce(sum(high_volume), 0), coalesce(sum(low_volume), 0)
		FROM prices_5m
		WHERE item_id = $1 AND ts >= $2
		GROUP BY 1 ORDER BY 1`, id, since, bucket)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ItemHistoryPoint{}
	for rows.Next() {
		var p ItemHistoryPoint
		if err := rows.Scan(&p.Bucket, &p.AvgHigh, &p.AvgLow, &p.HighVol, &p.LowVol); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
