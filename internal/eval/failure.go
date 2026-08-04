package eval

import (
	"context"
	"encoding/json"
	"log"

	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

// Failure-mode labeling (migration 012, docs/FEEDBACK-LOOP.md §3B).
// state_reason is free text; the lesson digest needs an enum. A kill is
// labeled by the failing health check that dominated the kill window; ties
// break by story priority — a breached stop or a collapsed margin is the
// lesson even when staleness tagged along.
var checkModes = []struct{ check, mode string }{
	{"kill_price_ok", "stopped_out"},
	{"margin_ok", "margin_collapse"},
	{"margin_alive", "margin_collapse"},
	{"floor_ok", "margin_collapse"},
	{"buy_leg_fresh", "leg_stale"},
	{"sell_leg_fresh", "leg_stale"},
	{"legs_fresh", "leg_stale"},
	{"legs_priced", "leg_stale"},
	{"vol_ok", "volume_dried"},
	{"entry_reachable", "entry_unreachable"},
	{"exit_reachable", "exit_not_printing"},
}

// dominantFailureMode counts failing checks across the evaluations of the
// kill window and returns the mode of the most frequent one (ties break in
// checkModes order). Nil when nothing countable failed — a legacy row or
// unreadable checks stays unlabeled rather than guessed at.
func dominantFailureMode(evals []store.Evaluation) *string {
	counts := map[string]int{}
	for _, e := range evals {
		var checks map[string]bool
		if err := json.Unmarshal(e.Checks, &checks); err != nil {
			continue
		}
		for k, ok := range checks {
			if !ok {
				counts[k]++
			}
		}
	}
	mode, best := "", 0
	for _, cm := range checkModes {
		if n := counts[cm.check]; n > best {
			mode, best = cm.mode, n
		}
	}
	if best == 0 {
		return nil
	}
	return &mode
}

// failureMode labels a close for the lesson digest. Best-effort: on a read
// error the label is dropped, never fabricated.
func (ev *Evaluator) failureMode(ctx context.Context, st store.Strategy, state string) *string {
	switch state {
	case "killed":
		evals, err := ev.Store.Evaluations(ctx, st.StrategyID, policyFor(st.Archetype).KillConsecutive)
		if err != nil {
			log.Printf("eval: failure mode for %s: %v", st.Sid, err)
			return nil
		}
		return dominantFailureMode(evals)
	case "expired":
		return strPtr("expired_below_pace")
	}
	return nil
}

// recalibrate refreshes the archetype's calibration factors after a close.
// Failure is logged, not returned — the close already happened and must not
// be retried as if it hadn't (same contract as watchlist scoring).
func (ev *Evaluator) recalibrate(ctx context.Context, archetype string) {
	if _, err := ev.Store.RecomputeCalibration(ctx, archetype); err != nil {
		log.Printf("eval: recalibrate %s: %v", archetype, err)
	}
}

func strPtr(s string) *string { return &s }
