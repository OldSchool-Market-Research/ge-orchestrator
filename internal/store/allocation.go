package store

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Lane allocation (research review P1): what each lane, source and signal kind
// yielded over the trailing window, and the lane weights that would follow,
// compiled by the collector and appended to orchestrator.lane_allocation. Log
// mode: no run reads these rows. Realized is still the evaluator's spread-mark
// estimate, not simulated fills, so the weights are a record to judge before
// anything acts on them.
const (
	allocWindowDays = 14
	// allocMinCloses is the sample a lane needs before its weight moves.
	allocMinCloses = 20
	// allocMaxStepPerDay caps how fast a weight moves.
	allocMaxStepPerDay = 0.25
	// A live lane keeps exploring at the floor; only the operator sets 0.
	allocWeightFloor = 0.25
	allocWeightCeil  = 2.0 // brief.Params.Validate's maximum
)

// LaneYield is one lane's record over the window.
type LaneYield struct {
	Lane          string   `json:"lane"`
	Pitched       int      `json:"pitched"`
	Vetoed        int      `json:"vetoed"`
	Closed        int      `json:"closed"`
	Survived      int      `json:"survived_6h"`
	Confirmed     int      `json:"confirmed"`
	EstRealizedGp int64    `json:"est_realized_gp"`
	PerShipGp     *int64   `json:"per_ship_gp"`   // nil: nothing closed
	Target        *float64 `json:"target_weight"` // nil: below the sample gate
}

// SourceYield is one (lane, source) cell. Source is a strategy's source_lens,
// or "unattributed" for ships from before provenance.
type SourceYield struct {
	Lane          string `json:"lane"`
	Source        string `json:"source"`
	Pitched       int    `json:"pitched"`
	Vetoed        int    `json:"vetoed"`
	Closed        int    `json:"closed"`
	Survived      int    `json:"survived_6h"`
	Confirmed     int    `json:"confirmed"`
	EstRealizedGp int64  `json:"est_realized_gp"`
}

// SignalKindYield is how one signal kind's resolved signals ended up.
type SignalKindYield struct {
	Kind      string `json:"kind"`
	Shipped   int    `json:"shipped"`   // a run's verdict
	Linked    int    `json:"linked"`    // an accepted strategy answers it
	Dismissed int    `json:"dismissed"` // a run's verdict
	Expired   int    `json:"expired"`   // aged out unassigned
	Withdrawn int    `json:"withdrawn"` // harness cleanup
}

// LaneAllocation is one lane_allocation row.
type LaneAllocation struct {
	AllocationID int64              `json:"allocation_id"`
	ComputedAt   time.Time          `json:"computed_at"`
	Mode         string             `json:"mode"`
	WindowDays   int                `json:"window_days"`
	Lanes        []LaneYield        `json:"lanes"`
	Sources      []SourceYield      `json:"sources"`
	SignalKinds  []SignalKindYield  `json:"signal_kinds"`
	Weights      map[string]float64 `json:"weights"`
}

// allocationTargets maps each lane with enough closes to a weight by its
// realized gp per closed ship, relative to the best such lane: the best earns
// the ceiling, a lane earning nothing the floor. No lane with positive yield
// means no targets, so every weight holds.
func allocationTargets(lanes []LaneYield) map[string]float64 {
	best := int64(0)
	for _, l := range lanes {
		if l.Closed >= allocMinCloses && l.PerShipGp != nil && *l.PerShipGp > best {
			best = *l.PerShipGp
		}
	}
	targets := map[string]float64{}
	if best <= 0 {
		return targets
	}
	for _, l := range lanes {
		if l.Closed < allocMinCloses || l.PerShipGp == nil {
			continue
		}
		rel := float64(*l.PerShipGp) / float64(best)
		if rel < 0 {
			rel = 0
		}
		targets[l.Lane] = allocWeightFloor + (allocWeightCeil-allocWeightFloor)*rel
	}
	return targets
}

// stepWeights moves each live lane toward its target, at most
// allocMaxStepPerDay x elapsed (capped at a day). base is the operator's
// weights: its lanes are the live set, a 0 stays 0, and a lane with no previous
// weight starts from its base. A lane with no target holds.
func stepWeights(base, prev, targets map[string]float64, elapsed time.Duration) map[string]float64 {
	if elapsed > 24*time.Hour {
		elapsed = 24 * time.Hour
	}
	maxStep := allocMaxStepPerDay * elapsed.Hours() / 24
	out := make(map[string]float64, len(base))
	for lane, b := range base {
		if b == 0 {
			out[lane] = 0
			continue
		}
		w, ok := prev[lane]
		if !ok || w == 0 {
			w = b
		}
		if t, ok := targets[lane]; ok {
			switch {
			case t > w+maxStep:
				w += maxStep
			case t < w-maxStep:
				w -= maxStep
			default:
				w = t
			}
		}
		if w < allocWeightFloor {
			w = allocWeightFloor
		}
		if w > allocWeightCeil {
			w = allocWeightCeil
		}
		out[lane] = w
	}
	return out
}

