package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Signal is one row of the collector's work queue: something the sweeps
// flagged that a research run should investigate. Lifecycle:
// pending -> assigned (attached to a run's brief) -> investigated/dismissed
// (the run's signal_verdicts), or dismissed by staleness.
type Signal struct {
	SignalID   int64           `json:"signal_id"`
	Kind       string          `json:"kind"` // seasonal | volume | band
	ItemID     int             `json:"item_id"`
	ItemName   string          `json:"item_name"`
	Metrics    json.RawMessage `json:"metrics"`
	Status     string          `json:"status"`
	RunID      *int64          `json:"run_id,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	ResolvedAt *time.Time      `json:"resolved_at,omitempty"`
	Reason     *string         `json:"reason,omitempty"`
}

const (
	// Repeat guard: over Oct 16-30, 93% of the signals assigned to runs had
	// been dismissed before (vflip items ~22 times each), 0.45% became ships,
	// and re-checking them cost ~8.5 tool calls a run. A (kind, item) a run
	// dismissed repeatDismissMin times inside the window stays out of the
	// queue unless its score reaches repeatEscapeRatio x the score it was last
	// dismissed at: only a materially bigger edge re-opens a settled question.
	repeatDismissWindowDays = 14
	repeatDismissMin        = 2
	repeatEscapeRatio       = 2.0

	// settledDismissed is anything that should keep the collector from re-queueing an
	// item for the cooldown: a run's verdict, or the script's refusal, which would only
	// repeat hourly. A signal that aged out unassigned proved nothing about its item, and
	// neither did one withdrawn in a queue cleanup (migration 018).
	settledDismissed = `status='dismissed' AND coalesce(reason,'') <> 'expired unassigned' AND coalesce(reason,'') NOT LIKE 'withdrawn:%'`

	// agentDismissed selects the verdicts a run actually reached. A script's refusal
	// (evaluate mode's "script: ...") applied a rule at one moment, so it falsified nothing.
	agentDismissed = settledDismissed + ` AND coalesce(reason,'') NOT LIKE 'script:%'`

	// signalScore is a detection's size in its lens's own units, for the repeat
	// guard's escape. NULL (no such metric) never escapes.
	signalScore = `CASE $1::text WHEN 'combo' THEN (%[1]s->>'budget_cycle_gp')::float8
		WHEN 'process' THEN (%[1]s->>'gp_hr')::float8
		WHEN 'volume' THEN abs((%[1]s->>'z')::float8)
		ELSE (%[1]s->>'gp_cycle')::float8 END`
)

var (
	upsertSignalSQL    = upsertSQL(settledDismissed)
	upsertCandidateSQL = upsertSQL(agentDismissed)
)

// upsertSQL queues a detection unless the cooldown's dismissals or the repeat guard hold
// it out.
func upsertSQL(cooldown string) string {
	return fmt.Sprintf(`INSERT INTO orchestrator.signals (kind, item_id, item_name, metrics)
	SELECT $1::text,$2::int,$3::text,$4::jsonb
	WHERE $5::interval = '0'::interval OR (
		NOT EXISTS (
			SELECT 1 FROM orchestrator.signals
			WHERE kind=$1 AND item_id=$2 AND %[4]s
			  AND resolved_at > now() - $5::interval)
		AND NOT (
			(SELECT count(*) FROM orchestrator.signals
			 WHERE kind=$1 AND item_id=$2 AND %[1]s
			   AND resolved_at > now() - make_interval(days => $6::int)) >= $7
			AND coalesce(%[2]s >= $8::float8 * (
				SELECT %[3]s FROM orchestrator.signals
				WHERE kind=$1 AND item_id=$2 AND %[1]s AND %[3]s IS NOT NULL
				ORDER BY resolved_at DESC LIMIT 1), false) = false))
	ON CONFLICT (kind, item_id) WHERE status IN ('pending','assigned')
	DO UPDATE SET metrics = EXCLUDED.metrics
	RETURNING signal_id, (xmax = 0)`,
		agentDismissed, fmt.Sprintf(signalScore, "$4::jsonb"), fmt.Sprintf(signalScore, "metrics"), cooldown)
}

// UpsertSignal queues a detection. A live (pending/assigned) signal for the
// same (kind, item) just gets its metrics refreshed — detections don't stack.
// Returns true when a NEW pending signal was created.
//
// dismissCooldown is the dismissal-memory guard (docs/FEEDBACK-LOOP.md §3E):
// a (kind, item) dismissed within the cooldown is not re-queued — the
// falsification that killed it is assumed to still hold — and neither is one
// the repeat guard above holds out. Zero disables both (watch revalidation
// passes 0: re-proving is that lens's whole job). The guards also suppress the
// metrics refresh of an already-live signal whose (kind, item) they hold out,
// such as an escaped signal whose score has since fallen back; that refresh is
// cosmetic, so the simpler SQL wins.
func (s *Store) UpsertSignal(ctx context.Context, kind string, itemID int, itemName string, metrics any, dismissCooldown time.Duration) (bool, error) {
	_, inserted, err := s.upsert(ctx, upsertSignalSQL, kind, itemID, itemName, metrics, dismissCooldown)
	return inserted, err
}

// UpsertCandidateSignal queues a signal for something a packet would show the model. The
// script's own refusals don't hold it out: it clears the rules now, and they only said it
// didn't then. Returns the new signal's id and true; false means a run's verdict holds it
// out, or a signal for it is already live.
func (s *Store) UpsertCandidateSignal(ctx context.Context, kind string, itemID int, itemName string, metrics any, dismissCooldown time.Duration) (int64, bool, error) {
	return s.upsert(ctx, upsertCandidateSQL, kind, itemID, itemName, metrics, dismissCooldown)
}

func (s *Store) upsert(ctx context.Context, sql, kind string, itemID int, itemName string, metrics any, dismissCooldown time.Duration) (int64, bool, error) {
	m, err := json.Marshal(metrics)
	if err != nil {
		return 0, false, err
	}
	var id int64
	var inserted bool
	err = s.Pool.QueryRow(ctx, sql, kind, itemID, itemName, m, dismissCooldown.String(),
		repeatDismissWindowDays, repeatDismissMin, repeatEscapeRatio).Scan(&id, &inserted)
	if err == pgx.ErrNoRows {
		return 0, false, nil // suppressed: recently or repeatedly dismissed, not re-queued
	}
	return id, inserted, err
}

// LatestDismissal returns when a run last dismissed a (kind, item) and the
// recorded falsification, or nils if none ever did. The brief attaches this to
// re-entered candidates so a run starts from the prior verdict instead of
// re-deriving it.
func (s *Store) LatestDismissal(ctx context.Context, kind string, itemID int) (*time.Time, *string, error) {
	var at *time.Time
	var reason *string
	err := s.Pool.QueryRow(ctx, `SELECT resolved_at, reason FROM orchestrator.signals
		WHERE kind=$1 AND item_id=$2 AND `+agentDismissed+`
		ORDER BY resolved_at DESC LIMIT 1`, kind, itemID).Scan(&at, &reason)
	if err == pgx.ErrNoRows {
		return nil, nil, nil
	}
	return at, reason, err
}

// signalQueueScan bounds how much of the queue one assignment considers. It
// sits well above the queue's usual size (under 200 live signals).
const signalQueueScan = 500

// NextSignals picks the signals for one run: the oldest of each kind, shared
// out by lane weight (pickSignals).
func (s *Store) NextSignals(ctx context.Context, weights map[string]float64, limit int) ([]Signal, error) {
	pending, err := s.collectSignals(ctx, `SELECT signal_id, kind, item_id, item_name, metrics,
		status, run_id, created_at, resolved_at, reason
		FROM orchestrator.signals WHERE status='pending' ORDER BY created_at LIMIT $1`, signalQueueScan)
	if err != nil {
		return nil, err
	}
	return pickSignals(pending, weights, limit), nil
}

// kindLane is the lane behind each signal kind and collector lens. 'watch'
// (revalidation of the portfolio) answers to no lane.
var kindLane = map[string]string{
	"combo": "C", "vflip": "F", "flip": "F", "hvflip": "B", "volume": "V", "volume_how": "V", "process": "P",
	"seasonal": "S", "band": "H",
}

// LaneOff reports whether the weights switch off the lane behind a signal
// kind. A lane with no weight stays on, matching the vetter's rule 0.
func LaneOff(kind string, weights map[string]float64) bool {
	w, ok := weights[kindLane[kind]]
	return ok && w == 0
}

// pickSignals chooses up to limit signals from pending (oldest first). Watch
// revalidations go first, up to half the run. The rest is smooth weighted
// round-robin over the kinds by lane weight, oldest first within a kind, and
// kinds whose lane is off are skipped: first-in-first-out let the noisiest
// lens fill every run whatever its lane earned (volume signals: 965 dismissed,
// 1 shipped over Oct 16-30). A kind with no lane or weight counts as 1.
func pickSignals(pending []Signal, weights map[string]float64, limit int) []Signal {
	byKind := map[string][]Signal{}
	var kinds []string
	var out []Signal
	watchCap := limit / 2
	for _, sig := range pending {
		if sig.Kind == "watch" {
			if len(out) < watchCap {
				out = append(out, sig)
			}
			continue
		}
		if LaneOff(sig.Kind, weights) {
			continue
		}
		if _, seen := byKind[sig.Kind]; !seen {
			kinds = append(kinds, sig.Kind)
		}
		byKind[sig.Kind] = append(byKind[sig.Kind], sig)
	}
	sort.Strings(kinds) // deterministic whatever the queue's order
	weight := func(kind string) float64 {
		if w, ok := weights[kindLane[kind]]; ok {
			return w
		}
		return 1
	}
	credit := map[string]float64{}
	for len(out) < limit {
		best, total := "", 0.0
		for _, k := range kinds {
			if len(byKind[k]) == 0 {
				continue
			}
			credit[k] += weight(k)
			total += weight(k)
			if best == "" || credit[k] > credit[best] {
				best = k
			}
		}
		if best == "" {
			break
		}
		credit[best] -= total
		out = append(out, byKind[best][0])
		byKind[best] = byKind[best][1:]
	}
	return out
}

// Signals lists recent signals of any status, newest first.
func (s *Store) Signals(ctx context.Context, limit int) ([]Signal, error) {
	return s.collectSignals(ctx, `SELECT signal_id, kind, item_id, item_name, metrics,
		status, run_id, created_at, resolved_at, reason
		FROM orchestrator.signals ORDER BY signal_id DESC LIMIT $1`, limit)
}

// AssignSignals marks the given pending signals as assigned to a run.
func (s *Store) AssignSignals(ctx context.Context, runID int64, ids []int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE orchestrator.signals
		SET status='assigned', run_id=$1 WHERE signal_id = ANY($2) AND status='pending'`, runID, ids)
	return err
}

