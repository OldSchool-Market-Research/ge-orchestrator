// Package collect is the no-LLM trend collector: on its own ticker it sweeps
// the ENTIRE market with the same SQL lenses the research tools use, persists
// the top rows as trend_snapshots (the durable market-intelligence base —
// pattern history stays queryable after prices move on), and queues signals
// (detections crossing significance) for research runs to investigate.
//
// This is the inversion at the heart of the re-architecture: observation and
// screening are deterministic and continuous; the LLM only interprets. A
// signal is an *assignment of attention*, so consecutive runs cover different
// ground instead of re-deriving the same top-of-scan.
package collect

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/osrs-ge/ge-orchestrator/internal/eval"
	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

type Config struct {
	// Signal thresholds — start conservative, tune from the queue's hit rate.
	VolZMin         float64       // |z| to queue a volume signal (default 8)
	SnapshotTopN    int           // rows persisted per lens per cycle (default 25)
	SignalTTL       time.Duration // pending signals expire after this (default 72h)
	RevalidateAfter time.Duration // watch entries re-queue after this (default 5d)
	// DismissCooldown is dismissal memory (docs/FEEDBACK-LOOP.md §3E): a
	// (kind, item) the agent dismissed is not re-queued for this long — its
	// falsification is presumed to still hold. Applies to detection lenses
	// only; watch revalidation is exempt (re-proving is its whole job).
	// Default 48h; negative disables.
	DismissCooldown time.Duration
	// LaneWeights are the brief's lane weights: a lens whose lane is off
	// (weight 0, like B) keeps its snapshots but queues no signals, since a run
	// could only dismiss them. nil = every lane on.
	LaneWeights map[string]float64

	// Lane F — volume flips: deep-market commodities where post-tax margin x
	// buy limit pays real gp per 4h cycle. Ranked and gated in absolute gp.
	FlipFreshAge   time.Duration // both legs traded within this (default 30m)
	FlipVolMin24h  int64         // 24h volume floor in units (default 100k)
	FlipGpCycleMin int64         // margin x buy_limit floor to queue a signal (default 200k)

	// Lane B — high-value flips: 10M+ items with a fresh two-sided market,
	// ranked by absolute post-tax margin, sized by what the budget affords.
	HighValueMinPrice   int64 // buy-leg price floor (default 10M)
	HighValueVolMin24h  int64 // 24h volume floor in units (default 200)
	HighValueGpCycleMin int64 // margin x affordable units floor to queue (default 100k)

	// Lane C — conversion arbitrage: every item_relations row priced both
	// directions, ranked by budget-sized gp per 4h batch. The floor sits
	// below lane F's on purpose: C is the record's only lifetime-positive
	// lane, its edges are mechanical (GE clerk / Bob Barter / an anvil),
	// and its attention cost is batchy.
	ComboGpCycleMin int64 // budget-sized gp/4h floor to queue a signal (default 150k)

	// ResearchBudgetGp caps lane B's affordable units and lane C's
	// affordable conversions (default 50M). A per-opportunity sizing
	// scale, not a shared pool.
	ResearchBudgetGp int64
}

func (c *Config) defaults() {
	if c.VolZMin == 0 {
		c.VolZMin = 8
	}
	if c.SnapshotTopN == 0 {
		c.SnapshotTopN = 25
	}
	if c.SignalTTL == 0 {
		c.SignalTTL = 72 * time.Hour
	}
	if c.RevalidateAfter == 0 {
		c.RevalidateAfter = 5 * 24 * time.Hour
	}
	if c.DismissCooldown == 0 {
		c.DismissCooldown = 48 * time.Hour
	} else if c.DismissCooldown < 0 {
		c.DismissCooldown = 0
	}
	if c.FlipFreshAge == 0 {
		c.FlipFreshAge = 30 * time.Minute
	}
	if c.FlipVolMin24h == 0 {
		c.FlipVolMin24h = 100_000
	}
	if c.FlipGpCycleMin == 0 {
		c.FlipGpCycleMin = 200_000
	}
	if c.HighValueMinPrice == 0 {
		c.HighValueMinPrice = 10_000_000
	}
	if c.HighValueVolMin24h == 0 {
		c.HighValueVolMin24h = 200
	}
	if c.HighValueGpCycleMin == 0 {
		c.HighValueGpCycleMin = 100_000
	}
	if c.ComboGpCycleMin == 0 {
		c.ComboGpCycleMin = 150_000
	}
	if c.ResearchBudgetGp == 0 {
		c.ResearchBudgetGp = 50_000_000
	}
}

// Ship floors, for the regime snapshot's counts (runner/vet.go; runner imports
// brief and main wires this package, so the numbers are duplicated like the
// other floor copies).
const (
	regimeFlipFloorGp  = 400_000
	regimeComboFloorGp = 200_000
)

type Collector struct {
	Store *store.Store
	Cfg   Config
}

