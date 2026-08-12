package store

import (
	"context"
	"time"
)

// Position is one strategy's fill-simulation ledger state: running cash and
// inventory, the current 4h buy-limit window, and the bucket cursor that
// makes replay idempotent. Rows exist only for strategies the simulator
// trades (epoch-2 F); everything else never touches this table.
type Position struct {
	StrategyID      int64      `json:"strategy_id"`
	CursorTs        time.Time  `json:"cursor_ts"`
	CashGp          int64      `json:"cash_gp"`
	Inventory       int64      `json:"inventory"`
	BoughtUnits     int64      `json:"bought_units"`
	SoldUnits       int64      `json:"sold_units"`
	TaxPaidGp       int64      `json:"tax_paid_gp"`
	WindowStart     *time.Time `json:"window_start,omitempty"`
	WindowBought    int64      `json:"window_bought"`
	CyclesCompleted int        `json:"cycles_completed"`
	LastFillAt      *time.Time `json:"last_fill_at,omitempty"`
	LastLiqLow      *int64     `json:"last_liq_low,omitempty"`
	LastLiqAt       *time.Time `json:"last_liq_at,omitempty"`
	LiquidatedAt    *time.Time `json:"liquidated_at,omitempty"`
	FinalEquityGp   *int64     `json:"final_equity_gp,omitempty"`
}

// Fill is one append-only ledger entry. bucket_ts is the 5m bucket the fill
// was simulated in (closed_at for forced liquidations); the UNIQUE
// (strategy_id, bucket_ts, side) constraint makes bucket replay after a
// restart insert-once.
type Fill struct {
	StrategyID  int64     `json:"strategy_id"`
	BucketTs    time.Time `json:"bucket_ts"`
	Side        string    `json:"side"` // buy | sell | liquidate
	Units       int64     `json:"units"`
	PriceGp     int64     `json:"price_gp"` // per unit
	TaxGp       int64     `json:"tax_gp"`   // total for the fill
	CashDeltaGp int64     `json:"cash_delta_gp"`
	SideVolume  *int64    `json:"side_volume,omitempty"`
}

const positionCols = `strategy_id, cursor_ts, cash_gp, inventory, bought_units, sold_units,
	tax_paid_gp, window_start, window_bought, cycles_completed, last_fill_at,
	last_liq_low, last_liq_at, liquidated_at, final_equity_gp`

// Position returns a strategy's ledger state, nil when the simulator has
// never traded it.
func (s *Store) Position(ctx context.Context, strategyID int64) (*Position, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+positionCols+`
		FROM orchestrator.positions WHERE strategy_id = $1`, strategyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var p Position
	if err := scanPosition(rows.Scan, &p); err != nil {
		return nil, err
	}
	return &p, rows.Err()
}

func scanPosition(scan func(...any) error, p *Position) error {
	return scan(&p.StrategyID, &p.CursorTs, &p.CashGp, &p.Inventory, &p.BoughtUnits,
		&p.SoldUnits, &p.TaxPaidGp, &p.WindowStart, &p.WindowBought, &p.CyclesCompleted,
		&p.LastFillAt, &p.LastLiqLow, &p.LastLiqAt, &p.LiquidatedAt, &p.FinalEquityGp)
}

// AdvancePosition persists one tick's simulation outcome atomically: the
// updated ledger state plus that tick's fills. Fill inserts are ON CONFLICT
// DO NOTHING — a replayed bucket (restart, crash between insert and cursor
// write) records nothing twice; the ledger upsert then makes the cursor
// authoritative. A fill, once recorded, is never recomputed.
func (s *Store) AdvancePosition(ctx context.Context, p *Position, fills []Fill) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO orchestrator.positions
		(strategy_id, cursor_ts, cash_gp, inventory, bought_units, sold_units,
		 tax_paid_gp, window_start, window_bought, cycles_completed, last_fill_at,
		 last_liq_low, last_liq_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (strategy_id) DO UPDATE SET
		 cursor_ts = EXCLUDED.cursor_ts, cash_gp = EXCLUDED.cash_gp,
		 inventory = EXCLUDED.inventory, bought_units = EXCLUDED.bought_units,
		 sold_units = EXCLUDED.sold_units, tax_paid_gp = EXCLUDED.tax_paid_gp,
		 window_start = EXCLUDED.window_start, window_bought = EXCLUDED.window_bought,
		 cycles_completed = EXCLUDED.cycles_completed, last_fill_at = EXCLUDED.last_fill_at,
		 last_liq_low = EXCLUDED.last_liq_low, last_liq_at = EXCLUDED.last_liq_at,
		 updated_at = now()`,
		p.StrategyID, p.CursorTs, p.CashGp, p.Inventory, p.BoughtUnits, p.SoldUnits,
		p.TaxPaidGp, p.WindowStart, p.WindowBought, p.CyclesCompleted, p.LastFillAt,
		p.LastLiqLow, p.LastLiqAt); err != nil {
		return err
	}
	for _, f := range fills {
		if _, err := tx.Exec(ctx, `INSERT INTO orchestrator.fills
			(strategy_id, bucket_ts, side, units, price_gp, tax_gp, cash_delta_gp, side_volume)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (strategy_id, bucket_ts, side) DO NOTHING`,
			f.StrategyID, f.BucketTs, f.Side, f.Units, f.PriceGp, f.TaxGp,
			f.CashDeltaGp, f.SideVolume); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// FinalizePosition closes the ledger on kill/expire: records the forced
// liquidation fill (nil when the position closed flat) and freezes
// final_equity_gp — the number PnL and the graveyard report for sim-scored
// strategies from then on.
func (s *Store) FinalizePosition(ctx context.Context, strategyID int64, at time.Time, finalEquityGp int64, liq *Fill) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if liq != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO orchestrator.fills
			(strategy_id, bucket_ts, side, units, price_gp, tax_gp, cash_delta_gp, side_volume)
			VALUES ($1,$2,'liquidate',$3,$4,$5,$6,$7)
			ON CONFLICT (strategy_id, bucket_ts, side) DO NOTHING`,
			liq.StrategyID, liq.BucketTs, liq.Units, liq.PriceGp, liq.TaxGp,
			liq.CashDeltaGp, liq.SideVolume); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE orchestrator.positions
		SET liquidated_at = $2, final_equity_gp = $3, inventory = 0, updated_at = now()
		WHERE strategy_id = $1 AND liquidated_at IS NULL`, strategyID, at, finalEquityGp); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Fills returns a strategy's ledger entries in bucket order (the audit
// trail behind its PnL row).
func (s *Store) Fills(ctx context.Context, strategyID int64) ([]Fill, error) {
	rows, err := s.Pool.Query(ctx, `SELECT strategy_id, bucket_ts, side, units, price_gp,
		tax_gp, cash_delta_gp, side_volume
		FROM orchestrator.fills WHERE strategy_id = $1 ORDER BY bucket_ts, side`, strategyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Fill
	for rows.Next() {
		var f Fill
		if err := rows.Scan(&f.StrategyID, &f.BucketTs, &f.Side, &f.Units, &f.PriceGp,
			&f.TaxGp, &f.CashDeltaGp, &f.SideVolume); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
