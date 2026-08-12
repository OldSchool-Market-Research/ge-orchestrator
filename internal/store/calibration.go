package store

import (
	"context"
	"time"
)

// Calibration (docs/FEEDBACK-LOOP.md §3A): per-archetype factors compiled
// from the paper record, appended on every strategy close. The factor
// decomposes to dodge the censoring trap — a strategy killed at 1h realized
// ≈ 0 *because it was killed early*, so a blended realized/projected average
// reads ~0 and teaches "ship nothing" instead of "ship honestly":
//
//	EV_calibrated = p_survive × pace_ratio × EV_raw
//
// p_survive  — share of closed ships alive past CalSurviveHours
// pace_ratio — median realized/projected pace of the ships that survived
//
// Components below their sample gate stay NULL and conservative defaults
// apply, so a fresh install neither flatters nor panics.
const (
	CalWindowDays   = 14
	CalSurviveHours = 6.0

	calMinSample       = 10
	calDefaultPSurvive = 0.25 // ≈ the observed pre-loop reality
	calDefaultPace     = 0.5
	// calFactorMin raised 0.05 -> 0.15 (2026-08-11): at 0.05 the brief
	// translated the 400k F floor into an ~8M raw claim — the mathematically
	// correct way to say "ship nothing", and the loop obliged (75% of runs
	// shipped zero). 0.15 reads ~2.7M: demanding but pitchable, and equal to
	// the clamped conservative default, so cold starts and the floor agree.
	calFactorMin = 0.15
	calFactorMax = 1.2
)

type CalibrationRow struct {
	ComputedAt   time.Time `json:"computed_at"`
	Archetype    string    `json:"archetype"`
	WindowDays   int       `json:"window_days"`
	SurviveHours float64   `json:"survive_hours"`
	NClosed      int       `json:"n_closed"`
	NSurvived    int       `json:"n_survived"`
	PSurvive     *float64  `json:"p_survive"`
	NPace        int       `json:"n_pace"`
	PaceRatio    *float64  `json:"pace_ratio"`
	Factor       float64   `json:"factor"`
	// Epoch is the measurement regime the row was computed under (see
	// migration 015) — the dashboard draws the cut line from it.
	Epoch int `json:"epoch"`
}

// EpochFor returns the archetype's current measurement epoch. The default
// is 1 for everything; the fill-sim flag flip moves F to 2 (set from main
// via Epochs). Strategies are stamped with it at insert; calibration
// recomputes filter to it, so cross-regime samples never blend.
func (s *Store) EpochFor(archetype string) int {
	if s.Epochs != nil {
		if e, ok := s.Epochs[archetype]; ok && e > 0 {
			return e
		}
	}
	return 1
}

// calibrationFactor multiplies the components, substituting conservative
// defaults for below-gate (nil) ones, and clamps the product: the floor keeps
// a cold start from vetoing everything, the ceiling keeps a lucky small
// sample from inflating claims past what any record supports.
func calibrationFactor(pSurvive, paceRatio *float64) float64 {
	p, pace := calDefaultPSurvive, calDefaultPace
	if pSurvive != nil {
		p = *pSurvive
	}
	if paceRatio != nil {
		pace = *paceRatio
	}
	f := p * pace
	if f < calFactorMin {
		return calFactorMin
	}
	if f > calFactorMax {
		return calFactorMax
	}
	return f
}

