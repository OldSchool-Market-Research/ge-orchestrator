-- Measurement epochs (fillsim phase 1, 2026-08-11). When the fill
-- simulator goes live for F, its realized numbers are apples-to-oranges
-- with the instant-spread marks that came before — but only for F, so the
-- reset is a per-archetype epoch column, not a truncation. Calibration
-- recomputes filter to the archetype's CURRENT epoch; epoch-1 history
-- stays append-only so the learning chart keeps both regimes and the
-- dashboard can draw the cut line.

ALTER TABLE orchestrator.strategies  ADD COLUMN eval_epoch int NOT NULL DEFAULT 1;
ALTER TABLE orchestrator.calibration ADD COLUMN epoch      int NOT NULL DEFAULT 1;

-- Seed the epoch-2 F row at a conservative 0.15 so the first live-flag
-- ingest reads a real row instead of flashing factor=1 (fail-open) or
-- inheriting epoch-1's spiral. 0.15 == the raised clamp floor: the brief
-- then implies a ~2.7M raw claim against the 400k floor — demanding but
-- pitchable, vs the 8M the 0.05 floor demanded.
INSERT INTO orchestrator.calibration
  (archetype, window_days, survive_hours, n_closed, n_survived, n_pace, factor, epoch)
VALUES ('F', 14, 6.0, 0, 0, 0, 0.15, 2);
