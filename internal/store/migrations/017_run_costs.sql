-- Run legibility (the token-plan exhaustion, 2026-08-16): hourly runs burned
-- ~25-35M input tokens/day with no per-run record of cost or of why a run
-- happened, so the burn was invisible until MiniMax's plan ran dry mid-day.
--
-- trigger_source: why this run started. Nullable — history predates the
-- column and any backfilled label would be a guess; new runs always set it.
-- Cost columns come from the agent's <report>.stats.json (ge-agent 0.10.0),
-- written on success AND failure; null means the sidecar was absent (old
-- agent image or a crash before the first turn).

ALTER TABLE orchestrator.runs
  ADD COLUMN trigger_source    text CHECK (trigger_source IN ('schedule','signal','empty','manual')),
  ADD COLUMN turns             int,
  ADD COLUMN tool_calls        int,
  ADD COLUMN input_tokens      bigint,
  ADD COLUMN output_tokens     bigint,
  ADD COLUMN peak_input_tokens bigint,
  ADD COLUMN pruned_bytes      bigint;
