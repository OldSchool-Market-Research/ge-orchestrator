-- v12: labeled failure modes (docs/FEEDBACK-LOOP.md §3B). state_reason is
-- free text; the brief's lesson digest needs an enum. Set at close from the
-- dominant failing health check over the kill window:
--   margin_collapse | leg_stale | volume_dried | entry_unreachable |
--   exit_not_printing | stopped_out | never_triggered | expired_below_pace
-- NULL on confirmed closes, vetoed rows, and everything closed before v12.

ALTER TABLE orchestrator.strategies ADD COLUMN failure_mode text;
