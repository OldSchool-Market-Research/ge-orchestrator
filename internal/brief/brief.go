// Package brief renders the per-run brief: user constraints + the harness's
// own paper-trade track record + the collector's assigned signals and latest
// market sweep. The rendered text is stored on the run and shown by the
// preview endpoint, so the human always sees exactly what the model will see.
// The feedback loop closes here with zero LLM involvement.
package brief

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

// Params is the structured brief body (stored as runs.brief).
type Params struct {
	CapitalGp     int64              `json:"capital_gp"`
	Risk          string             `json:"risk"` // low | medium | high
	Members       *bool              `json:"members"`
	MinConfidence string             `json:"min_confidence"`
	Archetypes    map[string]float64 `json:"archetypes"` // S/V/C/U/H -> weight 0..2
	Notes         string             `json:"notes"`
}

// C is the primary lane; S/H are retired as shippable kinds but stay in the
// order so their paper-trade track record still renders (it is the evidence
// for why they were retired).
var archetypeOrder = []string{"C", "P", "F", "B", "V", "U", "S", "H"}

var archetypeNames = map[string]string{
	"F": "volume flip", "B": "high-value flip",
	"S": "seasonal window (retired)", "V": "volume anomaly", "C": "conversion",
	"P": "processing recipe, gp per hour of play",
	"U": "update/event", "H": "swing hold (retired)",
}

func Defaults() Params {
	return Params{
		// The operator runs a research engine on a 50M budget: the engine
		// surfaces and paper-proves every option worth running at that scale;
		// the operator picks. C is primary, the repeat-on-your-own-time lane
		// (decants/sets/combines vs the item_relations table — the agent
		// notes-and-moves-on if the table is absent): +299M est. over Oct 16-30
		// against F's +3.9M. V/U are opportunistic. F is OFF (weight 0, the
		// operator's call, 2026-10-30): 0 of its 81 closes that fortnight survived
		// 6h. The collector keeps its vflip snapshots and the regime row keeps
		// its persistence, so the evidence for turning it back on stays. S/H
		// retired (0% confirmed at this data age). B is OFF (weight 0,
		// vet-enforced) on its first-fortnight record: 3/33 winners, -67.5M
		// realized, and still -21.6M in the most generous counterfactual
		// (every position sold at its last observed high leg) — bad selection
		// AND bad structure (2% tax + slippage needs >3% appreciation on a
		// 10-50M item). A high-value lane may return as a re-specced quick-arb
		// (roundtrips-gated, spread-capture eval), not as the mid-marked
		// multi-day hold this lane actually was.
		CapitalGp: 50_000_000, Risk: "low", MinConfidence: "medium",
		// P is new (Oct 30): recipes the harness can now judge in gp per hour of play.
		Archetypes: map[string]float64{"F": 0, "B": 0, "V": 0.5, "C": 1.5, "U": 0.5, "P": 1},
		// No default notes: the operator's old ones described the two flip screens
		// (vflip, hvflip), both now off, and the directive makes notes priority
		// candidates. Git history has them for when a flip lane returns.
	}
}

func (p *Params) Validate() error {
	switch p.Risk {
	case "low", "medium", "high":
	default:
		return fmt.Errorf("risk must be low|medium|high")
	}
	switch p.MinConfidence {
	case "high", "medium", "low", "insufficient_history":
	default:
		return fmt.Errorf("min_confidence must be a confidence level")
	}
	if p.CapitalGp <= 0 {
		return fmt.Errorf("capital_gp must be positive")
	}
	for k, w := range p.Archetypes {
		if _, ok := archetypeNames[k]; !ok {
			return fmt.Errorf("archetypes: unknown archetype %q (F/B/V/C/U/P, retired: S/H)", k)
		}
		if w < 0 || w > 2 {
			return fmt.Errorf("archetypes[%s]: weight must be 0..2", k)
		}
	}
	return nil
}

