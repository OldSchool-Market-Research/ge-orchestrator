-- v9: the harness's own projection. The confirm ratio (EvalStats), the PnL
-- estimate and the scoreboard all divided realized by the AGENT's claimed
-- per_1h_gp. The first fortnight's record shows those claims sit ~8x above
-- the evaluator's haircut arithmetic (median winner ratio 0.12; 4 of 555
-- closed F ever touched the 0.5 confirm bar), so no strategy could confirm
-- and the scoreboard couldn't separate good lanes from bad ones.
--
-- projected_per_1h_gp is the vetter's live recomputation at ship time
-- (eval.ProjectFlipPer1h: the same slippage/tax/participation formula the
-- evaluator realizes with, applied to the strategy's own offers). Ratios
-- divide by it when present and fall back to the claim for legacy rows.

ALTER TABLE orchestrator.strategies ADD COLUMN projected_per_1h_gp bigint;

DROP VIEW orchestrator.scoreboard;
CREATE VIEW orchestrator.scoreboard AS
SELECT s.archetype,
       count(*) FILTER (WHERE s.state <> 'vetoed')             AS n,
       count(*) FILTER (WHERE s.state = 'confirmed')           AS confirmed,
       count(*) FILTER (WHERE s.state = 'killed')              AS killed,
       count(*) FILTER (WHERE s.state = 'expired')             AS expired,
       count(*) FILTER (WHERE s.state = 'open')                AS open,
       count(*) FILTER (WHERE s.state = 'armed')               AS armed,
       count(*) FILTER (WHERE s.state = 'vetoed')              AS vetoed,
       round(avg(r.ratio) FILTER (WHERE s.state NOT IN ('open','armed','vetoed'))::numeric, 2) AS realized_vs_projected
FROM orchestrator.strategies s
LEFT JOIN LATERAL (
  SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY e.realized_per_1h_gp)
         / nullif(coalesce(s.projected_per_1h_gp, s.per_1h_gp), 0) AS ratio
  FROM orchestrator.evaluations e
  WHERE e.strategy_id = s.strategy_id
    AND (s.triggered_at IS NULL OR e.at >= s.triggered_at)  -- V: armed ticks don't count
) r ON true
GROUP BY s.archetype;
