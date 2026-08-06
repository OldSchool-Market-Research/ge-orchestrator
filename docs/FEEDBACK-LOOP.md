# Feedback loop: the system learns, the model doesn't

Design for closing the learning loop so the agent's output converges toward
strategies that actually make gp. Drafted 2026-08-04 against the live record;
implementation is scheduled after the ~2026-08-11 check-in so the attention/C-lane
week (agent 0.7.0 / orch 0.7.1) serves as untouched baseline data.

## 1. The problem, in numbers (as of 2026-08-04)

- **0 confirmed out of 634 strategies all-time.** −85M paper realized vs +301M
  projected. Median per-strategy realized/projected: **−0.11**.
- Post-persistence-gate (Aug 1+): bleed contained (−5.4M across 70, ~77k each)
  but **69/70 killed at median 1.0h**; median realized/projected **0.01**.
- The confirm bar (80% healthy ticks AND median realized/projected ≥ 0.5,
  `eval.go`) already divides by the **harness's own ship-time projection**
  (migration 009) — and confirms are *still* zero. The participation/slippage
  haircut isn't the missing correction: the projection still assumes the
  margin persists for the whole window, and persistence is precisely what
  fails. The system can only kill or expire; it can never say "this worked."
- The finder is not broken: 37/70 recent strategies realized positive; the best
  windows print 300k+ gp/hr. What's systematically wrong is **persistence and
  magnitude estimation** — and nothing feeds that error back into the next run.

A feedback loop half-exists (`brief.go` renders the scoreboard, last-12 kills,
watchlist), but its guidance is *qualitative* ("require stronger evidence").
The agent's arithmetic never changes, the harness's gates never recalibrate,
and each run's lessons evaporate past the 12-kill window.

## 2. Mental model

The LLM is stateless — a pure function from brief to strategies. So **all
learning lives in the orchestrator's data and flows through two channels**:

```
outcomes (evaluations, closes)
        │  deterministic compilers, zero LLM in the trusted path
        ▼
┌─ calibration factors ──► brief (agent self-selects candidates that will pass)
│                      ──► vetter (enforces the same arithmetic at ingest)
│                      ──► evaluator (confirms against honest denominators)
│                      ──► notify (pings on evidence-weighted gp/hr)
└─ labeled lessons     ──► brief (failure-mode digest, graveyard, playbook)
```

Design rule carried over from `brief.go`: **the feedback loop closes with zero
LLM involvement.** The agent is *informed* by calibration; it is never trusted
to apply it. Every number the loop produces is also enforced harness-side, so
a run that ignores the brief still can't ship uncalibrated claims past the
vetter. This is also the feedback-poisoning defense: the agent cannot write to
any table the compilers read.

## 3. The four missing pieces

### A. Numeric calibration — replace advice with arithmetic

**The censoring trap first.** A strategy killed at 1h has realized ≈ 0 *by
construction* — it says "the margin didn't persist," not "the strategy was
worth 1% of its claim." A single realized/projected factor computed over
mostly-killed strategies is poisoned by this censoring and would push the
factor to ~0, teaching "ship nothing" instead of "ship honestly." Decompose:

```
EV_calibrated = P(survive) × pace_ratio × EV_raw

P(survive)  = share of ships (archetype, trailing 14d) alive past 6h
pace_ratio  = median realized/projected pace of the ships that DID survive 6h+
```

`P(survive)` calibrates *how often the thesis is real*; `pace_ratio` calibrates
*how big it is when real*. Both are per-archetype, trailing 14 days, recomputed
on every strategy close (cheap — one aggregate over `strategies`).

Guards: each component needs **n ≥ 10** closed samples or it reports `null`
and a conservative default applies (P=0.25, pace=0.5 — roughly today's
observed reality); factors clamp to [0.05, 1.2]; no per-item factors (n is
nowhere near supporting them — archetype-level only until the data grows).

