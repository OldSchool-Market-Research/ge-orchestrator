-- Agent proposals (Jade's go-ahead, 2026-10-30): the agent could only search inside
-- fixed lanes and a hand-curated recipe table, so anything new it noticed stopped at
-- report prose. A proposal reaches the operator and never the book.
--
-- One row per idea: a repeat bumps times_proposed, which is itself a signal, and a
-- decided idea stays decided. recipe_check is the harness's own read of a proposed
-- recipe at filing time: the model's claim is not evidence, because a recipe missing
-- a component books phantom gp (the moon-set bug).
CREATE TABLE orchestrator.proposals (
  proposal_id    bigserial   PRIMARY KEY,
  run_id         bigint      NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now(),
  last_seen_at   timestamptz NOT NULL DEFAULT now(),
  kind           text        NOT NULL CHECK (kind IN ('recipe','lane','method','tool')),
  title          text        NOT NULL,
  detail         text        NOT NULL,
  evidence       text        NOT NULL DEFAULT '',
  recipe         jsonb,
  recipe_check   text,
  fingerprint    text        NOT NULL UNIQUE,
  times_proposed int         NOT NULL DEFAULT 1,
  status         text        NOT NULL DEFAULT 'open' CHECK (status IN ('open','accepted','rejected')),
  decided_at     timestamptz,
  decision_note  text
);
CREATE INDEX proposals_status_seen ON orchestrator.proposals (status, last_seen_at DESC);