// ResolveSignal applies a run's verdict: shipped -> investigated,
// dismissed -> dismissed, both with the model's reason. A dismissed 'watch'
// signal is a failed revalidation, so it decays the item's portfolio entry
// (shipped watches score later, through the strategy's own outcome).
func (s *Store) ResolveSignal(ctx context.Context, signalID int64, verdict, reason string) error {
	status := "dismissed"
	if verdict == "shipped" {
		status = "investigated"
	}
	var kind string
	var itemID int
	err := s.Pool.QueryRow(ctx, `UPDATE orchestrator.signals
		SET status=$2, reason=$3, resolved_at=now()
		WHERE signal_id=$1 AND status IN ('pending','assigned')
		RETURNING kind, item_id`, signalID, status, reason).Scan(&kind, &itemID)
	if err == pgx.ErrNoRows {
		return nil // already resolved (or unknown id) — same as the old no-op
	}
	if err != nil {
		return err
	}
	if kind == "watch" && status == "dismissed" {
		return s.RecordWatchDismissal(ctx, itemID)
	}
	return nil
}

// DismissUnanswered closes a run's unanswered signals as its verdict. One linked to a
// strategy shipped, so it's left for ReleaseRunSignals: the open strategy answers it.
func (s *Store) DismissUnanswered(ctx context.Context, runID int64, reason string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE orchestrator.signals
		SET status='dismissed', reason=$2, resolved_at=now()
		WHERE run_id=$1 AND status='assigned' AND strategy_id IS NULL`, runID, reason)
	return err
}

// ReleaseRunSignals returns a run's unanswered signals to the queue so the next
// run picks them up. A signal going back is open again, so it drops any link to a
// strategy inferred for it; the strategy keeps its source_signal_id.
func (s *Store) ReleaseRunSignals(ctx context.Context, runID int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE orchestrator.signals
		SET status='pending', run_id=NULL, strategy_id=NULL WHERE run_id=$1 AND status='assigned'`, runID)
	return err
}

