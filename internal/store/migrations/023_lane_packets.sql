-- Script-built candidate packets in log mode (evaluate/research split, 2026-10-30).
-- A run's model spends most of its requests gathering and filtering numbers a script
-- could: over runs 8687-8689 lane P shipped exactly the recipes process_quote flagged
-- shippable, 10 of 10. One row per run: every recipe's verdict from the vet's own
-- rules, as of the run's start. No run reads these rows; they're compared with what
-- the run's model pitched before anything acts on them.
CREATE TABLE orchestrator.lane_packets (
  run_id     bigint      NOT NULL REFERENCES orchestrator.runs,
  lane       text        NOT NULL CHECK (lane IN ('P')),
  mode       text        NOT NULL CHECK (mode IN ('log')),
  built_at   timestamptz NOT NULL DEFAULT now(),
  candidates jsonb       NOT NULL,
  PRIMARY KEY (run_id, lane)
);
