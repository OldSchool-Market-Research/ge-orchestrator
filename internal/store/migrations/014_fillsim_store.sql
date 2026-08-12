-- Fill-simulation state (fillsim phase 1, 2026-08-11). Until now the
-- evaluator had no fills at all: "realized" was the instantaneous live
-- spread marked every tick, which scores a 4h-cycle flip on 15-minute
-- snapshots (backtest of the 14 post-gate F kills: 6 had both sides
-- printing through their offers and would have completed profitable
-- cycles). These tables hold the simulator's per-strategy ledger:
-- positions carry the running inventory/cash state, fills are the
-- append-only audit trail (the FEEDBACK-LOOP frozen-snapshot rule: a
-- fill, once recorded, is never recomputed).
--
-- Dark by design: no code writes here until the evaluator ships behind
-- GE_ORCH_FILL_SIM, and every reader LEFT JOINs.

CREATE TABLE orchestrator.positions (
  strategy_id      bigint PRIMARY KEY REFERENCES orchestrator.strategies (strategy_id),
  cursor_ts        timestamptz NOT NULL,          -- end of last processed 5m bucket
  cash_gp          bigint NOT NULL DEFAULT 0,     -- signed; buys negative
  inventory        bigint NOT NULL DEFAULT 0,
  bought_units     bigint NOT NULL DEFAULT 0,
  sold_units       bigint NOT NULL DEFAULT 0,
  tax_paid_gp      bigint NOT NULL DEFAULT 0,
  window_start     timestamptz,                   -- current 4h buy-limit window
  window_bought    bigint NOT NULL DEFAULT 0,
  cycles_completed int    NOT NULL DEFAULT 0,
  last_fill_at     timestamptz,
  last_liq_low     bigint,                        -- inventory mark source
  last_liq_at      timestamptz,
  liquidated_at    timestamptz,
  final_equity_gp  bigint,
  updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE orchestrator.fills (
  fill_id       bigserial PRIMARY KEY,
  strategy_id   bigint NOT NULL REFERENCES orchestrator.strategies (strategy_id),
  bucket_ts     timestamptz NOT NULL,
  side          text NOT NULL CHECK (side IN ('buy','sell','liquidate')),
  units         bigint NOT NULL,
  price_gp      bigint NOT NULL,                  -- per-unit
  tax_gp        bigint NOT NULL DEFAULT 0,        -- total for the fill
  cash_delta_gp bigint NOT NULL,
  side_volume   bigint,                           -- the 5m volume that capped it
  UNIQUE (strategy_id, bucket_ts, side)           -- idempotent bucket replay
);

CREATE INDEX fills_strategy_idx ON orchestrator.fills (strategy_id, bucket_ts);