// Cycle runs one full sweep. Returns how many NEW signals were queued (the
// caller uses that to decide whether to trigger a research run).
func (c *Collector) Cycle(ctx context.Context) int {
	c.Cfg.defaults()
	asOf := time.Now().UTC()
	newSignals := 0
	newSignals += c.sweepVolume(ctx, asOf)
	newSignals += c.sweepBand(ctx, asOf) // snapshots only — qualification evidence, no signals
	newSignals += c.sweepFlip(ctx, asOf)
	newSignals += c.sweepHighValue(ctx, asOf)
	newSignals += c.sweepCombo(ctx, asOf)
	newSignals += c.sweepProcess(ctx, asOf)
	// The slow full-history scans (~3 min together) run after the price-sensitive
	// lenses, so they don't delay detection.
	newSignals += c.sweepSeasonal(ctx, asOf)  // snapshots only — timing evidence, no signals
	newSignals += c.sweepVolumeHow(ctx, asOf) // snapshots only — V evidence, no signals
	newSignals += c.sweepWatch(ctx)
	if n, err := c.Store.ExpireStaleSignals(ctx, c.Cfg.SignalTTL); err != nil {
		log.Printf("collect: expire stale: %v", err)
	} else if n > 0 {
		log.Printf("collect: expired %d stale signal(s)", n)
	}
	c.logRecord(ctx, asOf)
	return newSignals
}

// --- volume lens: trailing-7d hourly z-score, whole market ---
// (Same computation as ge-mcp volume_zscore baseline=trailing.)

const volumeSweepSQL = `
WITH cur AS (
  SELECT item_id,
         sum(coalesce(high_volume,0)+coalesce(low_volume,0)) AS cur_vol,
         sum(coalesce(high_volume,0)) AS buys,
         sum(coalesce(low_volume,0)) AS sells
  FROM prices_5m WHERE ts >= now() - interval '1 hour'
  GROUP BY 1
),
hist AS (
  SELECT item_id, date_trunc('hour', ts) AS hb,
         sum(coalesce(high_volume,0)+coalesce(low_volume,0)) AS vol
  FROM prices_5m
  WHERE ts < date_trunc('hour', now()) AND ts >= now() - interval '7 days'
  GROUP BY 1, 2
),
base AS (
  SELECT item_id, avg(vol) AS mean, stddev_samp(vol) AS sd, count(*) AS n
  FROM hist GROUP BY 1
),
px AS (
  SELECT item_id,
         (array_agg((coalesce(avg_high_price,avg_low_price)+coalesce(avg_low_price,avg_high_price))/2.0 ORDER BY ts ASC))[1]  AS p_start,
         (array_agg((coalesce(avg_high_price,avg_low_price)+coalesce(avg_low_price,avg_high_price))/2.0 ORDER BY ts DESC))[1] AS p_end
  FROM prices_5m
  WHERE ts >= now() - interval '1 hour' AND (avg_high_price IS NOT NULL OR avg_low_price IS NOT NULL)
  GROUP BY 1
)
SELECT c.item_id, i.name, c.cur_vol, c.buys, c.sells,
       round(((c.cur_vol - b.mean) / b.sd)::numeric, 2)::float8 AS z,
       b.n,
       round((100*(p.p_end - p.p_start)/nullif(p.p_start,0))::numeric, 2)::float8 AS price_move_pct,
       round(p.p_end)::bigint AS cur_price
FROM cur c
JOIN base b USING (item_id)
JOIN items i USING (item_id)
LEFT JOIN px p USING (item_id)
WHERE b.sd > 0 AND b.n >= 24 AND c.cur_vol >= 100
ORDER BY abs((c.cur_vol - b.mean) / b.sd) DESC
LIMIT $1`

func (c *Collector) sweepVolume(ctx context.Context, asOf time.Time) int {
	rows, err := c.Store.Pool.Query(ctx, volumeSweepSQL, c.Cfg.SnapshotTopN)
	if err != nil {
		log.Printf("collect: volume sweep: %v", err)
		return 0
	}
	defer rows.Close()
	type volRow struct {
		ItemID       int      `json:"item_id"`
		Name         string   `json:"name"`
		CurVol       int64    `json:"cur_vol"`
		Buys         int64    `json:"buys"`
		Sells        int64    `json:"sells"`
		Z            float64  `json:"z"`
		NBaseline    int64    `json:"n_baseline"`
		PriceMovePct *float64 `json:"price_move_pct"`
		CurPrice     *int64   `json:"cur_price"`
	}
	var parsed []volRow
	for rows.Next() {
		var r volRow
		if err := rows.Scan(&r.ItemID, &r.Name, &r.CurVol, &r.Buys, &r.Sells,
			&r.Z, &r.NBaseline, &r.PriceMovePct, &r.CurPrice); err != nil {
			log.Printf("collect: volume scan: %v", err)
			return 0
		}
		parsed = append(parsed, r)
	}
	return persist(c, ctx, asOf, "volume", parsed, func(r volRow) (int, string, bool) {
		return r.ItemID, r.Name, r.Z >= c.Cfg.VolZMin || r.Z <= -c.Cfg.VolZMin
	})
}

