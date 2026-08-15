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
	Kind       string // decant | set | combine
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
	err := s.Pool.QueryRow(ctx, `SELECT relation_id, kind, name, reversible, inputs, outputs
		FROM item_relations WHERE relation_id = $1`, id).
		Scan(&rel.RelationID, &rel.Kind, &rel.Name, &rel.Reversible, &inputs, &outputs)
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
