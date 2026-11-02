package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Outcome is one strategy's life as the dashboard's status pages explain it:
// the state it is in, why it closed, how its evaluations split, and what it
// made on paper. Unlike PnLRow it covers every lane and state, so a vetoed or
// a lane-P strategy still answers "what happened to it". A null estimate means
// there is no gp figure to give (lane P's pace is per hour of play, a vetoed
// strategy never traded), never zero.
type Outcome struct {
	StrategyID  int64      `json:"strategy_id"`
	Sid         string     `json:"sid"`
	Title       string     `json:"title"`
	Archetype   string     `json:"archetype"`
	State       string     `json:"state"`
	StateReason *string    `json:"state_reason"`
	FailureMode *string    `json:"failure_mode"`
	OpenedAt    time.Time  `json:"opened_at"`
	TriggeredAt *time.Time `json:"triggered_at"`
	ClosedAt    *time.Time `json:"closed_at"`
	// Hours spent trading, 0 for a strategy that never did. WindowHours is the
	// test it is judged over.
	Hours       float64 `json:"hours"`
	WindowHours float64 `json:"window_hours"`

	NEvals      int        `json:"n_evals"`
	NHealthy    int        `json:"n_healthy"`
	NDegraded   int        `json:"n_degraded"`
	NKillSignal int        `json:"n_kill_signal"`
	LastEvalAt  *time.Time `json:"last_eval_at"`
	LastVerdict *string    `json:"last_verdict"`
	// KillStreak counts the newest evaluations in a row that said kill_signal.
	KillStreak int `json:"kill_streak"`

	MedRealized1h  *float64 `json:"med_realized_per_1h_gp"`
	LastRealized1h *int64   `json:"last_realized_per_1h_gp"`
	Projected1h    *int64   `json:"projected_per_1h_gp"`
	EstRealizedGp  *int64   `json:"est_realized_gp"`
	ProjectedGp    *int64   `json:"projected_gp"`
	SimScored      bool     `json:"sim_scored,omitempty"`

	Capital    *int64 `json:"capital_required"`
	Units      *int64 `json:"units_used"`
	PerCycleGp *int64 `json:"per_cycle_gp"`
	// known-bad numbers (migration 016): listed for the record, counted nowhere
	QuarantinedReason *string `json:"quarantined_reason,omitempty"`

	RunID           int64   `json:"run_id"`
	RunMode         string  `json:"run_mode"`
	TriggerSource   *string `json:"trigger_source"`
	PrimaryItemID   *int    `json:"primary_item_id"`
	PrimaryItemName *string `json:"primary_item_name"`
	Icon            *string `json:"icon"`

	// What closes it, from the evaluator's policy for this lane; the API fills these.
	KillNeeded        int     `json:"kill_needed"`
	ConfirmHealthy    float64 `json:"confirm_healthy"`
	ConfirmRatioBar   float64 `json:"confirm_ratio_bar"`
	ConfirmMinPer1hGp int64   `json:"confirm_min_per_1h_gp,omitempty"`
}

// killStreakDepth is how far back a kill streak is counted: the longest kill
// policy in package eval is 6 evaluations in a row.
const killStreakDepth = 6