// Render produces the brief text from params + the current scoreboard +
// the signals assigned to this run + the latest market sweep.
func Render(ctx context.Context, s *store.Store, p Params, at time.Time, assigned []store.Signal) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "_Generated %s UTC._\n\n", at.Format("2006-01-02 15:04"))

	b.WriteString("### Constraints\n")
	fmt.Fprintf(&b, "- Research budget: %s gp, a PER-OPPORTUNITY sizing scale — size every strategy independently against the full budget; the Open book does not drain it. No single strategy's capital_required may exceed it.\n", group(p.CapitalGp))
	fmt.Fprintf(&b, "- Risk appetite: %s.", p.Risk)
	if p.Members != nil {
		if *p.Members {
			b.WriteString(" Members items: allowed.")
		} else {
			b.WriteString(" F2P items only (members=false).")
		}
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "- Minimum confidence to ship a strategy: %s.\n", p.MinConfidence)
	if len(p.Archetypes) > 0 {
		b.WriteString("- Archetype weights (0 = do not pitch it at all; >1 = favor):")
		for _, a := range archetypeOrder {
			if w, ok := p.Archetypes[a]; ok {
				fmt.Fprintf(&b, " %s(%s) %.1f,", a, archetypeNames[a], w)
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("- Objective: rank ALL viable options by absolute post-tax gp/day at fillable size. The floor is absolute (C: 200k gp/cycle, P: 250k gp per hour of play, F: 400k gp/cycle = 100k gp/hr, B: 100k/cycle) — dismiss below-floor candidates whatever their ROI%. Shipping NOTHING is a legitimate outcome when nothing clears the bar.\n")
	b.WriteString("- Every F/B/C/P strategy must state its attention contract (offer cadence, longest safe unattended window, reaction risk) — the operator decides what fits their day.\n")

	cal, calErr := s.CalibrationLatest(ctx)
	if calErr == nil && len(cal) > 0 {
		modes, _ := s.FailureModeCounts(ctx, store.CalWindowDays)
		writeCalibration(&b, cal, modes)
	}

	writeOpenBook(ctx, &b, s, p)
	writeWatchlist(ctx, &b, s)
	// per status, so a backlog of open ideas can't push out the rejections the
	// directive tells the agent not to re-file
	var proposals []store.Proposal
	for _, status := range []string{"open", "rejected", "accepted"} {
		if list, err := s.Proposals(ctx, status, maxBriefProposals); err == nil {
			proposals = append(proposals, list...)
		}
	}
	writeProposals(&b, proposals)

	if len(assigned) > 0 {
		b.WriteString("\n### Assigned candidates (from the collector's work queue — investigate these FIRST)\n")
		b.WriteString("Every signal below needs a verdict in submit_report's `signal_verdicts`: `shipped` (name the strategy id) or `dismissed` (name the falsification that killed it).\n")
		for _, sig := range assigned {
			fmt.Fprintf(&b, "- signal_id %d [%s] %s (item_id %d): %s\n",
				sig.SignalID, sig.Kind, sig.ItemName, sig.ItemID, compactJSON(sig.Metrics))
			// A candidate that already died once re-enters with its obituary:
			// the run starts from the prior falsification instead of re-deriving
			// it. Best-effort — a lookup failure just omits the line.
			if at, reason, err := s.LatestDismissal(ctx, sig.Kind, sig.ItemID); err == nil {
				if line := dismissalLine(at, reason); line != "" {
					b.WriteString(line)
				}
			}
		}
	}

	writeSweep(ctx, &b, s, p, at)

	rows, err := s.Scoreboard(ctx)
	if err != nil {
		return "", err
	}
	// Only the live letter set goes to the model — retired A-G history stays
	// in the DB/dashboard but would just confuse the directive.
	live := map[string]bool{}
	for _, a := range archetypeOrder {
		live[a] = true
	}
	var liveRows []store.ScoreboardRow
	for _, r := range rows {
		if live[r.Archetype] {
			liveRows = append(liveRows, r)
		}
	}
	if len(liveRows) > 0 {
		b.WriteString("\n### Track record (paper-traded by the harness with a self-impact haircut — weight your search by it)\n")
		for _, r := range liveRows {
			closed := r.N - r.Open - r.Armed
			line := fmt.Sprintf("- %s: n=%d (%d open, %d armed)", r.Archetype, r.N, r.Open, r.Armed)
			if r.Vetoed > 0 {
				if r.Archetype == "P" {
					line += fmt.Sprintf(", %d vetoed at ship time — run process_quote last and check meta.harness says shippable, and that items[0] is its output_item, before shipping", r.Vetoed)
				} else {
					line += fmt.Sprintf(", %d vetoed at ship time — check kill_price and capital against a live quote, and that units_used x entry_price matches capital_required, before shipping", r.Vetoed)
				}
			}
			if closed > 0 {
				surv := float64(r.Confirmed) / float64(closed) * 100
				line += fmt.Sprintf(", %.0f%% of closed confirmed", surv)
			}
			if r.RealizedVsProjected != nil {
				line += fmt.Sprintf(", realized/projected %.2f", *r.RealizedVsProjected)
				line += trackAdvice(*r.RealizedVsProjected, calRow(cal, r.Archetype))
			}
			b.WriteString(line + "\n")
		}
	}

	if err := writeRecentlyClosed(ctx, &b, s); err != nil {
		return "", err
	}

	if grave, err := s.Graveyard(ctx); err == nil {
		writeGraveyard(&b, grave)
	}

	if strings.TrimSpace(p.Notes) != "" {
		b.WriteString("\n### Operator notes\n" + strings.TrimSpace(p.Notes) + "\n")
	}
	return b.String(), nil
}

// RenderEvaluate produces an evaluate run's brief: the packet the run judges, the
// signals it answers and the record it judges against. The rest of a research brief
// is what a search needs, and the packet has already been through it.
func RenderEvaluate(ctx context.Context, s *store.Store, p Params, at time.Time, assigned []store.Signal, candidates []string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "_Generated %s UTC._\n\n", at.Format("2006-01-02 15:04"))
	b.WriteString("### Constraints\n")
	fmt.Fprintf(&b, "- Research budget: %s gp per opportunity.\n", group(p.CapitalGp))
	fmt.Fprintf(&b, "- Risk appetite: %s.\n", p.Risk)
	fmt.Fprintf(&b, "- Minimum confidence to ship a strategy: %s.\n", p.MinConfidence)
	if len(assigned) > 0 {
		b.WriteString("\n### Assigned candidates\n")
		b.WriteString("Every signal below needs a verdict in submit_report's `signal_verdicts`. Each one's item leads a candidate in the packet.\n")
		for _, sig := range assigned {
			fmt.Fprintf(&b, "- signal_id %d [%s] %s (item_id %d)\n", sig.SignalID, sig.Kind, sig.ItemName, sig.ItemID)
			if at, reason, err := s.LatestDismissal(ctx, sig.Kind, sig.ItemID); err == nil {
				if line := dismissalLine(at, reason); line != "" {
					b.WriteString(line)
				}
			}
		}
	}
	fmt.Fprintf(&b, "\n### Packet (candidates: %d)\n", len(candidates))
	for _, line := range candidates {
		b.WriteString(line + "\n")
	}
	if err := writeRecentlyClosed(ctx, &b, s); err != nil {
		return "", err
	}
	if strings.TrimSpace(p.Notes) != "" {
		b.WriteString("\n### Operator notes\n" + strings.TrimSpace(p.Notes) + "\n")
	}
	return b.String(), nil
}

func writeRecentlyClosed(ctx context.Context, b *strings.Builder, s *store.Store) error {
	closedList, err := s.RecentlyClosed(ctx, 12)
	if err != nil {
		return err
	}
	if len(closedList) == 0 {
		return nil
	}
	b.WriteString("\n### Recently killed/expired (do not re-pitch without materially new evidence)\n")
	b.WriteString("How long each survived is the lesson: a kill inside a couple of hours means the pitched margin was a spike, not a standing spread.\n")
	for _, st := range closedList {
		reason := ""
		if st.StateReason != nil {
			reason = *st.StateReason
		}
		line := fmt.Sprintf("- %s [%s, item_id %d] %s", st.Sid, st.Archetype, st.PrimaryItemID, st.State)
		if st.ClosedAt != nil {
			line += fmt.Sprintf(" after %.1fh", st.ClosedAt.Sub(st.OpenedAt).Hours())
		}
		fmt.Fprintf(b, "%s: %s\n", line, reason)
	}
	return nil
}

// Ship floors, duplicated from runner/vet.go (runner imports brief, so the
// constants can't live there and be shared). The calibration section uses
// them to translate a factor into the raw claim it implies. C has no line here:
// its calibrated floor is not enforced, and a raw-claim bar would throttle it by
// instruction instead.
const (
	briefFloorFPerCycleGp = 400_000
	briefFloorBPerCycleGp = 100_000
)

// trackMinSurvive is the share of recent closes that must outlive the
// survival bar before a realized/projected ratio can read as good news. The
// ratio only measures how a ship paced while it lived: F read 1.15, "keep
// mining", through a fortnight in which 0 of its 81 closes survived 6h.
const trackMinSurvive = 0.2

// calRow finds an archetype's calibration row, nil when none exists.
func calRow(cal []store.CalibrationRow, archetype string) *store.CalibrationRow {
	for i := range cal {
		if cal[i].Archetype == archetype {
			return &cal[i]
		}
	}
	return nil
}

// trackAdvice is the track-record line's verdict on a realized/projected
// ratio, read against how many recent closes survived at all. The ratio only
// exists from store.Scoreboard's sample of 5 closes in the current epoch.
func trackAdvice(ratio float64, cal *store.CalibrationRow) string {
	switch {
	case cal != nil && cal.PSurvive != nil && *cal.PSurvive < trackMinSurvive:
		return fmt.Sprintf(" — but only %d of %d recent closes lived past %.0fh, so projections have not held; require stronger evidence before pitching.",
			cal.NSurvived, cal.NClosed, cal.SurviveHours)
	case ratio >= 0.8:
		return " — holding up, keep mining."
	case ratio <= 0.5:
		return " — projections have not held; require stronger evidence before pitching."
	}
	return ""
}

// calOrder sorts calibration rows for the brief: lanes in archetypeOrder, each
// lane's blended row, then its recipe kinds (C:combine, ...), then its families.
func calOrder(cal []store.CalibrationRow) []store.CalibrationRow {
	rank := func(key string) int {
		lane, _, _ := strings.Cut(key, ":")
		for i, a := range archetypeOrder {
			if a == lane {
				return i
			}
		}
		return len(archetypeOrder)
	}
	out := append([]store.CalibrationRow(nil), cal...)
	sort.SliceStable(out, func(i, j int) bool {
		if ri, rj := rank(out[i].Archetype), rank(out[j].Archetype); ri != rj {
			return ri < rj
		}
		if fi, fj := store.IsFamilyKey(out[i].Archetype), store.IsFamilyKey(out[j].Archetype); fi != fj {
			return fj
		}
		return out[i].Archetype < out[j].Archetype
	})
	return out
}

// writeCalibration renders the harness-measured factors (docs/FEEDBACK-LOOP.md
// §3A) and the failure-mode digest (§3B). Pure over its inputs so it is
// testable; Render only calls it when at least one factor exists.
func writeCalibration(b *strings.Builder, cal []store.CalibrationRow, modes []store.FailureModeCount) {
	b.WriteString("\n### Calibration (harness-measured from the paper record)\n")
	fmt.Fprintf(b, "- EV_calibrated = factor x your claimed EV, where factor = p_survive (share of closed ships alive past %.0fh, trailing %dd) x pace_ratio (median realized/projected of those survivors). Lane F is vetoed when its calibrated EV is under the floor, so pre-filter F with it. Lane C is not: use its rows to choose where to spend turns, and never dismiss a conversion on its factor alone.\n",
		store.CalSurviveHours, store.CalWindowDays)
	for _, r := range calOrder(cal) {
		// A family below its sample gate holds only the default factor; its kind's
		// row says more, in fewer words.
		if store.IsFamilyKey(r.Archetype) && r.PSurvive == nil && r.PaceRatio == nil {
			continue
		}
		fmt.Fprintf(b, "- %s: factor %.2f = p_survive %s x pace %s.",
			r.Archetype, r.Factor,
			calComponent(r.PSurvive, fmt.Sprintf("%d of %d closed alive", r.NSurvived, r.NClosed), r.NClosed),
			calComponent(r.PaceRatio, fmt.Sprintf("n=%d", r.NPace), r.NPace))
		var floor int64
		lane, _, _ := strings.Cut(r.Archetype, ":")
		switch lane {
		case "F":
			floor = briefFloorFPerCycleGp
		case "B":
			floor = briefFloorBPerCycleGp
		}
		if floor > 0 && r.Factor > 0 && r.Factor < 1 {
			fmt.Fprintf(b, " At this factor the %s gp/cycle floor implies a raw claim of ~%s gp/cycle to hold up.",
				group(floor), group(int64(float64(floor)/r.Factor)))
		}
		b.WriteString("\n")
	}
	if len(modes) > 0 {
		byArch := map[string][]string{}
		var order []string
		for _, m := range modes {
			if _, seen := byArch[m.Archetype]; !seen {
				order = append(order, m.Archetype)
			}
			byArch[m.Archetype] = append(byArch[m.Archetype], fmt.Sprintf("%s %d", m.Mode, m.N))
		}
		for _, a := range order {
			fmt.Fprintf(b, "- %s closes by failure mode, last %dd: %s.\n", a, store.CalWindowDays, strings.Join(byArch[a], ", "))
		}
	}
}

// calComponent renders one factor component: the value with its sample
// evidence, or the below-gate marker citing the sample that fell short.
func calComponent(v *float64, evidence string, gateN int) string {
	if v == nil {
		return fmt.Sprintf("default (below sample gate, n=%d)", gateN)
	}
	return fmt.Sprintf("%.2f (%s)", *v, evidence)
}

// writeGraveyard renders the do-not-pitch set (docs/FEEDBACK-LOOP.md §3D).
// Pure over its input; empty input writes nothing.
func writeGraveyard(b *strings.Builder, rows []store.GraveyardRow) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(b, "\n### Graveyard (do not pitch — >=%d kills and net-negative realized in the trailing %dd; lane P on kills alone)\n",
		store.GraveyardMinKills, store.GraveyardWindowDays)
	b.WriteString("The record has falsified these item x archetype pairs repeatedly. Pitch one only with a materially NEW signal class (e.g. a U event on an F corpse), and say so in the thesis.\n")
	const maxLines = 15
	for i, r := range rows {
		if i == maxLines {
			fmt.Fprintf(b, "- …and %d more.\n", len(rows)-maxLines)
			break
		}
		if r.Archetype == "P" { // realized is a pace there, not gp
			fmt.Fprintf(b, "- %s [P] (item_id %d): %d kills, last killed %s\n",
				r.ItemName, r.ItemID, r.Kills, r.LastKilledAt.Format("01-02"))
			continue
		}
		fmt.Fprintf(b, "- %s [%s] (item_id %d): %d kills, %s gp realized, last killed %s\n",
			r.ItemName, r.Archetype, r.ItemID, r.Kills, group(r.EstRealizedGp), r.LastKilledAt.Format("01-02"))
	}
}

