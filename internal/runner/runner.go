// Package runner owns ge-agent execution: one run at a time, per-run
// workspace, brief delivery, stderr -> SSE events, sidecar ingestion.
package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/osrs-ge/ge-orchestrator/internal/brief"
	"github.com/osrs-ge/ge-orchestrator/internal/eval"
	"github.com/osrs-ge/ge-orchestrator/internal/notify"
	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

type Config struct {
	AgentPath string // GE_AGENT_PATH
	McpPath   string // GE_MCP_PATH
	McpDSN    string // GE_MCP_DSN
	Directive string // GE_AGENT_DIRECTIVE (absolute path)
	StateDir  string // GE_ORCH_STATE — run workspaces live under here
	ExtraEnv  []string

	// CalVetoEnforce names the lanes whose calibrated-floor rule is a real
	// veto (GE_ORCH_CAL_VETO_ENFORCE, default F); every other lane only logs
	// the "would veto" lines that size the rule before it costs any ships
	// (FEEDBACK-LOOP.md §6 Q2).
	CalVetoEnforce map[string]bool
	// GraveyardCooldown is how long after its last kill a graveyarded
	// item×archetype stays unpitchable (GE_ORCH_GRAVEYARD_COOLDOWN,
	// default 14d; 0 disables the veto).
	GraveyardCooldown time.Duration
}

type Runner struct {
	Cfg    Config
	Store  *store.Store
	Hub    *Hub
	Prices eval.PriceSource // ship-time vetting; nil skips the kill-price rule
	Notify *notify.Notifier // operator ping on above-bar ships; nil disables

	mu     sync.Mutex
	active *activeRun
}

type activeRun struct {
	runID       int64
	params      brief.Params
	precomputed bool // the brief carries the whole-market scans
	done        chan struct{}
}

// ActiveRunID returns the in-flight run id, or 0.
func (r *Runner) ActiveRunID() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		return 0
	}
	return r.active.runID
}

// Trigger starts a run with the given brief params. source records why the
// run happened (schedule|signal|empty|manual) — run legibility starts with
// knowing what woke the agent. Returns the run id, or ErrBusy if one is
// already in flight.
var ErrBusy = fmt.Errorf("a run is already in progress")

func (r *Runner) Trigger(ctx context.Context, p brief.Params, source string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != nil {
		return r.active.runID, ErrBusy
	}

	// Pull pending signals into this run's brief, shared out by lane weight —
	// the work-queue mechanism that makes consecutive runs investigate different
	// candidates.
	assigned, err := r.Store.NextSignals(ctx, p.Archetypes, 10)
	if err != nil {
		return 0, err
	}
	briefText, err := brief.Render(ctx, r.Store, p, time.Now().UTC(), assigned)
	if err != nil {
		return 0, err
	}
	runID, err := r.Store.CreateRun(ctx, brief.MarshalParams(p), briefText, source)
	if err != nil {
		return 0, err
	}
	if len(assigned) > 0 {
		ids := make([]int64, len(assigned))
		for i, sig := range assigned {
			ids[i] = sig.SignalID
		}
		if err := r.Store.AssignSignals(ctx, runID, ids); err != nil {
			return 0, err
		}
	}

	workspace := filepath.Join(r.Cfg.StateDir, "runs", fmt.Sprintf("%d", runID))
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return 0, err
	}
	briefPath := filepath.Join(workspace, "brief.md")
	if err := os.WriteFile(briefPath, []byte(briefText), 0o644); err != nil {
		return 0, err
	}

	run := &activeRun{runID: runID, params: p, precomputed: brief.HasPrecomputedScans(briefText), done: make(chan struct{})}
	r.active = run
	go r.execute(runID, workspace, briefPath, run)
	return runID, nil
}