// --- same-hour-of-week volume lens: whole market ---
// (Same computation as ge-mcp volume_zscore baseline=same_how, with its default
// gates.) The scan reads every compressed chunk, ~65s: it took 45% of the
// agent's tool time before it moved here, and ran past the CLI's per-call limit.

const volumeHowSweepSQL = `
WITH cur AS (
  SELECT item_id,
         sum(coalesce(high_volume,0)+coalesce(low_volume,0)) AS cur_vol,
         sum(coalesce(high_volume,0)) AS buys,
         sum(coalesce(low_volume,0)) AS sells
  FROM prices_5m WHERE ts >= now() - interval '1 hour'
  GROUP BY 1
),
hist AS (
  SELECT item_id, date_trunc('hour', ts) AS hb,
         sum(coalesce(high_volume,0)+coalesce(low_volume,0)) AS vol
  FROM prices_5m
  WHERE ts < date_trunc('hour', now())
    AND (extract(dow from ts AT TIME ZONE 'utc')*24 + extract(hour from ts AT TIME ZONE 'utc'))::int
      = (extract(dow from now() AT TIME ZONE 'utc')*24 + extract(hour from now() AT TIME ZONE 'utc'))::int
  GROUP BY 1, 2
),
base AS (
  SELECT item_id, avg(vol) AS mean, stddev_samp(vol) AS sd, count(*) AS n
  FROM hist GROUP BY 1
),
px AS (
  SELECT item_id,
         (array_agg((coalesce(avg_high_price,avg_low_price)+coalesce(avg_low_price,avg_high_price))/2.0 ORDER BY ts ASC))[1]  AS p_start,
         (array_agg((coalesce(avg_high_price,avg_low_price)+coalesce(avg_low_price,avg_high_price))/2.0 ORDER BY ts DESC))[1] AS p_end
  FROM prices_5m
  WHERE ts >= now() - interval '1 hour' AND (avg_high_price IS NOT NULL OR avg_low_price IS NOT NULL)
  GROUP BY 1
)
SELECT c.item_id, i.name, c.cur_vol, round(b.mean)::bigint, c.buys, c.sells,
       round(((c.cur_vol - b.mean) / b.sd)::numeric, 2)::float8 AS z,
       b.n,
       round((100*(p.p_end - p.p_start)/nullif(p.p_start,0))::numeric, 2)::float8 AS price_move_pct,
       round(p.p_end)::bigint AS cur_price
FROM cur c
JOIN base b USING (item_id)
JOIN items i USING (item_id)
LEFT JOIN px p USING (item_id)
WHERE b.sd > 0 AND b.n >= 3 AND c.cur_vol >= 100
ORDER BY abs((c.cur_vol - b.mean) / b.sd) DESC
LIMIT $1`

