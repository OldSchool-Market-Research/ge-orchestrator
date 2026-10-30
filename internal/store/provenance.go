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
	hits, err := s.screenHits(ctx, runStart, ids)
	if err != nil {
		return err
	}
	for i := range list {
		if len(list[i].Items) == 0 {
			continue // InsertStrategies rejects the whole sidecar with a real error
		}
		h := hits[list[i].PrimaryItemID()]
		list[i].ScreenHits = h
		list[i].SourceSignalID, list[i].SourceLens = attribute(list[i], assigned, verdicts, h)
	}
	return nil
}

// attribute picks a strategy's source: the assigned signal whose shipped verdict
// names it, else an assigned signal for its item or recipe (its own lane's kind
// first), else its lane's collector screen, else "free". Other lanes' screens
// holding the item stay in screen_hits as association only: a seasonal top-25
// row did not find a conversion.
func attribute(st SidecarStrategy, assigned []Signal, verdicts []SignalVerdict, hits []string) (*int64, string) {
	byID := make(map[int64]Signal, len(assigned))
	for _, sig := range assigned {
		byID[sig.SignalID] = sig
	}
	for _, v := range verdicts {
		if sig, ok := byID[int64(v.SignalID)]; ok && v.Verdict == "shipped" && st.ID != "" && strings.Contains(v.Reason, st.ID) {
			id := sig.SignalID
			return &id, sig.Kind
		}
	}
	var other *Signal
	for i := range assigned {
		if !signalMatches(assigned[i], st) {
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
	for _, lens := range hits {
		if kindLane[lens] == st.Archetype {
			return nil, "sweep:" + lens
		}
	}
	return nil, "free"
}

// signalMatches: the signal's item is the strategy's primary item, or, for a
// conversion, the signal priced the same recipe.
func signalMatches(sig Signal, st SidecarStrategy) bool {
	if sig.ItemID == st.PrimaryItemID() {
		return true
	}
	if st.RelationID == nil {
		return false
	}
	var m struct {
		RelationID *int `json:"relation_id"`
	}
	return json.Unmarshal(sig.Metrics, &m) == nil && m.RelationID != nil && *m.RelationID == *st.RelationID
}

// screenHits maps each item to the lenses whose latest sweep before the run held
// it, sorted. Sweeps older than a day say nothing about this run.
func (s *Store) screenHits(ctx context.Context, runStart time.Time, itemIDs []int) (map[int][]string, error) {
	rows, err := s.Pool.Query(ctx, `WITH latest AS (
			SELECT lens, max(as_of) AS as_of FROM orchestrator.trend_snapshots
			WHERE as_of <= $1 AND as_of > $1::timestamptz - interval '1 day'
			GROUP BY lens
		)
		SELECT t.item_id, t.lens FROM orchestrator.trend_snapshots t
		JOIN latest l USING (lens, as_of)
		WHERE t.item_id = ANY($2)`, runStart, itemIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int][]string{}
	for rows.Next() {
		var id int
		var lens string
		if err := rows.Scan(&id, &lens); err != nil {
			return nil, err
		}
		out[id] = append(out[id], lens)
	}
	for id := range out {
		sort.Strings(out[id])
	}
	return out, rows.Err()
}

// LinkSignalStrategies points each signal at the accepted strategy it led to.
func (s *Store) LinkSignalStrategies(ctx context.Context, runID int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE orchestrator.signals sig SET strategy_id = st.strategy_id
		FROM orchestrator.strategies st
		WHERE st.run_id=$1 AND st.source_signal_id = sig.signal_id AND st.state <> 'vetoed'`, runID)
	return err
}