// execute runs the child to completion (detached from the trigger ctx — an
// HTTP disconnect must not kill a research run).
func (r *Runner) execute(runID int64, workspace, briefPath string, run *activeRun) {
	defer func() {
		r.mu.Lock()
		r.active = nil
		r.mu.Unlock()
		close(run.done)
	}()
	ctx := context.Background()

	r.logProcessPacket(runID, run.params)

	cmd := exec.Command(r.Cfg.AgentPath)
	cmd.Dir = workspace
	cmd.Env = append([]string{
		"GE_MCP_PATH=" + r.Cfg.McpPath,
		"GE_MCP_DSN=" + r.Cfg.McpDSN,
		"GE_AGENT_DIRECTIVE=" + r.Cfg.Directive,
		"GE_AGENT_BRIEF_FILE=" + briefPath,
	}, r.Cfg.ExtraEnv...)
	if run.precomputed {
		cmd.Env = append(cmd.Env, "GE_AGENT_PRECOMPUTED_SCANS=1")
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		r.fail(ctx, runID, "stderr pipe: "+err.Error())
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		r.fail(ctx, runID, "stdout pipe: "+err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		r.fail(ctx, runID, "start ge-agent: "+err.Error())
		return
	}
	r.Hub.Publish(runID, Event{Type: "started", Data: map[string]any{"run_id": runID}})

	// stderr -> events
	var lastLines []string
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			lastLines = append(lastLines, line)
			if len(lastLines) > 20 {
				lastLines = lastLines[1:]
			}
			r.Hub.Publish(runID, parseLine(line))
		}
	}()

	var stdoutBuf strings.Builder
	scOut := bufio.NewScanner(stdout)
	for scOut.Scan() {
		stdoutBuf.WriteString(scOut.Text() + "\n")
	}

	err = cmd.Wait()
	// Cost record lands whatever the outcome — failed runs burn tokens too,
	// and that is exactly when the burn must stay visible (run 924 burned 31
	// minutes with no record; the plan died the same day).
	r.ingestStats(ctx, runID, workspace)
	reportPath := strings.TrimSpace(stdoutBuf.String())
	if err != nil || reportPath == "" {
		reason := "ge-agent exited without a report"
		if err != nil {
			reason = fmt.Sprintf("ge-agent failed: %v; tail: %s", err, strings.Join(lastLines, " | "))
		}
		r.fail(ctx, runID, reason)
		return
	}

	if err := r.ingest(ctx, runID, run.params, workspace, reportPath); err != nil {
		r.fail(ctx, runID, "ingest: "+err.Error())
		return
	}
	r.Hub.Publish(runID, Event{Type: "finished", Data: map[string]any{"run_id": runID, "status": "succeeded"}})
	log.Printf("run %d succeeded: %s", runID, reportPath)
}

// ingestStats persists the agent's cost sidecar (<report>.stats.json,
// ge-agent >= 0.10.0). The per-run workspace holds exactly one run's
// artifacts, so a glob finds it without knowing the report basename — which
// stdout does not carry on failure. Absence is fine (old agent image, crash
// before turn 1); a parse or DB error is log-worthy, never run-fatal.
func (r *Runner) ingestStats(ctx context.Context, runID int64, workspace string) {
	matches, err := filepath.Glob(filepath.Join(workspace, "reports", "*.stats.json"))
	if err != nil || len(matches) == 0 {
		return
	}
	raw, err := os.ReadFile(matches[len(matches)-1])
	if err != nil {
		log.Printf("run %d: read stats sidecar: %v", runID, err)
		return
	}
	var c store.RunCosts
	if err := json.Unmarshal(raw, &c); err != nil {
		log.Printf("run %d: parse stats sidecar: %v", runID, err)
		return
	}
	if err := r.Store.SetRunCosts(ctx, runID, c); err != nil {
		log.Printf("run %d: persist run costs: %v", runID, err)
		return
	}
	log.Printf("run %d: cost %d turns, %d tool calls, %d in / %d out tokens (peak %d, pruned %dB)",
		runID, c.Turns, c.ToolCalls, c.InputTokens, c.OutputTokens, c.PeakInputTokens, c.PrunedBytes)
}

