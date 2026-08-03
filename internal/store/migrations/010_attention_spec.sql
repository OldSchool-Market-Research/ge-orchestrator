-- v10: the F/B attention contract as numbers (agent 0.7.0's attention_spec
-- sidecar field): checks_per_hour and max_unattended_hours, mirroring the
-- prose attention column. The operator ping gates on cadence, and prose
-- can't be gated on. Nullable: legacy rows and non-F/B kinds have none.

ALTER TABLE orchestrator.strategies ADD COLUMN attention_spec jsonb;
