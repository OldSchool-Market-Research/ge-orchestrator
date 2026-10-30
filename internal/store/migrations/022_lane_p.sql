-- Lane P (Jade's go-ahead, 2026-10-30): processing recipes as strategies, judged in gp
-- per hour of play. Recipes come from ge-data's process_recipes; a strategy cites one
-- by recipe_id. process is a collector lens and a signal kind.

ALTER TABLE orchestrator.strategies ADD COLUMN recipe_id int;

ALTER TABLE orchestrator.strategies DROP CONSTRAINT strategies_archetype_check;
ALTER TABLE orchestrator.strategies
  ADD CONSTRAINT strategies_archetype_check
  CHECK (archetype IN ('F','B','S','V','C','U','H','P')) NOT VALID;

ALTER TABLE orchestrator.watchlist DROP CONSTRAINT watchlist_archetype_check;
ALTER TABLE orchestrator.watchlist
  ADD CONSTRAINT watchlist_archetype_check
  CHECK (archetype IN ('F','B','S','V','C','U','H','P'));

ALTER TABLE orchestrator.trend_snapshots DROP CONSTRAINT trend_snapshots_lens_check;
ALTER TABLE orchestrator.trend_snapshots
  ADD CONSTRAINT trend_snapshots_lens_check
  CHECK (lens IN ('seasonal','volume','volume_how','band','flip','vflip','hvflip','combo','process'));

ALTER TABLE orchestrator.signals DROP CONSTRAINT signals_kind_check;
ALTER TABLE orchestrator.signals
  ADD CONSTRAINT signals_kind_check
  CHECK (kind IN ('seasonal','volume','band','watch','flip','vflip','hvflip','combo','process'));