// ExpireStaleSignals dismisses pending signals older than ttl — a week-old
// anomaly is no longer a reaction-speed edge.
func (s *Store) ExpireStaleSignals(ctx context.Context, ttl time.Duration) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE orchestrator.signals
		SET status='dismissed', reason='expired unassigned', resolved_at=now()
		WHERE status='pending' AND created_at < now() - $1::interval`, ttl.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) collectSignals(ctx context.Context, query string, args ...any) ([]Signal, error) {
	rows, err := s.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Signal
	for rows.Next() {
		var sig Signal
		if err := rows.Scan(&sig.SignalID, &sig.Kind, &sig.ItemID, &sig.ItemName, &sig.Metrics,
			&sig.Status, &sig.RunID, &sig.CreatedAt, &sig.ResolvedAt, &sig.Reason); err != nil {
			return nil, err
		}
		out = append(out, sig)
	}
	return out, rows.Err()
}

// --- trend snapshots (the durable market-intelligence base) ---

// BriefSweepRows is how many of a lens's top rows the brief shows, which is also
// what provenance credits to it. The same-hour volume lens shows more: it stands
// in for a refused 25-row tool call, and its very top rows are often illiquid.
func BriefSweepRows(lens string) int {
	if lens == "volume_how" {
		return 15
	}
	return 5
}

type TrendRow struct {
	AsOf    time.Time       `json:"as_of"`
	Lens    string          `json:"lens"`
	ItemID  int             `json:"item_id"`
	Rank    int             `json:"rank,omitempty"` // 1 = the lens's top row; 0 on rows before migration 019
	Metrics json.RawMessage `json:"metrics"`
}

// InsertTrendSnapshots stores one sweep's top rows for a lens.
func (s *Store) InsertTrendSnapshots(ctx context.Context, asOf time.Time, lens string, rows []TrendRow) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, r := range rows {
		if _, err := tx.Exec(ctx, `INSERT INTO orchestrator.trend_snapshots (as_of, lens, item_id, metrics, rank)
			VALUES ($1,$2,$3,$4,nullif($5::int, 0)) ON CONFLICT DO NOTHING`, asOf, lens, r.ItemID, r.Metrics, r.Rank); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// LatestTrends returns the most recent sweep's rows for a lens, top-ranked first.
func (s *Store) LatestTrends(ctx context.Context, lens string, limit int) ([]TrendRow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT as_of, lens, item_id, metrics, coalesce(rank, 0)
		FROM orchestrator.trend_snapshots
		WHERE lens=$1 AND as_of = (SELECT max(as_of) FROM orchestrator.trend_snapshots WHERE lens=$1)
		ORDER BY rank NULLS LAST, item_id
		LIMIT $2`, lens, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrendRow
	for rows.Next() {
		var r TrendRow
		if err := rows.Scan(&r.AsOf, &r.Lens, &r.ItemID, &r.Metrics, &r.Rank); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