Storage: `calibration` table (migration 011): archetype, window_days, n_total,
n_survived, p_survive, n_pace, pace_ratio, factor, computed_at. Append-only —
the history *is* the "is it learning?" chart.

Consumers:
- **Brief**: a `### Calibration (applied by the harness — pre-filter with it)`
  section: per archetype the factor, its components, and the resulting
  *effective raw floor* ("F factor 0.21 → a 400k-cycle floor needs a ~1.9M
  raw-arithmetic cycle to survive vetting").
- **Vetter**: computes `EV_calibrated` itself from the claim and vetoes ships
  whose calibrated per-cycle gp is below the floor. No new agent schema field —
  the harness owns the arithmetic; the agent just learns to pre-filter or
  waste its slots.
- **Notify**: ping gate becomes calibrated gp/hr ≥ `GE_ORCH_NOTIFY_MIN_GPHR`.
  Today's ping fires on raw projection — the exact number the record says is
  ~10x hot — so this change is what makes the ping worth Jade's attention.
- **Evaluator**: see C.

### B. Labeled failure modes — lessons need names

The evaluator already knows which health checks fail at kill time (the
`fails` list folded into `state_reason` free text). Structure it: a
`failure_mode` column on `strategies` (migration 012), set at close from the
dominant failing check across the kill window:

| mode | dominant signal | the lesson it encodes |
|---|---|---|
| `margin_collapse` | margin check | pitched a spike; persistence bar too generous |
| `leg_stale` | freshness check | one-sided book; freshness gate at ship was optimistic |
| `volume_dried` | volume check | size assumed volume that left with the margin |
| `entry_unreachable` / `exit_not_printing` | band checks | prices drifted off the offers |
| `stopped_out` | kill_price breach | trend risk the range-bound check missed |
| `never_triggered` | armed TTL expiry | trigger threshold miscalibrated (V) |
| `expired_below_pace` | horizon end, ratio < bar | survived but underdelivered |

Brief renders a 14-day digest per archetype ("F kills: 61% margin_collapse,
20% exit_not_printing, …") — and, for the dominant mode, one **learned
threshold**: e.g. for `margin_collapse`, the median `margin_persistence_24h`
of killed vs surviving ships. That turns the hard-coded 0.4 persistence gate
into a number the record itself keeps honest — if survivors cluster at 0.62
and corpses at 0.44, the brief says so, and bumping the vetter gate becomes a
data-backed one-line change instead of folklore.

### C. A reachable positive signal — you can't learn from wins that can't exist

Two denominator changes in the evaluator, no new machinery:

1. **Health ticks** compare the live margin against the *calibrated* projected
   margin (`marginOKFraction × factor × projection`) instead of the uncalibrated one.
2. **Confirm ratio** (0.5) is measured against calibrated projection.

Migration 009 already made the denominator the harness's own haircut
projection; the factor adds the persistence-probability component that
haircut lacks. With today's factor (~0.2), confirm requires realized ≈ 10% of
the projection —
demanding but reachable (37/70 recent ships realized positive). Kill logic
stays exactly as strict in structure (3 consecutive unhealthy ticks); only the
yardstick becomes honest. Consequences compound: confirmations start existing
→ the watchlist starts filling (its confirm-fed scoring is already built) →
`P(survive)` and `pace_ratio` gain uncensored samples → factors sharpen.
Every arrow in the loop needs this node to be reachable.

### D. Durable pattern memory — playbook and graveyard

Positive memory exists (watch portfolio, decayed scores) and starts working
once C makes confirms possible. Add the negative mirror (migration 013):

- **Graveyard**: item × archetype with ≥ 3 kills and negative cumulative
  realized in trailing 30d → 14-day do-not-pitch cooldown, **vet-enforced**
  (same veto path as open-book dedup), top offenders listed in the brief with
  their cumulative waste. Expiry is automatic; a materially new signal class
  (e.g. a U event on a graveyarded item) bypasses via a different archetype.
- **Playbook**: not a new table — the watchlist *is* it. One addition: watch
  entries carry the confirmed strategy's realized pace and attention_spec, so
  the brief's "validated-good ideas" line shows *what running it was actually
  worth per hour of Jade's attention*, not just that it confirmed.

### E. Dismissal memory — falsifications must outlive the run

Measured 2026-08-06: over 7 days, 2,031 signal dismissals vs 15
investigations, and 3,191 re-queues of 284 distinct (kind, item) pairs
within 48h of a dismissal. The agent's falsifications are recorded
(`signals.reason`) and then never consulted — most agent turns re-litigate
candidates the record already killed.

Two mechanisms, both zero-LLM:

- **Cooldown at queue time**: `UpsertSignal` skips a (kind, item) dismissed
  within `GE_ORCH_SIGNAL_DISMISS_COOLDOWN` (default 48h; negative disables).
  Watch-revalidation signals are exempt — re-proving is that lens's job.
- **Obituary at re-entry**: when a candidate does re-enter after cooldown,
  its brief line carries the prior falsification ("previously dismissed
  08-05: margin was a spike — persistence 0.08"), so the run starts from the
  verdict instead of re-deriving it.

The cooldown is deliberately dumb for now (no "unless metrics moved
materially" bypass): the collector's lenses re-detect on current data every
hour, so a genuinely changed market re-queues the moment the cooldown lapses.
If 48h proves too sticky for real regime changes, add a metric-delta bypass
then — with the re-queue stats above as the before/after.

## 4. What "making gp" means here

The system never trades; Jade does. The loop's terminal metric is therefore
**confirmed, repeatable, attention-priced strategies per week** — paper-proof
at her bar (≥250k gp/hr calibrated at ≤2 checks/hr) that she can pick up on
her own time. Paper PnL is diagnostic, not the goal. "Growing" means:

1. `median |log(realized/calibrated)|` shrinks over time — projections converge
   on reality (the direct measure that learning is happening);
2. confirm rate rises off zero and confirmed strategies re-confirm on re-runs;
3. the ping only fires when it's worth a bank trip — measured by pings/week vs
   confirms-after-ping.

Expose via `GET /api/calibration` (factor history + the three metrics) and a
dashboard panel; the weekly check-in reads this instead of raw /api/pnl.

## 5. Rollout (post Aug-11 check-in, PR-train discipline)

| phase | repo | content | risk |
|---|---|---|---|
| 1 | orch | migrations 011–013, failure-mode labeling at close, calibration compiler, brief sections, /api/calibration | none — additive, agent unchanged |
| 2 | orch | vetter calibrated-floor veto, notify calibrated gate, graveyard veto | ship volume drops (intended); ping goes quiet until quality rises |
| 3 | agent | DIRECTIVE: drop hard-coded fortnight stats, teach the calibration section + pre-filter arithmetic, failure-mode vocabulary in Discarded | prose-only |
| 4 | orch | evaluator denominator changes (health + confirm vs calibrated) | flips the confirm regime — deploy alone, watch a week |

Phase 4 is deliberately last and isolated: it changes what "confirmed" means,
and its effect must be attributable. The Aug 4→11 window is the frozen
baseline; each phase gets its own before/after on the /api/calibration chart.

## 6. Open questions for Jade

1. Survival threshold for `P(survive)`: 6h is proposed (past the 3-tick kill
   zone, short of expiry). Gut-check against how long a thesis "should" live.
2. Should the calibrated floor veto (phase 2) start in log-only mode for a few
   days ("would have vetoed N") before enforcing? — *Resolved: yes. It ships
   as `GE_ORCH_CAL_VETO_MODE=log` (default); flipping to `enforce` is a
   config change once the "would veto" lines size the rule.*
3. Graveyard cooldown 14d — too long for a market this fast?
