package runner

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/osrs-ge/ge-orchestrator/internal/brief"
	"github.com/osrs-ge/ge-orchestrator/internal/eval"
	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

// Lane P's candidate packet: what a script would ship at a run's start, judged by the
// vet's own rules so it can't drift from them. An evaluate run judges it; a research
// run never sees it, and the row only measures what the run picked. Each read is the vet's own per-recipe query, which is cheap at this scale: 254
// recipes took about a second on prod, once per run.

// packetTimeout bounds a build, so a slow database can't leave one hanging.
const packetTimeout = time.Minute

// logProcessPacket stores the packet for a run that's starting, beside the run rather
// than ahead of it. Nothing depends on it, so any failure only costs the row.
func (r *Runner) logProcessPacket(runID int64, p brief.Params) {
	if r.Prices == nil {
		return
	}
	if w, ok := p.Archetypes["P"]; ok && w == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), packetTimeout)
	defer cancel()
	rows, err := r.buildProcessPacket(ctx, p)
	if err != nil {
		log.Printf("run %d: lane P packet: %v", runID, err)
		return
	}
	if err := r.Store.InsertLanePacket(ctx, runID, "log", rows); err != nil {
		log.Printf("run %d: lane P packet: %v", runID, err)
		return
	}
	ship := 0
	for _, row := range rows {
		if row.Ship {
			ship++
		}
	}
	log.Printf("run %d: lane P packet (log only): %d of %d recipes would ship", runID, ship, len(rows))
}

// logPacketComparison logs a finished ingest's agreement with its packet.
func (r *Runner) logPacketComparison(ctx context.Context, runID int64) {
	runs, _, err := r.Store.PacketRuns(ctx, runID, 1)
	if err != nil {
		log.Printf("run %d: lane P packet comparison: %v", runID, err)
		return
	}
	if len(runs) == 0 {
		return
	}
	c := runs[0]
	log.Printf("run %d: lane P packet: script %d, model %d, both %d",
		runID, len(c.Script), len(c.Both)+len(c.ModelOnly), len(c.Both))
}

// buildProcessPacket judges every recipe. A lookup failure is that recipe's verdict,
// unless the deadline caused it: then the whole packet is unreliable.
func (r *Runner) buildProcessPacket(ctx context.Context, p brief.Params) ([]store.PacketRow, error) {
	recipes, err := r.Store.ProcessQuotes(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]store.PacketRow, 0, len(recipes))
	for _, q := range recipes {
		row := store.PacketRow{RecipeID: q.RecipeID, Name: q.Name, Category: q.Category}
		snap, err := r.Prices.ProcessSnapshot(ctx, q.RecipeID)
		if err == nil {
			row.HoursAtFloor24h, row.Hours24h, err = r.Store.ProcessPersistence(ctx, q.RecipeID, eval.FloorPGpHr)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			row.Why = lookupFailed + err.Error()
		} else {
			judgeRecipe(&row, snap, q.FeeGp, p.CapitalGp)
		}
		rows = append(rows, row)
	}
	keepBestPerOutput(rows)
	r.blockOnRecord(ctx, rows)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	sortPacket(rows)
	return rows, nil
}

// judgeRecipe applies the vet's lane P rules to a recipe at its honest size: the size
// a strategy would copy, so the projection is the one the vet would compute.
func judgeRecipe(row *store.PacketRow, snap *eval.ProcessSnap, feeGp float64, budgetGp int64) {
	row.OutputItem, row.MaxLegAgeS = snap.OutputItem, snap.MaxLegAgeS
	why := ""
	if units := eval.ProcessUnitsPer4h(snap, budgetGp); units > 0 { // 0 would project at the market rate, not at no size
		row.UnitsPer4h = &units
		row.GpHr = eval.ProjectProcessPer1h(snap, units)
		size := copiedSize(units, *snap.GrossIn, feeGp)
		why = sizeConsistencyVeto(size, budgetGp)
		// The vet's tolerance stops short of the agent's validator: it holds capital to
		// the budget exactly, and one strategy over it rejects the whole report.
		if why == "" && size.CapitalRequired > budgetGp {
			why = fmt.Sprintf("capital_required %d is over the %d research budget", size.CapitalRequired, budgetGp)
		}
	}
	if why == "" {
		why = processVeto(snap, row.GpHr)
	}
	if why == "" {
		why = processPersistenceVeto(row.HoursAtFloor24h, row.Hours24h)
	}
	if why == "" && snap.OutputItem == nil {
		why = "no main output for a strategy to name"
	}
	row.Why = strings.TrimPrefix(why, "vetoed at ship time: ")
	row.Ship = why == ""
}

// copiedSize is the size fields a strategy would copy from process_quote: its
// entry_price includes the recipe's fee, which the budget bound on units doesn't.
func copiedSize(units int64, grossIn, feeGp float64) store.SidecarStrategy {
	var st store.SidecarStrategy
	st.Size.UnitsUsed = units
	st.EntryPrice = int64(math.Round(grossIn + feeGp))
	st.CapitalRequired = units * st.EntryPrice
	return st
}

// keepBestPerOutput leaves one shippable recipe per output item, the best paid: the
// dedup keys on the item, so recipes sharing one compete for its place on the book.
func keepBestPerOutput(rows []store.PacketRow) {
	best := map[int]int{}
	for i := range rows {
		r := &rows[i]
		if !r.Ship {
			continue
		}
		at, seen := best[*r.OutputItem]
		switch {
		case !seen:
			best[*r.OutputItem] = i
		case *r.GpHr > *rows[at].GpHr || (*r.GpHr == *rows[at].GpHr && r.RecipeID < rows[at].RecipeID):
			demote(&rows[at], r.RecipeID)
			best[*r.OutputItem] = i
		default:
			demote(r, rows[at].RecipeID)
		}
	}
}

func demote(r *store.PacketRow, by int) {
	r.Ship = false
	r.Why = fmt.Sprintf("recipe %d makes the same item for more", by)
}

// blockOnRecord applies the vet's book rules to what's left: the item's already
// trading in lane P, or it's graveyarded. A failed lookup passes, as in the vet.
func (r *Runner) blockOnRecord(ctx context.Context, rows []store.PacketRow) {
	rec := r.loadRecord(ctx)
	for i := range rows {
		row := &rows[i]
		if !row.Ship {
			continue
		}
		dup, err := r.Store.HasOpenStrategyForItem(ctx, *row.OutputItem, "P")
		if err != nil {
			log.Printf("lane P packet: dedup lookup item %d: %v", *row.OutputItem, err)
		} else if dup {
			row.Ship, row.Why = false, fmt.Sprintf("item %d already has an open P strategy", *row.OutputItem)
			continue
		}
		if g, ok := rec.graveEntry(*row.OutputItem, "P"); ok {
			if why := graveyardVeto(g, r.Cfg.GraveyardCooldown, time.Now().UTC()); why != "" {
				row.Ship, row.Why = false, strings.TrimPrefix(why, "vetoed at ship time: ")
			}
		}
	}
}

// sortPacket orders rows best paid first, with the unpriced last.
func sortPacket(rows []store.PacketRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch {
		case a.GpHr == nil || b.GpHr == nil:
			if (a.GpHr == nil) != (b.GpHr == nil) {
				return b.GpHr == nil
			}
		case *a.GpHr != *b.GpHr:
			return *a.GpHr > *b.GpHr
		}
		return a.RecipeID < b.RecipeID
	})
}