// ingest stores the report markdown + parses the sidecar strategies.
func (r *Runner) ingest(ctx context.Context, runID int64, p brief.Params, workspace, reportPath string) error {
	full := reportPath
	if !filepath.IsAbs(full) {
		full = filepath.Join(workspace, reportPath)
	}
	md, err := os.ReadFile(full)
	if err != nil {
		return fmt.Errorf("read report: %w", err)
	}
	scPath := strings.TrimSuffix(full, ".md") + ".strategies.json"
	raw, err := os.ReadFile(scPath)
	if err != nil {
		return fmt.Errorf("read sidecar: %w", err)
	}
	var sc store.Sidecar
	if err := json.Unmarshal(raw, &sc); err != nil {
		return fmt.Errorf("parse sidecar: %w", err)
	}
	now := time.Now().UTC()
	// Provenance needs the run's signals before the verdicts below resolve them.
	// Best-effort: a lost attribution must not cost the run its ships.
	if err := r.Store.AttachProvenance(ctx, runID, sc.RunStartedAt, sc.Strategies, sc.SignalVerdicts); err != nil {
		log.Printf("run %d: provenance: %v", runID, err)
	}
	accepted, vetoed := r.vet(ctx, p, sc.Strategies)
	for _, v := range vetoed {
		log.Printf("run %d: strategy %s %s", runID, v.Strategy.ID, v.Reason)
		r.Hub.Publish(runID, Event{Type: "vetoed", Data: map[string]any{"sid": v.Strategy.ID, "reason": v.Reason}})
	}
	if err := r.Store.InsertStrategies(ctx, runID, now, accepted, vetoed); err != nil {
		return err
	}
	if err := r.Store.LinkSignalStrategies(ctx, runID); err != nil {
		log.Printf("run %d: link signals: %v", runID, err)
	}
	r.logPacketComparison(ctx, runID)
	if r.Notify != nil {
		// The ping gate weighs projections by the same calibration rows
		// the vetter just used — a re-fetch keeps the coupling loose and the
		// cost is one indexed query per ingest. The full row (not just the
		// factor) lets the gate skip calibration when it's only a default.
		rec := r.loadRecord(ctx)
		for _, st := range accepted {
			r.Notify.StrategyShipped(ctx, st, rec.CalRowFor(st))
		}
	}
	// Apply the run's verdicts on its assigned signals, then return any it
	// ignored to the queue (a verdict-less signal must not rot as 'assigned').
	for _, v := range sc.SignalVerdicts {
		if err := r.Store.ResolveSignal(ctx, int64(v.SignalID), v.Verdict, v.Reason); err != nil {
			log.Printf("run %d: resolve signal %d: %v", runID, v.SignalID, err)
		}
	}
	if err := r.Store.ReleaseRunSignals(ctx, runID); err != nil {
		log.Printf("run %d: release unanswered signals: %v", runID, err)
	}
	if sc.ProposalsDropped > 0 {
		log.Printf("run %d: agent dropped %d malformed or excess proposal(s)", runID, sc.ProposalsDropped)
	}
	r.fileProposals(ctx, runID, sc.Proposals)
	return r.Store.FinishRun(ctx, runID, "succeeded", full, string(md), "")
}

// fileProposals stores what the agent suggested. Best-effort: a proposal never
// costs a run its ships. A recipe already in the table is dropped, since the agent
// can't see the whole table at once.
func (r *Runner) fileProposals(ctx context.Context, runID int64, list []store.SidecarProposal) {
	for _, p := range list {
		check := ""
		if p.Kind == "recipe" && p.Recipe != nil {
			name, err := r.Store.RecipeOnFile(ctx, *p.Recipe)
			if err != nil {
				log.Printf("run %d: proposal %q: recipe lookup: %v", runID, p.Title, err)
			} else if name != "" {
				log.Printf("run %d: proposal %q is already recipe %q, dropped", runID, p.Title, name)
				continue
			}
			check = r.checkRecipe(ctx, *p.Recipe)
		}
		isNew, err := r.Store.InsertProposal(ctx, runID, p, check)
		if err != nil {
			log.Printf("run %d: proposal %q: %v", runID, p.Title, err)
			continue
		}
		log.Printf("run %d: proposal %q (%s, new=%v)", runID, p.Title, p.Kind, isNew)
	}
}