// dismissalLine renders a re-entered candidate's prior falsification as an
// indented follow-on line, or "" when there is none.
func dismissalLine(at *time.Time, reason *string) string {
	if at == nil || reason == nil || strings.TrimSpace(*reason) == "" {
		return ""
	}
	r := strings.TrimSpace(*reason)
	if len(r) > 220 {
		// Truncate on a rune boundary — a byte slice can split a multibyte
		// character and produce invalid UTF-8 (the run-534 ingest failure).
		runes := []rune(r)
		if len(runes) > 220 {
			runes = runes[:220]
		}
		r = string(runes) + "…"
	}
	return fmt.Sprintf("  - previously dismissed %s: %s\n", at.Format("01-02"), r)
}

// writeOpenBook appends the live book (open + armed strategies) so the model
// dedups against it — informational only since the flips-first redesign:
// capital is a per-opportunity sizing scale, not a pool the book drains.
// Best-effort: a query failure just omits the section (the vetter still has
// the last word on dedup).
func writeOpenBook(ctx context.Context, b *strings.Builder, s *store.Store, p Params) {
	open, err := s.EvaluableStrategies(ctx)
	if err != nil {
		return
	}
	b.WriteString("\n### Open book (already paper-trading — informational: dedup is enforced at ingest, capital is NOT drained by it)\n")
	if len(open) == 0 {
		b.WriteString("- The book is empty.\n")
		return
	}
	b.WriteString("- Do NOT pitch an item that already has an open/armed strategy of the same archetype — it is vetoed at ingest.\n")
	const maxLines = 20
	for i, st := range open {
		if i == maxLines {
			fmt.Fprintf(b, "- …and %d more open strategies.\n", len(open)-maxLines)
			break
		}
		capital := int64(0)
		if st.Capital != nil {
			capital = *st.Capital
		}
		fmt.Fprintf(b, "- [%s] %s (item_id %d, %s): %s gp committed, opened %s\n",
			st.Archetype, firstItemName(st.Items), st.PrimaryItemID, st.State, group(capital),
			st.OpenedAt.Format("01-02"))
	}
}

