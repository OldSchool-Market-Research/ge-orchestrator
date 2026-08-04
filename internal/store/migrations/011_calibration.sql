-- v11: calibration factor history (docs/FEEDBACK-LOOP.md §3A). One row per
-- recompute, appended on every strategy close — the history IS the "is the
-- system learning?" chart, so rows are never updated or deleted.
--
-- The factor decomposes to dodge the censoring trap: strategies killed at 1h
-- have realized ≈ 0 *because they were killed early* — a blended
-- realized/projected mean would read ~0 and teach "ship nothing". Instead:
--   factor = p_survive (share of closed ships alive past the survival bar)
--          × pace_ratio (median realized/projected pace of the SURVIVORS)
-- Components are NULL below their sample-size gate; the Go side substitutes
-- conservative defaults and clamps the product.

CREATE TABLE orchestrator.calibration (
    calibration_id bigserial PRIMARY KEY,
    computed_at    timestamptz NOT NULL DEFAULT now(),
    archetype      text        NOT NULL,
    window_days    int         NOT NULL,
    survive_hours  float8      NOT NULL,
    n_closed       int         NOT NULL,
    n_survived     int         NOT NULL,
    p_survive      float8,             -- NULL: n_closed below gate
    n_pace         int         NOT NULL,
    pace_ratio     float8,             -- NULL: n_pace below gate
    factor         float8      NOT NULL
);

CREATE INDEX calibration_archetype_at ON orchestrator.calibration (archetype, computed_at DESC);
