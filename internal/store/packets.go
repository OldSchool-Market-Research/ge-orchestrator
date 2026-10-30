package store

import (
	"context"
	"encoding/json"
	"sort"
	"time"
)

// Lane packets (migration 023): what a script would ship from a lane, built at a
// run's start from the vet's own rules, set against what the run's model pitched. Log
// mode: no run reads these rows. A pitch compares with the script once the vet has
// passed it, since the script's list has been through the same rules.

// PacketRow is one recipe's verdict. Nil numbers are NULLs kept as NULLs: a nil GpHr
// is a recipe that can't be priced or doesn't earn, never a zero.
type PacketRow struct {
	RecipeID        int    `json:"recipe_id"`
	Name            string `json:"name"`
	Category        string `json:"category"`
	OutputItem      *int   `json:"output_item"`
	GpHr            *int64 `json:"gp_hr"`
	UnitsPer4h      int64  `json:"units_per_4h"`
	Hours24h        int    `json:"hours_24h"`
	HoursAtFloor24h int    `json:"hours_at_floor_24h"`
	MaxLegAgeS      *int64 `json:"max_leg_age_s"`
	Ship            bool   `json:"ship"`
	Why             string `json:"why,omitempty"` // why not
}

// PacketPick is one P strategy a run's model pitched.
type PacketPick struct {
	RecipeID   *int    `json:"recipe_id"`
	Sid        string  `json:"sid"`
	State      string  `json:"state"`
	VetoReason *string `json:"veto_reason,omitempty"`
	ScriptWhy  string  `json:"script_why,omitempty"` // why the script passed on it
}

// PacketRun is one run's packet against its pitches. Only the rows the script would
// ship come back: the rest stay in the table, reachable through a pick's ScriptWhy.
type PacketRun struct {
	RunID      int64        `json:"run_id"`
	RunStatus  string       `json:"run_status"`
	BuiltAt    time.Time    `json:"built_at"`
	Judged     int          `json:"judged"`
	Script     []PacketRow  `json:"script"`
	Both       []int        `json:"both"`
	ScriptOnly []int        `json:"script_only"`
	ModelOnly  []PacketPick `json:"model_only"`
	Vetoed     []PacketPick `json:"model_vetoed"`
}

// PacketSummary totals the succeeded runs: a failed run pitched nothing, which says
// nothing about the script.
type PacketSummary struct {
	Runs       int `json:"runs"`
	Script     int `json:"script"`
	Model      int `json:"model"`
	Both       int `json:"both"`
	ScriptOnly int `json:"script_only"`
	ModelOnly  int `json:"model_only"`
	Vetoed     int `json:"model_vetoed"`
}

// InsertLanePacket records a run's lane P packet.
func (s *Store) InsertLanePacket(ctx context.Context, runID int64, rows []PacketRow) error {
	raw, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO orchestrator.lane_packets (run_id, lane, mode, candidates)
		VALUES ($1, 'P', 'log', $2)`, runID, raw)
	return err
}

// PacketRuns returns the latest lane P packets against their runs' pitches, newest
// first. runID 0 means any run.
func (s *Store) PacketRuns(ctx context.Context, runID int64, limit int) ([]PacketRun, PacketSummary, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT p.run_id, r.status, p.built_at, p.candidates
		FROM orchestrator.lane_packets p
		JOIN orchestrator.runs r USING (run_id)
		WHERE p.lane = 'P' AND ($1::bigint = 0 OR p.run_id = $1)
		ORDER BY p.run_id DESC LIMIT $2`, runID, limit)
	if err != nil {
		return nil, PacketSummary{}, err
	}
	defer rows.Close()
	type packet struct {
		run  PacketRun
		rows []PacketRow
	}
	var packets []packet
	ids := []int64{}
	for rows.Next() {
		var p packet
		var raw []byte
		if err := rows.Scan(&p.run.RunID, &p.run.RunStatus, &p.run.BuiltAt, &raw); err != nil {
			return nil, PacketSummary{}, err
		}
		if err := json.Unmarshal(raw, &p.rows); err != nil {
			return nil, PacketSummary{}, err
		}
		packets = append(packets, p)
		ids = append(ids, p.run.RunID)
	}
	if err := rows.Err(); err != nil {
		return nil, PacketSummary{}, err
	}

	pickRows, err := s.Pool.Query(ctx, `
		SELECT run_id, recipe_id, sid, state, state_reason
		FROM orchestrator.strategies
		WHERE archetype = 'P' AND run_id = ANY($1)
		ORDER BY run_id, sid`, ids)
	if err != nil {
		return nil, PacketSummary{}, err
	}
	defer pickRows.Close()
	picks := map[int64][]PacketPick{}
	for pickRows.Next() {
		var id int64
		var pk PacketPick
		if err := pickRows.Scan(&id, &pk.RecipeID, &pk.Sid, &pk.State, &pk.VetoReason); err != nil {
			return nil, PacketSummary{}, err
		}
		if pk.State != "vetoed" {
			pk.VetoReason = nil
		}
		picks[id] = append(picks[id], pk)
	}
	if err := pickRows.Err(); err != nil {
		return nil, PacketSummary{}, err
	}

	out := make([]PacketRun, 0, len(packets))
	for _, p := range packets {
		comparePacket(&p.run, p.rows, picks[p.run.RunID])
		out = append(out, p.run)
	}
	return out, summarizePackets(out), nil
}

// comparePacket fills a run's comparison from its packet rows and its pitches.
func comparePacket(run *PacketRun, rows []PacketRow, picks []PacketPick) {
	run.Judged = len(rows)
	run.Script, run.Both, run.ScriptOnly = []PacketRow{}, []int{}, []int{}
	run.ModelOnly, run.Vetoed = []PacketPick{}, []PacketPick{}
	byRecipe := make(map[int]PacketRow, len(rows))
	for _, r := range rows {
		byRecipe[r.RecipeID] = r
		if r.Ship {
			run.Script = append(run.Script, r)
		}
	}
	accepted := map[int]bool{}
	for _, pk := range picks {
		if pk.RecipeID != nil {
			if r, ok := byRecipe[*pk.RecipeID]; !ok {
				pk.ScriptWhy = "not in the packet"
			} else if !r.Ship {
				pk.ScriptWhy = r.Why
			}
		}
		if pk.State == "vetoed" {
			run.Vetoed = append(run.Vetoed, pk)
			continue
		}
		if pk.RecipeID != nil {
			accepted[*pk.RecipeID] = true
			if byRecipe[*pk.RecipeID].Ship {
				continue
			}
		}
		run.ModelOnly = append(run.ModelOnly, pk)
	}
	for _, r := range run.Script {
		if accepted[r.RecipeID] {
			run.Both = append(run.Both, r.RecipeID)
		} else {
			run.ScriptOnly = append(run.ScriptOnly, r.RecipeID)
		}
	}
	sort.Ints(run.Both)
	sort.Ints(run.ScriptOnly)
}

func summarizePackets(runs []PacketRun) PacketSummary {
	var sum PacketSummary
	for _, r := range runs {
		if r.RunStatus != "succeeded" {
			continue
		}
		sum.Runs++
		sum.Script += len(r.Script)
		sum.Model += len(r.Both) + len(r.ModelOnly)
		sum.Both += len(r.Both)
		sum.ScriptOnly += len(r.ScriptOnly)
		sum.ModelOnly += len(r.ModelOnly)
		sum.Vetoed += len(r.Vetoed)
	}
	return sum
}