// RecomputeCalibration recomputes one archetype's factors over the trailing
// window and appends the result to the history. Vetoed rows never traded and
// armed V rows that never fired have no meaningful lifetime — both are
// excluded. Pace divides by the same denominator as the confirm rule
// (harness projection when present, agent claim as legacy fallback) and
// counts only post-anchor ticks, mirroring EvalStats.
func (s *Store) RecomputeCalibration(ctx context.Context, archetype string) (*CalibrationRow, error) {
	row := CalibrationRow{Archetype: archetype, WindowDays: CalWindowDays, SurviveHours: CalSurviveHours,
		Epoch: s.EpochFor(archetype)}
	var medPace *float64
	err := s.Pool.QueryRow(ctx, `WITH closed AS (
			SELECT st.strategy_id,
			       coalesce(st.triggered_at, st.opened_at) AS anchor,
			       extract(epoch from (st.closed_at - coalesce(st.triggered_at, st.opened_at)))/3600.0 AS hours,
			       coalesce(st.projected_per_1h_gp, st.per_1h_gp) AS proj_1h
			FROM orchestrator.strategies st
			WHERE st.archetype = $1
			  AND st.eval_epoch = $4
			  AND st.state IN ('killed','expired','confirmed')
			  AND st.closed_at > now() - make_interval(days => $2)
			  AND (st.archetype <> 'V' OR st.triggered_at IS NOT NULL)
		), pace AS (
			SELECT (SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY e.realized_per_1h_gp)
			        FROM orchestrator.evaluations e
			        WHERE e.strategy_id = c.strategy_id AND e.at >= c.anchor)
			       / nullif(c.proj_1h, 0) AS ratio
			FROM closed c
			WHERE c.hours >= $3 AND c.proj_1h > 0
		)
		SELECT (SELECT count(*) FROM closed),
		       (SELECT count(*) FROM closed WHERE hours >= $3),
		       (SELECT count(*) FROM pace WHERE ratio IS NOT NULL),
		       (SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY ratio)::float8
		        FROM pace WHERE ratio IS NOT NULL)`,
		archetype, CalWindowDays, CalSurviveHours, row.Epoch).
		Scan(&row.NClosed, &row.NSurvived, &row.NPace, &medPace)
	if err != nil {
		return nil, err
	}
	if row.NClosed >= calMinSample {
		p := float64(row.NSurvived) / float64(row.NClosed)
		row.PSurvive = &p
	}
	if row.NPace >= calMinSample {
		row.PaceRatio = medPace
	}
	row.Factor = calibrationFactor(row.PSurvive, row.PaceRatio)

	err = s.Pool.QueryRow(ctx, `INSERT INTO orchestrator.calibration
			(archetype, window_days, survive_hours, n_closed, n_survived, p_survive, n_pace, pace_ratio, factor, epoch)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING computed_at`,
		row.Archetype, row.WindowDays, row.SurviveHours, row.NClosed, row.NSurvived,
		row.PSurvive, row.NPace, row.PaceRatio, row.Factor, row.Epoch).
		Scan(&row.ComputedAt)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

const calibrationCols = `computed_at, archetype, window_days, survive_hours,
	n_closed, n_survived, p_survive, n_pace, pace_ratio, factor, epoch`

// CalibrationLatest returns the newest row per archetype AT ITS CURRENT
// EPOCH — a regime cut must not keep gating on the old regime's numbers.
// Falls back to the newest row of any epoch when the current epoch has
// none yet (pre-cut installs; the epoch-2 seed row covers the F cut).
func (s *Store) CalibrationLatest(ctx context.Context) ([]CalibrationRow, error) {
	all, err := s.collectCalibrations(ctx, `SELECT DISTINCT ON (archetype, epoch) `+calibrationCols+`
		FROM orchestrator.calibration ORDER BY archetype, epoch, computed_at DESC`)
	if err != nil {
		return nil, err
	}
	best := map[string]CalibrationRow{}
	for _, r := range all {
		cur, ok := best[r.Archetype]
		switch {
		case !ok:
			best[r.Archetype] = r
		case r.Epoch == s.EpochFor(r.Archetype) && cur.Epoch != s.EpochFor(r.Archetype):
			best[r.Archetype] = r
		case r.Epoch == cur.Epoch && r.ComputedAt.After(cur.ComputedAt):
			best[r.Archetype] = r
		}
	}
	out := make([]CalibrationRow, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	return out, nil
}

// CalibrationHistory returns the trailing-days history, oldest first — the
// learning chart's data.
func (s *Store) CalibrationHistory(ctx context.Context, days int) ([]CalibrationRow, error) {
	return s.collectCalibrations(ctx, `SELECT `+calibrationCols+`
		FROM orchestrator.calibration
		WHERE computed_at > now() - make_interval(days => $1)
		ORDER BY computed_at`, days)
}

// FailureModeCount is one cell of the lesson digest: how often an archetype
// died a particular way in the trailing window.
type FailureModeCount struct {
	Archetype string `json:"archetype"`
	Mode      string `json:"mode"`
	N         int    `json:"n"`
}

func (s *Store) FailureModeCounts(ctx context.Context, days int) ([]FailureModeCount, error) {
	rows, err := s.Pool.Query(ctx, `SELECT archetype, failure_mode, count(*)
		FROM orchestrator.strategies
		WHERE failure_mode IS NOT NULL AND closed_at > now() - make_interval(days => $1)
		GROUP BY archetype, failure_mode ORDER BY archetype, count(*) DESC`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FailureModeCount
	for rows.Next() {
		var c FailureModeCount
		if err := rows.Scan(&c.Archetype, &c.Mode, &c.N); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) collectCalibrations(ctx context.Context, query string, args ...any) ([]CalibrationRow, error) {
	rows, err := s.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CalibrationRow
	for rows.Next() {
		var r CalibrationRow
		if err := rows.Scan(&r.ComputedAt, &r.Archetype, &r.WindowDays, &r.SurviveHours,
			&r.NClosed, &r.NSurvived, &r.PSurvive, &r.NPace, &r.PaceRatio, &r.Factor); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