func (c *Collector) sweepVolumeHow(ctx context.Context, asOf time.Time) int {
	// Full-history scan: run uncapped, like the seasonal sweep.
	tx, err := c.Store.Pool.Begin(ctx)
	if err != nil {
		log.Printf("collect: volume_how begin: %v", err)
		return 0
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
		log.Printf("collect: volume_how timeout off: %v", err)
		return 0
	}
	rows, err := tx.Query(ctx, volumeHowSweepSQL, c.Cfg.SnapshotTopN)
	if err != nil {
		log.Printf("collect: volume_how sweep: %v", err)
		return 0
	}
	type volHowRow struct {
		ItemID       int      `json:"item_id"`
		Name         string   `json:"name"`
		CurVol       int64    `json:"cur_vol"`
		BaselineMean int64    `json:"baseline_mean"`
		Buys         int64    `json:"buys"`
		Sells        int64    `json:"sells"`
		Z            float64  `json:"z"`
		NBaseline    int64    `json:"n_baseline"`
		PriceMovePct *float64 `json:"price_move_pct"`
		CurPrice     *int64   `json:"cur_price"`
	}
	var parsed []volHowRow
	for rows.Next() {
		var r volHowRow
		if err := rows.Scan(&r.ItemID, &r.Name, &r.CurVol, &r.BaselineMean, &r.Buys, &r.Sells,
			&r.Z, &r.NBaseline, &r.PriceMovePct, &r.CurPrice); err != nil {
			rows.Close()
			log.Printf("collect: volume_how scan: %v", err)
			return 0
		}
		parsed = append(parsed, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		log.Printf("collect: volume_how rows: %v", err)
		return 0
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		log.Printf("collect: volume_how commit: %v", err)
		return 0
	}
	// Evidence for the V lane, read from the brief. Signals stay on the trailing
	// lens, whose baseline has ~168 samples to this one's ~17.
	return persist(c, ctx, asOf, "volume_how", parsed, func(r volHowRow) (int, string, bool) {
		return r.ItemID, r.Name, false
	})
}

// logRecord appends this sweep's regime snapshot and the lane allocation.
// Neither reaches a run yet, so a failure is only logged.
func (c *Collector) logRecord(ctx context.Context, asOf time.Time) {
	if _, err := c.Store.RecordRegime(ctx, asOf, regimeFlipFloorGp, regimeComboFloorGp, c.Cfg.VolZMin); err != nil {
		log.Printf("collect: regime: %v", err)
	}
	if c.Cfg.LaneWeights == nil {
		return
	}
	a, err := c.Store.CompileAllocation(ctx, c.Cfg.LaneWeights)
	if err != nil {
		log.Printf("collect: allocation: %v", err)
		return
	}
	log.Printf("collect: allocation (log only, not applied): weights %v", a.Weights)
}

// --- seasonal lens: hour-of-week amplitude, whole market ---
// (Same computation as ge-mcp seasonal_scan; gates match its defaults.)

const seasonalSweepSQL = `
WITH raw5 AS (
  SELECT item_id,
         extract(dow from ts AT TIME ZONE 'utc')::int AS d,
         extract(hour from ts AT TIME ZONE 'utc')::int AS h,
         sum((coalesce(avg_high_price, avg_low_price) + coalesce(avg_low_price, avg_high_price)) / 2.0) AS sum_mid,
         count(*) AS n_mid,
         sum(coalesce(high_volume,0) + coalesce(low_volume,0)) AS vol
  FROM prices_5m
  WHERE avg_high_price IS NOT NULL OR avg_low_price IS NOT NULL
  GROUP BY 1, 2, 3
),
item_stats AS (
  SELECT item_id, sum(sum_mid)/sum(n_mid) AS mean_mid, sum(vol) AS tot_vol, sum(n_mid) AS tot_n
  FROM raw5 GROUP BY 1
),
pooled AS (
  SELECT r.item_id, (r.d*24 + r.h)::int AS b,
         sum(p.sum_mid)/nullif(sum(p.n_mid),0) AS mid, sum(p.n_mid) AS obs
  FROM raw5 r
  JOIN raw5 p ON p.item_id = r.item_id AND p.d = r.d
             AND (p.h = r.h OR p.h = (r.h+1)%24 OR p.h = (r.h+23)%24)
  GROUP BY 1, 2, r.d, r.h
),
gated AS (
  SELECT p.item_id, p.b, p.mid / s.mean_mid AS idx, p.obs
  FROM pooled p JOIN item_stats s USING (item_id)
  WHERE s.tot_vol::numeric / s.tot_n >= 500 AND s.mean_mid >= 250 AND p.obs >= 9
),
agg AS (
  SELECT item_id,
         min(idx) AS cheap_idx, max(idx) AS dear_idx,
         (array_agg(b ORDER BY idx ASC))[1]  AS cheap_bucket,
         (array_agg(b ORDER BY idx DESC))[1] AS dear_bucket,
         min(obs) AS min_bucket_obs
  FROM gated GROUP BY item_id HAVING count(*) = 168
)
SELECT a.item_id, i.name,
       round((a.dear_idx - a.cheap_idx)*100, 2)::float8 AS amplitude_pct,
       a.cheap_bucket, a.dear_bucket, a.min_bucket_obs,
       round(s.tot_vol::numeric / s.tot_n)::bigint AS avg_vol5m,
       round(s.mean_mid::numeric)::bigint AS mean_price
FROM agg a JOIN item_stats s USING (item_id) JOIN items i USING (item_id)
ORDER BY amplitude_pct DESC
LIMIT $1`

func (c *Collector) sweepSeasonal(ctx context.Context, asOf time.Time) int {
	// Full-history scan (~12s): run uncapped.
	tx, err := c.Store.Pool.Begin(ctx)
	if err != nil {
		log.Printf("collect: seasonal begin: %v", err)
		return 0
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
		log.Printf("collect: seasonal timeout off: %v", err)
		return 0
	}
	rows, err := tx.Query(ctx, seasonalSweepSQL, c.Cfg.SnapshotTopN)
	if err != nil {
		log.Printf("collect: seasonal sweep: %v", err)
		return 0
	}
	type seaRow struct {
		ItemID       int     `json:"item_id"`
		Name         string  `json:"name"`
		AmplitudePct float64 `json:"amplitude_pct"`
		CheapBucket  int     `json:"cheap_bucket"`
		DearBucket   int     `json:"dear_bucket"`
		MinBucketObs int64   `json:"min_bucket_obs"`
		AvgVol5m     int64   `json:"avg_vol5m"`
		MeanPrice    int64   `json:"mean_price"`
	}
	var parsed []seaRow
	for rows.Next() {
		var r seaRow
		if err := rows.Scan(&r.ItemID, &r.Name, &r.AmplitudePct, &r.CheapBucket,
			&r.DearBucket, &r.MinBucketObs, &r.AvgVol5m, &r.MeanPrice); err != nil {
			rows.Close()
			log.Printf("collect: seasonal scan: %v", err)
			return 0
		}
		parsed = append(parsed, r)
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		log.Printf("collect: seasonal commit: %v", err)
		return 0
	}
	// Flips-first redesign: seasonal structure is timing evidence for the
	// flip lanes, not a strategy source — persist snapshots, queue nothing.
	return persist(c, ctx, asOf, "seasonal", parsed, func(r seaRow) (int, string, bool) {
		return r.ItemID, r.Name, false
	})
}

// --- band lens: current price vs 21d range (archetype-H entries) ---

const bandSweepSQL = `
WITH series AS (
  SELECT item_id,
         (coalesce(avg_high_price,avg_low_price)+coalesce(avg_low_price,avg_high_price))/2.0 AS mid,
         ts,
         coalesce(high_volume,0)+coalesce(low_volume,0) AS vol
  FROM prices_5m
  WHERE ts >= now() - interval '21 days'
    AND (avg_high_price IS NOT NULL OR avg_low_price IS NOT NULL)
),
agg AS (
  SELECT item_id, min(mid) AS lo, max(mid) AS hi,
         (array_agg(mid ORDER BY ts DESC))[1] AS cur,
         sum(vol) AS vol, count(*) AS obs
  FROM series GROUP BY 1
  HAVING count(*) >= 500 AND sum(vol) >= 100000 AND min(mid) >= 100
)
SELECT a.item_id, i.name,
       round(((a.cur - a.lo) / nullif(a.hi - a.lo, 0))::numeric, 3)::float8 AS range_pos,
       round((100*(a.hi - a.lo)/nullif(a.lo,0))::numeric, 1)::float8 AS width_pct,
       round(a.cur)::bigint AS cur_price, round(a.lo)::bigint AS range_low,
       round(a.hi)::bigint AS range_high, a.obs
FROM agg a JOIN items i USING (item_id)
WHERE a.hi > a.lo
ORDER BY ((a.cur - a.lo) / nullif(a.hi - a.lo, 0)) ASC
LIMIT $1`

func (c *Collector) sweepBand(ctx context.Context, asOf time.Time) int {
	rows, err := c.Store.Pool.Query(ctx, bandSweepSQL, c.Cfg.SnapshotTopN)
	if err != nil {
		log.Printf("collect: band sweep: %v", err)
		return 0
	}
	defer rows.Close()
	type bandRow struct {
		ItemID    int     `json:"item_id"`
		Name      string  `json:"name"`
		RangePos  float64 `json:"range_pos"`
		WidthPct  float64 `json:"width_pct"`
		CurPrice  int64   `json:"cur_price"`
		RangeLow  int64   `json:"range_low"`
		RangeHigh int64   `json:"range_high"`
		Obs       int64   `json:"obs"`
	}
	var parsed []bandRow
	for rows.Next() {
		var r bandRow
		if err := rows.Scan(&r.ItemID, &r.Name, &r.RangePos, &r.WidthPct,
			&r.CurPrice, &r.RangeLow, &r.RangeHigh, &r.Obs); err != nil {
			log.Printf("collect: band scan: %v", err)
			return 0
		}
		parsed = append(parsed, r)
	}
	// Flips-first redesign: band position is lane-B qualification evidence,
	// not a strategy source — persist snapshots, queue nothing.
	return persist(c, ctx, asOf, "band", parsed, func(r bandRow) (int, string, bool) {
		return r.ItemID, r.Name, false
	})
}

// --- lane F: volume flips ---
// Both legs traded within FlipFreshAge (a margin between a stale high and a
// fresh low was never simultaneously real), 24h volume >= FlipVolMin24h
// (deep commodity markets only), ranked by gp_cycle = post-tax margin x
// buy_limit (prices_1m.margin — already tax-adjusted, never recomputed).
// The signal gate is the same number: absolute gp per 4h cycle, no ratios.

const flipSweepSQL = `
WITH latest AS (
  SELECT DISTINCT ON (item_id) item_id, high, low, margin, high_time, low_time
  FROM prices_1m WHERE ts >= now() - interval '2 hours'
  ORDER BY item_id, ts DESC
),
vol AS (
  SELECT item_id, sum(coalesce(high_volume,0)+coalesce(low_volume,0)) AS vol24h
  FROM prices_5m WHERE ts >= now() - interval '24 hours' GROUP BY 1
)
SELECT l.item_id, i.name, l.high, l.low, l.margin,
       i.buy_limit,
       l.margin * i.buy_limit AS gp_cycle,
       extract(epoch from (now()-l.high_time))::int AS high_age_s,
       extract(epoch from (now()-l.low_time))::int  AS low_age_s,
       v.vol24h
FROM latest l JOIN vol v USING (item_id) JOIN items i USING (item_id)
WHERE l.margin IS NOT NULL AND l.margin > 0
  AND i.buy_limit > 0
  AND l.high_time >= now() - $1::interval
  AND l.low_time  >= now() - $1::interval
  AND v.vol24h >= $2
ORDER BY l.margin * i.buy_limit DESC
LIMIT $3`

func (c *Collector) sweepFlip(ctx context.Context, asOf time.Time) int {
	rows, err := c.Store.Pool.Query(ctx, flipSweepSQL,
		c.Cfg.FlipFreshAge.String(), c.Cfg.FlipVolMin24h, c.Cfg.SnapshotTopN)
	if err != nil {
		log.Printf("collect: flip sweep: %v", err)
		return 0
	}
	defer rows.Close()
	type flipRow struct {
		ItemID   int    `json:"item_id"`
		Name     string `json:"name"`
		High     int64  `json:"high"`
		Low      int64  `json:"low"`
		Margin   int64  `json:"margin"`
		BuyLimit int64  `json:"buy_limit"`
		GpCycle  int64  `json:"gp_cycle"`
		HighAgeS int    `json:"high_age_s"`
		LowAgeS  int    `json:"low_age_s"`
		Vol24h   int64  `json:"vol24h"`
	}
	var parsed []flipRow
	for rows.Next() {
		var r flipRow
		if err := rows.Scan(&r.ItemID, &r.Name, &r.High, &r.Low, &r.Margin,
			&r.BuyLimit, &r.GpCycle, &r.HighAgeS, &r.LowAgeS, &r.Vol24h); err != nil {
			log.Printf("collect: flip scan: %v", err)
			return 0
		}
		parsed = append(parsed, r)
	}
	return persist(c, ctx, asOf, "vflip", parsed, func(r flipRow) (int, string, bool) {
		return r.ItemID, r.Name, r.GpCycle >= c.Cfg.FlipGpCycleMin
	})
}

// --- lane B: high-value flips ---
// 10M+ items with a fresh two-sided market. Ranked by absolute post-tax
// margin; gp_cycle = margin x the units the research budget affords
// (bounded by buy_limit). Items the budget cannot buy one unit of are out.

const highValueSweepSQL = `
WITH latest AS (
  SELECT DISTINCT ON (item_id) item_id, high, low, margin, high_time, low_time
  FROM prices_1m WHERE ts >= now() - interval '2 hours'
  ORDER BY item_id, ts DESC
),
vol AS (
  SELECT item_id, sum(coalesce(high_volume,0)+coalesce(low_volume,0)) AS vol24h
  FROM prices_5m WHERE ts >= now() - interval '24 hours' GROUP BY 1
)
SELECT l.item_id, i.name, l.high, l.low, l.margin,
       i.buy_limit,
       least(greatest(i.buy_limit,1), $4::bigint / l.low) AS units_affordable,
       l.margin * least(greatest(i.buy_limit,1), $4::bigint / l.low) AS gp_cycle,
       extract(epoch from (now()-l.high_time))::int AS high_age_s,
       extract(epoch from (now()-l.low_time))::int  AS low_age_s,
       v.vol24h
FROM latest l JOIN vol v USING (item_id) JOIN items i USING (item_id)
WHERE l.margin IS NOT NULL AND l.margin > 0
  AND l.low >= $3
  AND l.low <= $4
  AND l.high_time >= now() - $1::interval
  AND l.low_time  >= now() - $1::interval
  AND v.vol24h >= $2
ORDER BY l.margin DESC
LIMIT $5`

func (c *Collector) sweepHighValue(ctx context.Context, asOf time.Time) int {
	rows, err := c.Store.Pool.Query(ctx, highValueSweepSQL,
		c.Cfg.FlipFreshAge.String(), c.Cfg.HighValueVolMin24h,
		c.Cfg.HighValueMinPrice, c.Cfg.ResearchBudgetGp, c.Cfg.SnapshotTopN)
	if err != nil {
		log.Printf("collect: high-value sweep: %v", err)
		return 0
	}
	defer rows.Close()
	type hvRow struct {
		ItemID          int    `json:"item_id"`
		Name            string `json:"name"`
		High            int64  `json:"high"`
		Low             int64  `json:"low"`
		Margin          int64  `json:"margin"`
		BuyLimit        *int64 `json:"buy_limit"` // nil: unknown; the sweep sizes it as one unit
		UnitsAffordable int64  `json:"units_affordable"`
		GpCycle         int64  `json:"gp_cycle"`
		HighAgeS        int    `json:"high_age_s"`
		LowAgeS         int    `json:"low_age_s"`
		Vol24h          int64  `json:"vol24h"`
	}
	var parsed []hvRow
	for rows.Next() {
		var r hvRow
		if err := rows.Scan(&r.ItemID, &r.Name, &r.High, &r.Low, &r.Margin,
			&r.BuyLimit, &r.UnitsAffordable, &r.GpCycle, &r.HighAgeS, &r.LowAgeS, &r.Vol24h); err != nil {
			log.Printf("collect: high-value scan: %v", err)
			return 0
		}
		parsed = append(parsed, r)
	}
	return persist(c, ctx, asOf, "hvflip", parsed, func(r hvRow) (int, string, bool) {
		return r.ItemID, r.Name, r.GpCycle >= c.Cfg.HighValueGpCycleMin
	})
}

// --- lane C: conversion arbitrage ---
// Every item_relations row priced end-to-end both directions in one pass:
// buy legs at latest low, sell legs at latest high minus the per-leg GE tax
// (least(high/50, 5M) — the ingest margin formula applied to a sell leg;
// the stored single-item margin never applies to multi-leg conversions).
// Conversions/4h are bounded by the tightest buy leg's buy_limit, by ~15%
// participation of the tightest leg's 4h volume, and by what the research
// budget affords; budget_cycle_gp = combo_margin x that bound is the same
// absolute-gp gate the other lanes use. A relation with any unpriced leg is
// not a candidate this cycle (nulls are signal — sum() would silently skip
// a null leg, so all_priced gates the whole row). The signal's item is the
// direction's primary output, so forward and reverse of one relation queue
// under different items and dedup does not collapse them.

func (c *Collector) sweepCombo(ctx context.Context, asOf time.Time) int {
	parsed, err := c.Store.ComboCandidates(ctx, c.Cfg.ResearchBudgetGp, c.Cfg.SnapshotTopN)
	if err != nil {
		log.Printf("collect: combo sweep: %v", err)
		return 0
	}
	return persist(c, ctx, asOf, "combo", parsed, func(r store.ComboCandidate) (int, string, bool) {
		return r.PrimaryItemID, r.PrimaryName, r.BudgetCycleGp >= c.Cfg.ComboGpCycleMin
	})
}

// --- lane P: processing recipes ---
// Every recipe that can be sized, ranked by what an hour of play pays at the
// harness's haircut (eval.processHonest: 0.5% slippage both sides, 15% of the market,
// the research budget). Not the view's gp_hr, which assumes all of the market's
// volume. A recipe linked to a C relation is lane C's. A signal also needs the
// persistence the vetter requires, so a run isn't sent after a recipe it can't ship.

const processSweepSQL = `
WITH legs AS (
  SELECT recipe_id,
         sum(CASE WHEN side = 'buy' THEN qty * low END) AS gross_in,
         sum(CASE WHEN side = 'sell' THEN qty * high END) AS gross_out,
         (array_agg(item_id ORDER BY qty DESC, item_id) FILTER (WHERE side = 'sell'))[1] AS output_item
  FROM process_legs_now GROUP BY 1
),
honest AS (
  SELECT q.recipe_id, q.name, q.category, q.throughput, l.output_item,
         round(q.gp_hr)::bigint AS view_gp_hr,
         q.margin_slow - 0.005 * (l.gross_in + l.gross_out) AS margin,
         floor(least(coalesce(q.actions_per_hour, 1e12), coalesce(q.limit_actions_per_hour, 1e12),
                     0.15 * q.market_actions_per_hour, $1::numeric / nullif(l.gross_in, 0) / 4) * 4) / 4 AS rate
  FROM process_quotes_now q
  JOIN process_recipes r USING (recipe_id)
  JOIN legs l USING (recipe_id)
  WHERE r.relation_name IS NULL AND q.throughput <> 'unknown' AND q.unknown_limit_legs = 0
    AND q.margin_slow IS NOT NULL AND l.output_item IS NOT NULL
),
snap AS (
  SELECT recipe_id, count(*) AS hours, count(*) FILTER (WHERE gp_hr >= $3) AS at_floor
  FROM process_snapshots WHERE ts > now() - interval '24 hours' GROUP BY 1
)
SELECT h.recipe_id, h.name, h.category, h.throughput, h.output_item,
       round(h.margin * h.rate)::bigint AS gp_hr, h.view_gp_hr,
       round(h.rate, 1)::float8 AS actions_per_hour,
       coalesce(s.hours, 0), coalesce(s.at_floor, 0)
FROM honest h
LEFT JOIN snap s USING (recipe_id)
WHERE h.rate IS NOT NULL
ORDER BY h.margin * h.rate DESC
LIMIT $2`

func (c *Collector) sweepProcess(ctx context.Context, asOf time.Time) int {
	rows, err := c.Store.Pool.Query(ctx, processSweepSQL, c.Cfg.ResearchBudgetGp, c.Cfg.SnapshotTopN, eval.FloorPGpHr)
	if err != nil {
		log.Printf("collect: process sweep: %v", err)
		return 0
	}
	defer rows.Close()
	type processRow struct {
		RecipeID       int     `json:"recipe_id"`
		Name           string  `json:"name"`
		Category       string  `json:"category"`
		Throughput     string  `json:"throughput"`
		OutputItem     int     `json:"output_item"`
		GpHr           int64   `json:"gp_hr"`
		ViewGpHr       *int64  `json:"view_gp_hr"`
		ActionsPerHour float64 `json:"actions_per_hour"`
		Hours          int     `json:"hours_24h"`
		AtFloor        int     `json:"hours_at_floor_24h"`
	}
	persistent := func(r processRow) bool {
		return r.Hours >= 12 && float64(r.AtFloor) >= 0.8*float64(r.Hours)
	}
	var parsed []processRow
	// Best recipe per output only: two share a signal, keyed on the item, and the later
	// lower-ranked row would overwrite the better one. Rows arrive best first, and a
	// recipe that holds beats a spike ranked above it, or the spike would hide it.
	at := map[int]int{}
	for rows.Next() {
		var r processRow
		if err := rows.Scan(&r.RecipeID, &r.Name, &r.Category, &r.Throughput, &r.OutputItem,
			&r.GpHr, &r.ViewGpHr, &r.ActionsPerHour, &r.Hours, &r.AtFloor); err != nil {
			log.Printf("collect: process scan: %v", err)
			return 0
		}
		i, seen := at[r.OutputItem]
		switch {
		case !seen:
			at[r.OutputItem] = len(parsed)
			parsed = append(parsed, r)
		case !persistent(parsed[i]) && persistent(r):
			parsed[i] = r
		}
	}
	// A recipe already on the board needs no new look: the vet would refuse it, and a
	// new signal can start a run. A failed lookup signals anyway, as the vet still holds.
	open := map[int]bool{}
	for _, r := range parsed {
		if r.GpHr >= eval.FloorPGpHr {
			has, err := c.Store.HasOpenStrategyForItem(ctx, r.OutputItem, "P")
			if err != nil {
				log.Printf("collect: process open lookup %d: %v", r.OutputItem, err)
			}
			open[r.OutputItem] = has
		}
	}
	return persist(c, ctx, asOf, "process", parsed, func(r processRow) (int, string, bool) {
		return r.OutputItem, r.Name, r.GpHr >= eval.FloorPGpHr && persistent(r) && !open[r.OutputItem]
	})
}

// --- watch lens: revalidation of the ranked portfolio ---

// sweepWatch queues a 'watch' signal for portfolio entries that haven't been
// validated recently, so research runs re-prove them (ship a fresh strategy)
// or decay them (dismiss). This is the loop that keeps the "good" stack
// honest: without it an entry's score would just coast on its history.
func (c *Collector) sweepWatch(ctx context.Context) int {
	due, err := c.Store.WatchDueForRevalidation(ctx, c.Cfg.RevalidateAfter, 10)
	if err != nil {
		log.Printf("collect: watch due: %v", err)
		return 0
	}
	newSignals := 0
	for _, w := range due {
		metrics := map[string]any{
			"watch_id": w.WatchID, "score": w.Score, "eff_score": w.EffScore,
			"source": w.Source, "times_confirmed": w.TimesConfirmed,
			"times_validated": w.TimesValidated,
		}
		if w.Archetype != nil {
			metrics["archetype"] = *w.Archetype
		}
		if w.Note != nil {
			metrics["note"] = *w.Note
		}
		if w.LastResult != nil {
			metrics["last_result"] = *w.LastResult
		}
		isNew, err := c.Store.UpsertSignal(ctx, "watch", w.ItemID, w.ItemName, metrics, 0)
		if err != nil {
			log.Printf("collect: watch signal %d: %v", w.ItemID, err)
			continue
		}
		if isNew {
			newSignals++
			log.Printf("collect: revalidation due: %s (eff score %.2f)", w.ItemName, w.EffScore)
		}
	}
	return newSignals
}

// persist stores the sweep's rows as trend snapshots and queues signals for
// rows the lens flags as significant (flag returns item_id, name,
// significant). Returns the count of NEW signals.
func persist[T any](c *Collector, ctx context.Context, asOf time.Time, lens string, rows []T, flag func(T) (int, string, bool)) int {
	trend := make([]store.TrendRow, 0, len(rows))
	for i, r := range rows {
		id, _, _ := flag(r)
		m, _ := json.Marshal(r)
		trend = append(trend, store.TrendRow{AsOf: asOf, Lens: lens, ItemID: id, Rank: i + 1, Metrics: m})
	}
	if err := c.Store.InsertTrendSnapshots(ctx, asOf, lens, trend); err != nil {
		log.Printf("collect: %s snapshots: %v", lens, err)
	}
	if store.LaneOff(lens, c.Cfg.LaneWeights) {
		return 0
	}
	newSignals := 0
	for _, r := range rows {
		id, name, significant := flag(r)
		if !significant {
			continue
		}
		isNew, err := c.Store.UpsertSignal(ctx, lens, id, name, r, c.Cfg.DismissCooldown)
		if err != nil {
			log.Printf("collect: %s signal %d: %v", lens, id, err)
			continue
		}
		if isNew {
			newSignals++
			log.Printf("collect: new %s signal: %s", lens, name)
		}
	}
	return newSignals
}
