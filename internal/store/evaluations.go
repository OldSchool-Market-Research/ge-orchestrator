package store

import (
	"context"
	"encoding/json"
	"time"
)

type Evaluation struct {
	StrategyID      int64           `json:"strategy_id"`
	At              time.Time       `json:"at"`
	CurHigh         *int64          `json:"cur_high"`
	CurLow          *int64          `json:"cur_low"`
	HighAgeS        *int            `json:"high_age_s"`
	LowAgeS         *int            `json:"low_age_s"`
	CurMargin       *int64          `json:"cur_margin"`
	Vol30m          *int64          `json:"vol_30m"`
	RealizedPer1hGp *int64          `json:"realized_per_1h_gp"`
	Checks          json.RawMessage `json:"checks"`
	Verdict         string          `json:"verdict"`
	Detail          json.RawMessage `json:"detail,omitempty"`
}

func (s *Store) InsertEvaluation(ctx context.Context, e Evaluation) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO orchestrator.evaluations
		(strategy_id, at, cur_high, cur_low, high_age_s, low_age_s, cur_margin, vol_30m,
		 realized_per_1h_gp, checks, verdict, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		e.StrategyID, e.At, e.CurHigh, e.CurLow, e.HighAgeS, e.LowAgeS, e.CurMargin, e.Vol30m,
		e.RealizedPer1hGp, e.Checks, e.Verdict, e.Detail)
	return err
}