const outcomeSelect = `SELECT s.strategy_id, s.sid, s.title, s.archetype, s.state, s.state_reason,
		s.failure_mode, s.opened_at, s.triggered_at, s.closed_at,
		greatest(extract(epoch from (coalesce(s.closed_at, now()) - coalesce(s.triggered_at, s.opened_at)))/3600.0, 0)::float8,
		(extract(epoch from s.eval_window)/3600.0)::float8,
		est.n, est.healthy, est.degraded, est.kills, est.med_1h::float8,
		last.at, last.verdicts, last.realized,
		coalesce(s.projected_per_1h_gp, s.per_1h_gp), pos.final_equity_gp,
		s.capital_required, s.units_used, s.per_cycle_gp, s.quarantined_reason,
		s.run_id, r.mode, r.trigger_source, s.primary_item_id, i.name, i.icon
		FROM orchestrator.strategies s
		JOIN orchestrator.runs r ON r.run_id = s.run_id
		JOIN LATERAL (
			SELECT count(*) AS n,
				count(*) FILTER (WHERE e.verdict = 'healthy') AS healthy,
				count(*) FILTER (WHERE e.verdict = 'degraded') AS degraded,
				count(*) FILTER (WHERE e.verdict = 'kill_signal') AS kills,
				percentile_cont(0.5) WITHIN GROUP (ORDER BY e.realized_per_1h_gp) AS med_1h
			FROM orchestrator.evaluations e
			WHERE e.strategy_id = s.strategy_id
			  AND (s.triggered_at IS NULL OR e.at >= s.triggered_at)
		) est ON true
		JOIN LATERAL (
			SELECT max(t.at) AS at,
				array_agg(t.verdict ORDER BY t.at DESC) AS verdicts,
				(array_agg(t.realized_per_1h_gp ORDER BY t.at DESC))[1] AS realized
			FROM (
				SELECT e.at, e.verdict, e.realized_per_1h_gp FROM orchestrator.evaluations e
				WHERE e.strategy_id = s.strategy_id ORDER BY e.at DESC LIMIT $1
			) t
		) last ON true
		LEFT JOIN orchestrator.positions pos ON pos.strategy_id = s.strategy_id
		LEFT JOIN items i ON i.item_id = s.primary_item_id `

// Outcomes lists every strategy shipped or still alive since `since` (nil: all
// of them), newest first. A vetoed strategy counts when it shipped, the only
// moment it existed; anything else counts until it closed.
func (s *Store) Outcomes(ctx context.Context, since *time.Time) ([]Outcome, error) {
	rows, err := s.Pool.Query(ctx, outcomeSelect+`
		WHERE $2::timestamptz IS NULL
		   OR s.opened_at >= $2
		   OR (s.state <> 'vetoed' AND coalesce(s.closed_at, now()) >= $2)
		ORDER BY s.strategy_id DESC`, killStreakDepth, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Outcome
	for rows.Next() {
		o, err := scanOutcome(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OutcomeByID is one strategy's outcome; nil when there is no such strategy.
func (s *Store) OutcomeByID(ctx context.Context, id int64) (*Outcome, error) {
	rows, err := s.Pool.Query(ctx, outcomeSelect+`WHERE s.strategy_id = $2`, killStreakDepth, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	o, err := scanOutcome(rows)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func scanOutcome(rows pgx.Rows) (Outcome, error) {
	var o Outcome
	var verdicts []string
	var per1h, finalEquity *int64
	if err := rows.Scan(&o.StrategyID, &o.Sid, &o.Title, &o.Archetype, &o.State, &o.StateReason,
		&o.FailureMode, &o.OpenedAt, &o.TriggeredAt, &o.ClosedAt, &o.Hours, &o.WindowHours,
		&o.NEvals, &o.NHealthy, &o.NDegraded, &o.NKillSignal, &o.MedRealized1h,
		&o.LastEvalAt, &verdicts, &o.LastRealized1h,
		&per1h, &finalEquity, &o.Capital, &o.Units, &o.PerCycleGp, &o.QuarantinedReason,
		&o.RunID, &o.RunMode, &o.TriggerSource, &o.PrimaryItemID, &o.PrimaryItemName, &o.Icon); err != nil {
		return o, err
	}
	o.Projected1h = per1h
	if len(verdicts) > 0 {
		o.LastVerdict = &verdicts[0]
	}
	for _, v := range verdicts {
		if v != "kill_signal" {
			break
		}
		o.KillStreak++
	}

	neverFired := o.TriggeredAt == nil && (o.State == "armed" || (o.FailureMode != nil && *o.FailureMode == "never_triggered"))
	if o.State == "vetoed" || neverFired {
		o.Hours = 0 // never traded: the clock above would read its age
		return o, nil
	}
	// P is per hour of play: pace x hours open would invent gp (as in PnL).
	if o.Archetype == "P" {
		return o, nil
	}
	o.EstRealizedGp, o.SimScored = realizedEstimate(o.MedRealized1h, finalEquity, o.Hours)
	if per1h != nil {
		v := int64(float64(*per1h) * o.Hours)
		o.ProjectedGp = &v
	}
	return o, nil
}