// writeWatchlist appends the ranked watch portfolio: ideas that earned their
// place (operator conviction or a confirmed paper-trade) with a score that
// decays unless revalidation keeps re-proving it. Best-effort like the other
// intelligence sections.
func writeWatchlist(ctx context.Context, b *strings.Builder, s *store.Store) {
	watches, err := s.WatchRanked(ctx, 10)
	if err != nil || len(watches) == 0 {
		return
	}
	b.WriteString("\n### Watch portfolio (validated-good ideas, ranked by decayed score — strong hunting ground, but revalidate with live tools; scores decay unless re-confirmed)\n")
	for _, w := range watches {
		arch := "any"
		if w.Archetype != nil {
			arch = *w.Archetype
		}
		line := fmt.Sprintf("- %s (item_id %d, %s, score %.2f, %d/%d confirmed",
			w.ItemName, w.ItemID, arch, w.EffScore, w.TimesConfirmed, w.TimesValidated)
		if w.LastResult != nil {
			line += ", last: " + *w.LastResult
		}
		line += ")"
		if w.Note != nil && strings.TrimSpace(*w.Note) != "" {
			line += " — " + strings.TrimSpace(*w.Note)
		}
		b.WriteString(line + "\n")
	}
}

// maxBriefProposals bounds what the brief spends on ideas on file, per status.
const maxBriefProposals = 5

