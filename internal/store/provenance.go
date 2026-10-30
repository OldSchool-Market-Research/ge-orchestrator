package store

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// AttachProvenance records where each of a run's strategies came from, so the
// allocation compiler can say which method pays (migration 019). Inferred, not
// declared: the sidecar contract is unchanged and the agent can't flatter
// itself. Call it before the run's verdicts resolve its signals.
func (s *Store) AttachProvenance(ctx context.Context, runID int64, runStart time.Time, list []SidecarStrategy, verdicts []SignalVerdict) error {
	if len(list) == 0 {
		return nil
	}
	if runStart.IsZero() {
		runStart = time.Now()
	}
	assigned, err := s.collectSignals(ctx, `SELECT signal_id, kind, item_id, item_name, metrics,
		status, run_id, created_at, resolved_at, reason
		FROM orchestrator.signals WHERE run_id=$1 ORDER BY signal_id`, runID)
	if err != nil {
		return err
	}
	ids := make([]int, 0, len(list))
	for i := range list {
		if len(list[i].Items) > 0 {
			ids = append(ids, list[i].PrimaryItemID())
		}
	}
	hits, shown, err := s.screenHits(ctx, runStart, ids)
	if err != nil {
		return err
	}
	for i := range list {
		if len(list[i].Items) == 0 {
			continue // InsertStrategies rejects the whole sidecar with a real error
		}
		id := list[i].PrimaryItemID()
		list[i].ScreenHits = hits[id]
		list[i].SourceSignalID, list[i].SourceLens = attribute(list[i], assigned, verdicts, shown[id])
	}
	return nil
}

// attribute picks a strategy's source: the assigned signal whose shipped verdict
// names it, else an assigned signal for its item or recipe that the run didn't
// dismiss (its own lane's kind first), else its lane's collector rows if the brief
// showed the item there, else "free". shown lists those lenses. Everything else
// stays in screen_hits as association only: the agent runs its own screens, so a
// row the brief never showed did not find this strategy.
func attribute(st SidecarStrategy, assigned []Signal, verdicts []SignalVerdict, shown []string) (*int64, string) {
	byID := make(map[int64]Signal, len(assigned))
	for _, sig := range assigned {
		byID[sig.SignalID] = sig
	}
	dismissed := map[int64]bool{}
	for _, v := range verdicts {
		if v.Verdict != "shipped" {
			dismissed[int64(v.SignalID)] = true
			continue
		}
		if sig, ok := byID[int64(v.SignalID)]; ok && st.ID != "" && strings.Contains(v.Reason, st.ID) {
			id := sig.SignalID
			return &id, sig.Kind
		}
	}
	var other *Signal
	for i := range assigned {
		if dismissed[assigned[i].SignalID] || !signalMatches(assigned[i], st) {
			continue
		}
		if kindLane[assigned[i].Kind] == st.Archetype {
			id := assigned[i].SignalID
			return &id, assigned[i].Kind
		}
		if other == nil {
			other = &assigned[i]
		}
	}
	if other != nil {
		id := other.SignalID
		return &id, other.Kind
	}
	for _, lens := range shown {
		if kindLane[lens] == st.Archetype {
			return nil, "sweep:" + lens
		}
	}
	return nil, "free"
}

// signalMatches: the signal's item is the strategy's primary item, or the signal
// priced the same recipe (a conversion's relation, a processing recipe).
func signalMatches(sig Signal, st SidecarStrategy) bool {
	if sig.ItemID == st.PrimaryItemID() {
		return true
	}
	if st.RelationID == nil && st.RecipeID == nil {
		return false
	}
	var m struct {
		RelationID *int `json:"relation_id"`
		RecipeID   *int `json:"recipe_id"`
	}
	if json.Unmarshal(sig.Metrics, &m) != nil {
		return false
	}
	return (st.RelationID != nil && m.RelationID != nil && *m.RelationID == *st.RelationID) ||
		(st.RecipeID != nil && m.RecipeID != nil && *m.RecipeID == *st.RecipeID)
}

// screenHits maps each item to the lenses whose latest sweep before the run held
// it, and to the subset whose rows the brief showed (BriefSweepRows), both sorted.
// Sweeps older than a day say nothing about this run.
func (s *Store) screenHits(ctx context.Context, runStart time.Time, itemIDs []int) (hits, shown map[int][]string, err error) {
	rows, err := s.Pool.Query(ctx, `WITH latest AS (
			SELECT lens, max(as_of) AS as_of FROM orchestrator.trend_snapshots
			WHERE as_of <= $1 AND as_of > $1::timestamptz - interval '1 day'
			GROUP BY lens
		)
		SELECT t.item_id, t.lens, coalesce(t.rank, 0) FROM orchestrator.trend_snapshots t
		JOIN latest l USING (lens, as_of)
		WHERE t.item_id = ANY($2)`, runStart, itemIDs)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	hits, shown = map[int][]string{}, map[int][]string{}
	for rows.Next() {
		var id, rank int
		var lens string
		if err := rows.Scan(&id, &lens, &rank); err != nil {
			return nil, nil, err
		}
		hits[id] = append(hits[id], lens)
		if rank > 0 && rank <= BriefSweepRows(lens) {
			shown[id] = append(shown[id], lens)
		}
	}
	for _, m := range []map[int][]string{hits, shown} {
		for id := range m {
			sort.Strings(m[id])
		}
	}
	return hits, shown, rows.Err()
}

// LinkSignalStrategies points each signal at the accepted strategy it led to. Two
// ships can claim one signal (both directions of a recipe); the first one keeps it.
func (s *Store) LinkSignalStrategies(ctx context.Context, runID int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE orchestrator.signals sig SET strategy_id = st.strategy_id
		FROM (SELECT DISTINCT ON (source_signal_id) source_signal_id, strategy_id
		      FROM orchestrator.strategies
		      WHERE run_id=$1 AND source_signal_id IS NOT NULL AND state <> 'vetoed'
		      ORDER BY source_signal_id, strategy_id) st
		WHERE st.source_signal_id = sig.signal_id`, runID)
	return err
}