// CompileAllocation computes the current allocation against the operator's base
// weights and appends it. Lanes read their current epoch only, like calibration.
func (s *Store) CompileAllocation(ctx context.Context, base map[string]float64) (*LaneAllocation, error) {
	lanes, sources, err := s.laneYields(ctx)
	if err != nil {
		return nil, err
	}
	kinds, err := s.signalKindYields(ctx)
	if err != nil {
		return nil, err
	}
	targets := allocationTargets(lanes)
	for i := range lanes {
		if t, ok := targets[lanes[i].Lane]; ok {
			lanes[i].Target = &t
		}
	}
	prev, err := s.LatestAllocation(ctx)
	if err != nil {
		return nil, err
	}
	var prevWeights map[string]float64
	var elapsed time.Duration
	if prev != nil {
		prevWeights = prev.Weights
		elapsed = time.Since(prev.ComputedAt)
	}
	a := &LaneAllocation{Mode: "log", WindowDays: allocWindowDays, Lanes: lanes, Sources: sources,
		SignalKinds: kinds, Weights: stepWeights(base, prevWeights, targets, elapsed)}
	lanesJSON, _ := json.Marshal(a.Lanes)
	sourcesJSON, _ := json.Marshal(a.Sources)
	kindsJSON, _ := json.Marshal(a.SignalKinds)
	weightsJSON, _ := json.Marshal(a.Weights)
	err = s.Pool.QueryRow(ctx, `INSERT INTO orchestrator.lane_allocation
		(mode, window_days, lanes, sources, signal_kinds, weights)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING allocation_id, computed_at`,
		a.Mode, a.WindowDays, lanesJSON, sourcesJSON, kindsJSON, weightsJSON).
		Scan(&a.AllocationID, &a.ComputedAt)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// laneYields reads the window's strategies once, grouped by (lane, source,
// epoch), and rolls up to lanes. Pitches count by opened_at and closes by
// closed_at, so a lane's numbers need not be the same ships. Realized is PnL's
// estimate: sim equity when a position closed, else median pace x hours held.
func (s *Store) laneYields(ctx context.Context) ([]LaneYield, []SourceYield, error) {
	rows, err := s.Pool.Query(ctx, `WITH w AS (
			SELECT s.strategy_id, s.archetype, coalesce(s.source_lens,'unattributed') AS source,
			       s.eval_epoch, s.state, s.triggered_at,
			       s.opened_at > now() - make_interval(days => $1) AS pitched_in,
			       s.state IN ('killed','expired','confirmed')
			         AND s.closed_at > now() - make_interval(days => $1)
			         AND (s.archetype <> 'V' OR s.triggered_at IS NOT NULL) AS closed_in,
			       extract(epoch from (s.closed_at - coalesce(s.triggered_at, s.opened_at)))/3600.0 AS hours
			FROM orchestrator.strategies s
			WHERE s.quarantined_reason IS NULL
			  AND (s.opened_at > now() - make_interval(days => $1)
			       OR s.closed_at > now() - make_interval(days => $1))
		)
		SELECT w.archetype, w.source, w.eval_epoch,
		       count(*) FILTER (WHERE w.pitched_in),
		       count(*) FILTER (WHERE w.pitched_in AND w.state = 'vetoed'),
		       count(*) FILTER (WHERE w.closed_in),
		       count(*) FILTER (WHERE w.closed_in AND w.hours >= $2),
		       count(*) FILTER (WHERE w.closed_in AND w.state = 'confirmed'),
		       coalesce(sum(coalesce(pos.final_equity_gp, est.med_1h * greatest(w.hours, 0)))
		                FILTER (WHERE w.closed_in), 0)::bigint
		FROM w
		LEFT JOIN LATERAL (
			SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY e.realized_per_1h_gp) AS med_1h
			FROM orchestrator.evaluations e
			WHERE e.strategy_id = w.strategy_id
			  AND (w.triggered_at IS NULL OR e.at >= w.triggered_at)
		) est ON w.closed_in
		LEFT JOIN orchestrator.positions pos ON pos.strategy_id = w.strategy_id
		GROUP BY 1, 2, 3`, allocWindowDays, CalSurviveHours)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byLane := map[string]*LaneYield{}
	sources := []SourceYield{}
	for rows.Next() {
		var c SourceYield
		var epoch int
		if err := rows.Scan(&c.Lane, &c.Source, &epoch, &c.Pitched, &c.Vetoed, &c.Closed,
			&c.Survived, &c.Confirmed, &c.EstRealizedGp); err != nil {
			return nil, nil, err
		}
		if epoch != s.EpochFor(c.Lane) {
			continue
		}
		sources = append(sources, c)
		l := byLane[c.Lane]
		if l == nil {
			l = &LaneYield{Lane: c.Lane}
			byLane[c.Lane] = l
		}
		l.Pitched += c.Pitched
		l.Vetoed += c.Vetoed
		l.Closed += c.Closed
		l.Survived += c.Survived
		l.Confirmed += c.Confirmed
		l.EstRealizedGp += c.EstRealizedGp
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	lanes := make([]LaneYield, 0, len(byLane))
	for _, l := range byLane {
		if l.Closed > 0 {
			v := l.EstRealizedGp / int64(l.Closed)
			l.PerShipGp = &v
		}
		lanes = append(lanes, *l)
	}
	sort.Slice(lanes, func(i, j int) bool { return lanes[i].Lane < lanes[j].Lane })
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].Lane != sources[j].Lane {
			return sources[i].Lane < sources[j].Lane
		}
		return sources[i].Source < sources[j].Source
	})
	return lanes, sources, nil
}