// briefLine keeps model or operator text on its own bullet: this list is in the
// system prompt, where a newline in a title could forge a section.
func briefLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// writeProposals renders what's already on file, so a run doesn't re-file it and can
// learn from the operator's calls. Empty input writes nothing.
func writeProposals(b *strings.Builder, list []store.Proposal) {
	if len(list) == 0 {
		return
	}
	b.WriteString("\n### Your proposals on file (don't re-file these; a rejected one needs new evidence)\n")
	for _, p := range list {
		line := fmt.Sprintf("- [%s, %s] %s", p.Kind, p.Status, briefLine(p.Title, 120))
		if p.TimesProposed > 1 {
			line += fmt.Sprintf(" (filed %d times)", p.TimesProposed)
		}
		if p.DecisionNote != nil {
			if note := briefLine(*p.DecisionNote, 300); note != "" {
				line += " — operator: " + note
			}
		}
		b.WriteString(line + "\n")
	}
}

// firstItemName digs the display name out of the stored items JSON.
func firstItemName(raw json.RawMessage) string {
	var items []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 || items[0].Name == "" {
		return "unknown item"
	}
	return items[0].Name
}

// The sweep titles for the two whole-market scans ge-agent's proxy can refuse.
// Its refusal messages quote them; keep the wording in step with ge-agent's
// internal/loop/proxy.go (precomputedScan).
const (
	sweepTitleVolumeHow = "volume anomalies vs the same hour of the week (n_baseline is thin: check it)"
	sweepTitleSeasonal  = "hour-of-week amplitude (timing evidence only — not a strategy source)"
)