// checkRecipe is the harness's own read of a proposed recipe, for the person
// deciding: do its items exist, and what does it pay right now? The model's claim
// isn't evidence. A margin says nothing about whether the recipe is real, and a
// missing component is what makes a phantom one look rich.
func (r *Runner) checkRecipe(ctx context.Context, rec store.ProposedRecipe) string {
	var ids []int
	for _, l := range append(append([]store.RelationLeg{}, rec.Inputs...), rec.Outputs...) {
		ids = append(ids, l.ItemID)
	}
	names, err := r.Store.ItemNames(ctx, ids)
	if err != nil {
		return "item lookup failed: " + err.Error()
	}
	var unknown []string
	for _, id := range ids {
		if _, ok := names[id]; !ok {
			unknown = append(unknown, fmt.Sprintf("%d", id))
		}
	}
	if len(unknown) > 0 {
		return "unknown item ids: " + strings.Join(unknown, ", ")
	}
	describe := func(legs []store.RelationLeg) string {
		parts := make([]string, len(legs))
		for i, l := range legs {
			parts[i] = fmt.Sprintf("%d x %s", l.Qty, names[l.ItemID])
		}
		return strings.Join(parts, " + ")
	}
	out := describe(rec.Inputs) + " -> " + describe(rec.Outputs)
	if r.Prices == nil {
		return out
	}
	snaps, err := r.Prices.SnapshotMany(ctx, ids)
	if err != nil {
		return out + "; pricing failed: " + err.Error()
	}
	if margin, ok := eval.RecipeMargin(rec.Inputs, rec.Outputs, snaps); ok {
		return fmt.Sprintf("%s; margin now %d gp per conversion (buy at low, sell at high, after tax)", out, margin)
	}
	return out + "; a leg has no price right now"
}

func (r *Runner) fail(ctx context.Context, runID int64, reason string) {
	log.Printf("run %d failed: %s", runID, reason)
	if err := r.Store.ReleaseRunSignals(ctx, runID); err != nil {
		log.Printf("run %d: release signals: %v", runID, err)
	}
	if err := r.Store.FinishRun(ctx, runID, "failed", "", "", reason); err != nil {
		log.Printf("run %d: record failure: %v", runID, err)
	}
	r.Hub.Publish(runID, Event{Type: "finished", Data: map[string]any{"run_id": runID, "status": "failed", "reason": reason}})
}

// --- stderr line parsing (the loop's known formats; fallback = log event) ---

var (
	turnRe   = regexp.MustCompile(`turn (\d+): stop=(\S+) in=(\d+) out=(\d+)`)
	toolRe   = regexp.MustCompile(`\s+tool (\S+) \(err=(\w+), (\d+) bytes\)`)
	acceptRe = regexp.MustCompile(`report (accepted|rejected): (.*)`)
)

func parseLine(line string) Event {
	if m := turnRe.FindStringSubmatch(line); m != nil {
		return Event{Type: "turn", Data: map[string]any{"n": m[1], "stop": m[2], "in": m[3], "out": m[4]}}
	}
	if m := toolRe.FindStringSubmatch(line); m != nil {
		return Event{Type: "tool", Data: map[string]any{"name": m[1], "err": m[2] == "true", "bytes": m[3]}}
	}
	if m := acceptRe.FindStringSubmatch(line); m != nil {
		return Event{Type: "report_" + m[1], Data: map[string]any{"detail": m[2]}}
	}
	return Event{Type: "log", Data: map[string]any{"line": line}}
}