func (s *Store) signalKindYields(ctx context.Context) ([]SignalKindYield, error) {
	rows, err := s.Pool.Query(ctx, `SELECT kind,
		count(*) FILTER (WHERE status='investigated'),
		count(*) FILTER (WHERE strategy_id IS NOT NULL),
		count(*) FILTER (WHERE `+agentDismissed+`),
		count(*) FILTER (WHERE status='dismissed' AND reason='expired unassigned'),
		count(*) FILTER (WHERE status='dismissed' AND reason LIKE 'withdrawn:%')
		FROM orchestrator.signals
		WHERE resolved_at > now() - make_interval(days => $1)
		GROUP BY kind ORDER BY kind`, allocWindowDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SignalKindYield{}
	for rows.Next() {
		var k SignalKindYield
		if err := rows.Scan(&k.Kind, &k.Shipped, &k.Linked, &k.Dismissed, &k.Expired, &k.Withdrawn); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

const allocationCols = `allocation_id, computed_at, mode, window_days, lanes, sources, signal_kinds, weights`

func scanAllocation(row interface{ Scan(...any) error }) (*LaneAllocation, error) {
	var a LaneAllocation
	var lanes, sources, kinds, weights []byte
	if err := row.Scan(&a.AllocationID, &a.ComputedAt, &a.Mode, &a.WindowDays, &lanes, &sources, &kinds, &weights); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		raw []byte
		to  any
	}{{lanes, &a.Lanes}, {sources, &a.Sources}, {kinds, &a.SignalKinds}, {weights, &a.Weights}} {
		if err := json.Unmarshal(f.raw, f.to); err != nil {
			return nil, err
		}
	}
	return &a, nil
}

// LatestAllocation returns the newest row, nil when none exists.
func (s *Store) LatestAllocation(ctx context.Context) (*LaneAllocation, error) {
	a, err := scanAllocation(s.Pool.QueryRow(ctx, `SELECT `+allocationCols+`
		FROM orchestrator.lane_allocation ORDER BY computed_at DESC LIMIT 1`))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return a, err
}

// AllocationPoint is one weights-over-time sample.
type AllocationPoint struct {
	ComputedAt time.Time          `json:"computed_at"`
	Weights    map[string]float64 `json:"weights"`
}

// AllocationHistory returns the trailing-days weights, oldest first.
func (s *Store) AllocationHistory(ctx context.Context, days int) ([]AllocationPoint, error) {
	rows, err := s.Pool.Query(ctx, `SELECT computed_at, weights FROM orchestrator.lane_allocation
		WHERE computed_at > now() - make_interval(days => $1) ORDER BY computed_at`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AllocationPoint{}
	for rows.Next() {
		var p AllocationPoint
		var raw []byte
		if err := rows.Scan(&p.ComputedAt, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &p.Weights); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