// sweepMaxAge is how old a lens's rows may be before the brief flags them. The
// collector runs hourly, so past this it has failed or stalled.
const sweepMaxAge = 2 * time.Hour

// sweepHeading is a lens's line in the sweep. A stale sweep is marked up front,
// which also keeps HasPrecomputedScans from matching it.
func sweepHeading(title string, asOf, now time.Time) string {
	if now.Sub(asOf) > sweepMaxAge {
		return fmt.Sprintf("- STALE, the collector is behind: %s (as of %s):\n", title, asOf.Format("01-02 15:04"))
	}
	return fmt.Sprintf("- %s (as of %s):\n", title, asOf.Format("01-02 15:04"))
}

// HasPrecomputedScans reports whether a rendered brief carries both scans, fresh,
// the only case where the agent may be told to read them here instead of calling
// the slow tools: writeSweep skips a lens with no rows and marks a stale one.
func HasPrecomputedScans(text string) bool {
	return strings.Contains(text, "- "+sweepTitleVolumeHow) && strings.Contains(text, "- "+sweepTitleSeasonal)
}

// writeSweep appends a compact view of the collector's latest sweep so the
// model starts from accumulated intelligence instead of re-scanning cold.
// Best-effort: a missing sweep (fresh install) just omits the section.
func writeSweep(ctx context.Context, b *strings.Builder, s *store.Store, p Params, at time.Time) {
	lenses := []struct{ lens, lane, title string }{
		{"combo", "C", "lane C candidates: conversions priced both directions, ranked by budget-sized gp/4h (budget_cycle_gp — already sized to the research budget, do not dismiss on per-conversion margin)"},
		{"process", "P", "lane P candidates: processing recipes ranked by gp per hour of play at the harness's haircut (gp_hr; view_gp_hr is the screen's number, which assumes the whole market)"},
		{"vflip", "F", "lane F candidates: volume flips ranked by margin x buy_limit (gp_cycle)"},
		{"hvflip", "B", "lane B candidates: 10M+ flips ranked by absolute post-tax margin"},
		{"volume", "V", "volume anomalies vs the trailing 7 days"},
		{"volume_how", "V", sweepTitleVolumeHow},
		{"seasonal", "", sweepTitleSeasonal},
		{"band", "B", "below 21d band (lane-B qualification evidence only — not a strategy source)"},
	}
	wrote := false
	for _, l := range lenses {
		if w, ok := p.Archetypes[l.lane]; ok && w == 0 {
			continue // a lane the brief switches off gets no candidates or evidence
		}
		rows, err := s.LatestTrends(ctx, l.lens, store.BriefSweepRows(l.lens))
		if err != nil || len(rows) == 0 {
			continue
		}
		if !wrote {
			b.WriteString("\n### Latest market sweep (collector, no-LLM — re-verify anything you act on with live tools)\n")
			wrote = true
		}
		b.WriteString(sweepHeading(l.title, rows[0].AsOf, at))
		for _, r := range rows {
			fmt.Fprintf(b, "  - %s\n", compactJSON(r.Metrics))
		}
	}
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	s := buf.String()
	if len(s) > 220 {
		s = s[:220] + "…"
	}
	return s
}

func MarshalParams(p Params) json.RawMessage {
	raw, _ := json.Marshal(p)
	return raw
}

// group renders 157464000 as 157,464,000 (display only — never sent as data).
func group(n int64) string {
	if n < 0 {
		return "-" + group(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
