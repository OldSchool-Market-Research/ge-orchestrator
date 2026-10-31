package store

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// RelationLeg is one component of an item_relations recipe, quantities per
// single conversion.
type RelationLeg struct {
	ItemID int   `json:"item_id"`
	Qty    int64 `json:"qty"`
}

// Relation is one public.item_relations row — the hand-curated recipe a C
// strategy claims to be trading. The table is owned and seeded by ge-data;
// the orchestrator's grant is SELECT-only.
type Relation struct {
	RelationID int
	Kind       string  // decant | set | combine
	Family     *string // sub-type finer than Kind (moon, godsword, ...); nil = unlabelled
	Name       string
	Reversible bool
	Inputs     []RelationLeg
	Outputs    []RelationLeg
}

// RelationByID fetches one recipe. A missing id returns (nil, nil): for the
// vetter absence is an answer (veto), not a lookup failure (fail open).
func (s *Store) RelationByID(ctx context.Context, id int) (*Relation, error) {
	var rel Relation
	var inputs, outputs []byte
	err := s.Pool.QueryRow(ctx, `SELECT relation_id, kind, family, name, reversible, inputs, outputs
		FROM item_relations WHERE relation_id = $1`, id).
		Scan(&rel.RelationID, &rel.Kind, &rel.Family, &rel.Name, &rel.Reversible, &inputs, &outputs)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(inputs, &rel.Inputs); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(outputs, &rel.Outputs); err != nil {
		return nil, err
	}
	return &rel, nil
}

// BuyLimits returns each item's GE 4-hour buy limit; items without a known
// limit are left out of the map.
func (s *Store) BuyLimits(ctx context.Context, ids []int) (map[int]int64, error) {
	rows, err := s.Pool.Query(ctx, `SELECT item_id, buy_limit FROM items
		WHERE item_id = ANY($1) AND buy_limit > 0`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]int64{}
	for rows.Next() {
		var id int
		var limit int64
		if err := rows.Scan(&id, &limit); err != nil {
			return nil, err
		}
		out[id] = limit
	}
	return out, rows.Err()
}

// RelationNotes is a relation's curated notes: skill gates, fees, caveats.
func (s *Store) RelationNotes(ctx context.Context, id int) (*string, error) {
	var notes *string
	err := s.Pool.QueryRow(ctx, `SELECT notes FROM item_relations WHERE relation_id = $1`, id).Scan(&notes)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return notes, err
}
