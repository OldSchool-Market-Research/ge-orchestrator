-- Method provenance + lane allocation in log mode + regime logging (research
-- review P1, 2026-10-30). Nothing recorded which screen or signal produced a
-- strategy, so nothing could say which method pays: the loop learned which items
-- to avoid, never how to research.

-- Collector screens keep their rank: LatestTrends had no ORDER BY, and through
-- the primary key it returned the lowest item_ids of a sweep, so the brief's
-- "top" rows were never the top. NULL on rows from before this migration.
ALTER TABLE orchestrator.trend_snapshots ADD COLUMN rank int;

-- The same-hour-of-week volume scan moves into the collector (65s whole-market,
-- 45% of agent tool time). Snapshots only.
ALTER TABLE orchestrator.trend_snapshots DROP CONSTRAINT trend_snapshots_lens_check;
ALTER TABLE orchestrator.trend_snapshots
  ADD CONSTRAINT trend_snapshots_lens_check
  CHECK (lens IN ('seasonal','volume','volume_how','band','flip','vflip','hvflip','combo'));

-- Where a strategy came from, inferred at ingest (runner.attachProvenance):
--   source_signal_id  the assigned signal it answers, NULL when none does
--   source_lens       that signal's kind, 'sweep:<lens>' when the brief showed the
--                     item in its lane's collector rows, else 'free'. NULL on
--                     history: backfilling would be a guess
--   screen_hits       every collector lens holding the item before the run, shown
--                     or not
-- Screens are association, not cause: the agent runs similar screens of its own.
ALTER TABLE orchestrator.strategies
  ADD COLUMN source_signal_id bigint,
  ADD COLUMN source_lens      text,
  ADD COLUMN screen_hits      text[];

-- The accepted strategy a signal led to.
ALTER TABLE orchestrator.signals ADD COLUMN strategy_id bigint;

-- What each lane, source and signal kind yielded over the trailing window, and
-- the lane weights that would follow. Append-only like calibration; rows are
-- never read back into a run while mode='log'.
CREATE TABLE orchestrator.lane_allocation (
  allocation_id bigserial PRIMARY KEY,
  computed_at   timestamptz NOT NULL DEFAULT now(),
  mode          text        NOT NULL CHECK (mode IN ('log')),
  window_days   int         NOT NULL,
  lanes         jsonb       NOT NULL,
  sources       jsonb       NOT NULL,
  signal_kinds  jsonb       NOT NULL,
  weights       jsonb       NOT NULL
);
CREATE INDEX lane_allocation_at ON orchestrator.lane_allocation (computed_at DESC);

-- One row per collector sweep: the market conditions lane yields may later be
-- conditioned on (P4), logged from now because that takes weeks of history. Hour
-- and weekday come from as_of. Counts are over a sweep's top rows, so they
-- saturate at its row limit; the medians are what move.
CREATE TABLE orchestrator.regime_snapshots (
  as_of                   timestamptz PRIMARY KEY,
  flip_clear_floor        int,
  flip_median_gp          bigint,
  flip_persistence_median float8,
  combo_clear_floor       int,
  combo_median_gp         bigint,
  volume_anomalies        int,
  market_vol_ratio        float8
);
