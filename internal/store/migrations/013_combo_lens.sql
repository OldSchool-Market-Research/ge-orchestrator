-- Combo lens (C-lane collector, 2026-08-11). The record's only
-- lifetime-positive lane had no collector lens at all — C candidates
-- existed only when a research run happened to free-scan list_relations,
-- which is why 5 C strategies shipped ever. 'combo' becomes a first-class
-- signal kind and trend lens: the collector prices every relation both
-- directions hourly and queues the ones whose budget-sized gp/4h clears
-- the lane floor.

ALTER TABLE orchestrator.trend_snapshots DROP CONSTRAINT trend_snapshots_lens_check;
ALTER TABLE orchestrator.trend_snapshots
  ADD CONSTRAINT trend_snapshots_lens_check
  CHECK (lens IN ('seasonal','volume','band','flip','vflip','hvflip','combo'));

ALTER TABLE orchestrator.signals DROP CONSTRAINT signals_kind_check;
ALTER TABLE orchestrator.signals
  ADD CONSTRAINT signals_kind_check
  CHECK (kind IN ('seasonal','volume','band','watch','flip','vflip','hvflip','combo'));