func (s *Store) Evaluations(ctx context.Context, strategyID int64, limit int) ([]Evaluation, error) {
	rows, err := s.Pool.Query(ctx, `SELECT strategy_id, at, cur_high, cur_low, high_age_s,
		low_age_s, cur_margin, vol_30m, realized_per_1h_gp, checks, verdict, detail
		FROM orchestrator.evaluations WHERE strategy_id=$1 ORDER BY at DESC LIMIT $2`,
		strategyID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Evaluation
	for rows.Next() {
		var e Evaluation
		if err := rows.Scan(&e.StrategyID, &e.At, &e.CurHigh, &e.CurLow, &e.HighAgeS,
			&e.LowAgeS, &e.CurMargin, &e.Vol30m, &e.RealizedPer1hGp, &e.Checks, &e.Verdict, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LastEvalAt returns the newest evaluation time for a strategy (nil if none).
// The ticker uses it to honor per-kind cadences (H evaluates hourly).
func (s *Store) LastEvalAt(ctx context.Context, strategyID int64) (*time.Time, error) {
	var t *time.Time
	err := s.Pool.QueryRow(ctx, `SELECT max(at) FROM orchestrator.evaluations
		WHERE strategy_id=$1`, strategyID).Scan(&t)
	return t, err
}

// LastVerdicts returns the most recent n verdicts, newest first.
func (s *Store) LastVerdicts(ctx context.Context, strategyID int64, n int) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT verdict FROM orchestrator.evaluations
		WHERE strategy_id=$1 ORDER BY at DESC LIMIT $2`, strategyID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// EvalStats supports the confirmation rule: share of healthy evals + median
// realized/projected since the given anchor (a V strategy's armed ticks must
// not count toward its confirmation — pass triggered_at; zero time = all).
// The denominator is the harness's own ship-time projection when present
// (migration 009) — realized and projected then share the same haircut
// arithmetic, so the 0.5 confirm bar is actually reachable; the agent's
// claim is only the legacy fallback.
func (s *Store) EvalStats(ctx context.Context, strategyID int64, since time.Time) (total, healthy int, medianRatio *float64, err error) {
	err = s.Pool.QueryRow(ctx, `SELECT count(*),
		count(*) FILTER (WHERE verdict='healthy'),
		(percentile_cont(0.5) WITHIN GROUP (ORDER BY realized_per_1h_gp)
		 / nullif((SELECT coalesce(projected_per_1h_gp, per_1h_gp)
		           FROM orchestrator.strategies WHERE strategy_id=$1), 0))::float8
		FROM orchestrator.evaluations WHERE strategy_id=$1 AND at >= $2`, strategyID, since).
		Scan(&total, &healthy, &medianRatio)
	return
}

type ScoreboardRow struct {
	Archetype           string   `json:"archetype"`
	N                   int      `json:"n"`
	Confirmed           int      `json:"confirmed"`
	Killed              int      `json:"killed"`
	Expired             int      `json:"expired"`
	Open                int      `json:"open"`
	Armed               int      `json:"armed"`
	Vetoed              int      `json:"vetoed"`
	RealizedVsProjected *float64 `json:"realized_vs_projected"`
}

func (s *Store) Scoreboard(ctx context.Context) ([]ScoreboardRow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT archetype, n, confirmed, killed, expired, open, armed, vetoed,
		realized_vs_projected::float8 FROM orchestrator.scoreboard ORDER BY archetype`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScoreboardRow
	for rows.Next() {
		var r ScoreboardRow
		if err := rows.Scan(&r.Archetype, &r.N, &r.Confirmed, &r.Killed, &r.Expired, &r.Open, &r.Armed, &r.Vetoed, &r.RealizedVsProjected); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PnLRow is one strategy's paper-trade estimate: the median haircut
// realized_per_1h over its evaluated life × hours in that life. It is an
// ESTIMATE of what following the strategy would have printed, not a ledger —
// upper-bounded like every paper number (see package eval's honesty rules).
type PnLRow struct {
	StrategyID    int64      `json:"strategy_id"`
	Sid           string     `json:"sid"`
	Title         string     `json:"title"`
	Archetype     string     `json:"archetype"`
	State         string     `json:"state"`
	OpenedAt      time.Time  `json:"opened_at"`
	ClosedAt      *time.Time `json:"closed_at,omitempty"`
	Hours         float64    `json:"hours"`
	MedRealized1h *float64   `json:"med_realized_per_1h_gp"`
	EstRealizedGp *int64     `json:"est_realized_gp"`
	ProjectedGp   *int64     `json:"projected_gp"`
	Capital       *int64     `json:"capital_required"`
	// EvalEpoch annotates the measurement regime (migration 015); SimScored
	// marks rows whose est_realized_gp is the fill simulator's frozen
	// final equity rather than the median-pace estimate — the dashboard
	// annotates the regime break instead of hiding it.
	EvalEpoch int  `json:"eval_epoch"`
	SimScored bool `json:"sim_scored,omitempty"`
}

// PnL returns per-strategy paper-trade estimates for every strategy that has
// actually been evaluated: vetoed rows and never-triggered armed rows are
// excluded (they were never trading). Hours run from the eval-clock anchor
// (triggered_at for fired Vs, opened_at otherwise) to closed_at or now.
func (s *Store) PnL(ctx context.Context) ([]PnLRow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT s.strategy_id, s.sid, s.title, s.archetype, s.state,
		s.opened_at, s.closed_at,
		greatest(extract(epoch from (coalesce(s.closed_at, now()) - coalesce(s.triggered_at, s.opened_at)))/3600.0, 0)::float8 AS hours,
		est.med_1h::float8, coalesce(s.projected_per_1h_gp, s.per_1h_gp), s.capital_required,
		s.eval_epoch, pos.final_equity_gp
		FROM orchestrator.strategies s
		JOIN LATERAL (
			SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY e.realized_per_1h_gp) AS med_1h
			FROM orchestrator.evaluations e
			WHERE e.strategy_id = s.strategy_id
			  AND (s.triggered_at IS NULL OR e.at >= s.triggered_at)
		) est ON true
		LEFT JOIN orchestrator.positions pos ON pos.strategy_id = s.strategy_id
		WHERE s.state <> 'vetoed' AND NOT (s.state = 'armed' AND s.triggered_at IS NULL)
		ORDER BY s.strategy_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PnLRow
	for rows.Next() {
		var r PnLRow
		var per1h, finalEquity *int64
		if err := rows.Scan(&r.StrategyID, &r.Sid, &r.Title, &r.Archetype, &r.State,
			&r.OpenedAt, &r.ClosedAt, &r.Hours, &r.MedRealized1h, &per1h, &r.Capital,
			&r.EvalEpoch, &finalEquity); err != nil {
			return nil, err
		}
		switch {
		case finalEquity != nil:
			// Sim-scored close: the frozen ledger equity IS the realized
			// number — no pace-times-hours estimate involved.
			r.EstRealizedGp = finalEquity
			r.SimScored = true
		case r.MedRealized1h != nil:
			v := int64(*r.MedRealized1h * r.Hours)
			r.EstRealizedGp = &v
		}
		if per1h != nil {
			v := int64(float64(*per1h) * r.Hours)
			r.ProjectedGp = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecentlyClosed lists recently killed/expired strategies for the brief's
// do-not-re-pitch section.
func (s *Store) RecentlyClosed(ctx context.Context, limit int) ([]Strategy, error) {
	return s.collectStrategies(ctx, `SELECT `+strategyCols+` FROM orchestrator.strategies
		WHERE state IN ('killed','expired') ORDER BY closed_at DESC LIMIT $1`, limit)
}

// FlipPersistence24h computes the ship-time persistence gate's inputs for one
// item against a reference post-tax margin (the strategy's own claimed
// margin): okHours = hours of the last 24 whose hourly avg post-tax spread
// held >= 50% of refMargin; obsHours = hours where both sides traded at all.
// Same statistic, same tax rule (least(floor(high/50), 5M)) as ge-mcp's
// margin_persistence_24h field, so the number the agent cites and the number
// the vetter enforces cannot drift apart.
func (s *Store) FlipPersistence24h(ctx context.Context, itemID int, refMargin int64) (okHours, obsHours int, err error) {
	err = s.Pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE h.hi - least(floor(h.hi/50), 5000000) - h.lo >= $2 * 0.5),
		count(*)
		FROM (
			SELECT avg(avg_high_price) FILTER (WHERE high_volume > 0) AS hi,
			       avg(avg_low_price)  FILTER (WHERE low_volume  > 0) AS lo
			FROM prices_5m
			WHERE item_id = $1 AND ts > now() - interval '24 hours'
			GROUP BY date_trunc('hour', ts)
		) h
		WHERE h.hi IS NOT NULL AND h.lo IS NOT NULL`, itemID, refMargin).
		Scan(&okHours, &obsHours)
	return
}
