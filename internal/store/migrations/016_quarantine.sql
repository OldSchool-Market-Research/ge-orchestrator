-- Quarantine (the moon-set incident, 2026-08-14): a wrong item_relations
-- recipe (three of the set's four components) booked ~218M phantom realized
-- across strategies 1291/1293 — numbers that were measurement garbage, not
-- trading outcomes. Non-null quarantined_reason excludes a strategy from
-- every record-fed aggregate (calibration, graveyard, scoreboard, P&L
-- rollups); the row itself stays listed, annotated with the reason — the
-- record shows the break instead of hiding it (the migration 015 rule).
--
-- Rows are quarantined by hand (operator SQL), never by the service.

ALTER TABLE orchestrator.strategies ADD COLUMN quarantined_reason text;

-- Same view as migration 009, minus quarantined rows.
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
WHERE s.quarantined_reason IS NULL
GROUP BY s.archetype;
