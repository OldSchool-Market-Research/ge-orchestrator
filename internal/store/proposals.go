package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// SidecarProposal mirrors ge-agent's strategy.Proposal.
type SidecarProposal struct {
	Kind     string          `json:"kind"`
	Title    string          `json:"title"`
	Detail   string          `json:"detail"`
	Evidence string          `json:"evidence"`
	Recipe   *ProposedRecipe `json:"recipe,omitempty"`
}

// ProposedRecipe is a conversion the agent believes exists, per one conversion.
type ProposedRecipe struct {
	Inputs     []RelationLeg `json:"inputs"`
	Outputs    []RelationLeg `json:"outputs"`
	Reversible bool          `json:"reversible,omitempty"`
}

// Proposal is one row of orchestrator.proposals.
type Proposal struct {
	ProposalID    int64           `json:"proposal_id"`
	RunID         int64           `json:"run_id"`
	CreatedAt     time.Time       `json:"created_at"`
	LastSeenAt    time.Time       `json:"last_seen_at"`
	Kind          string          `json:"kind"`
	Title         string          `json:"title"`
	Detail        string          `json:"detail"`
	Evidence      string          `json:"evidence"`
	Recipe        json.RawMessage `json:"recipe,omitempty"`
	RecipeCheck   *string         `json:"recipe_check,omitempty"`
	TimesProposed int             `json:"times_proposed"`
	Status        string          `json:"status"`
	DecidedAt     *time.Time      `json:"decided_at,omitempty"`
	DecisionNote  *string         `json:"decision_note,omitempty"`
}

// proposalFingerprint identifies an idea across runs: a recipe by its legs, because
// models word the same conversion differently, anything else by its normalized title.
func proposalFingerprint(p SidecarProposal) string {
	if p.Kind == "recipe" && p.Recipe != nil {
		side := func(legs []RelationLeg) string {
			parts := make([]string, len(legs))
			for i, l := range legs {
				parts[i] = fmt.Sprintf("%d*%d", l.ItemID, l.Qty)
			}
			sort.Strings(parts)
			return strings.Join(parts, "+")
		}
		return "recipe:" + side(p.Recipe.Inputs) + ">" + side(p.Recipe.Outputs)
	}
	var b strings.Builder
	for _, r := range strings.ToLower(p.Title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return p.Kind + ":" + b.String()
}

// InsertProposal files one proposal, or counts a repeat of one already on file. A
// repeat never reopens a decision, but it does replace the text: the directive
// invites a rejected idea back with new evidence, and the operator must read that,
// not the old filing. check is the harness's read of a proposed recipe, "" for other
// kinds. Returns true when the idea is new.
func (s *Store) InsertProposal(ctx context.Context, runID int64, p SidecarProposal, check string) (bool, error) {
	var recipe any
	if p.Recipe != nil {
		raw, err := json.Marshal(p.Recipe)
		if err != nil {
			return false, err
		}
		recipe = raw
	}
	var isNew bool
	err := s.Pool.QueryRow(ctx, `INSERT INTO orchestrator.proposals
			(run_id, kind, title, detail, evidence, recipe, recipe_check, fingerprint)
		VALUES ($1,$2,$3,$4,$5,$6::jsonb,nullif($7,''),$8)
		ON CONFLICT (fingerprint) DO UPDATE
			SET times_proposed = orchestrator.proposals.times_proposed + 1, last_seen_at = now(),
			    run_id = EXCLUDED.run_id, title = EXCLUDED.title, detail = EXCLUDED.detail,
			    evidence = COALESCE(NULLIF(EXCLUDED.evidence, ''), orchestrator.proposals.evidence),
			    recipe = EXCLUDED.recipe, recipe_check = EXCLUDED.recipe_check
		RETURNING (xmax = 0)`,
		runID, p.Kind, p.Title, p.Detail, p.Evidence, recipe, check, proposalFingerprint(p)).Scan(&isNew)
	return isNew, err
}

// RecipeOnFile reports whether item_relations already holds these exact legs, in
// either direction it can run: the agent can't see the whole table at once, so it may
// re-propose. The reverse of a one-way recipe is a different, new recipe.
func (s *Store) RecipeOnFile(ctx context.Context, r ProposedRecipe) (string, error) {
	inputs, err := json.Marshal(r.Inputs)
	if err != nil {
		return "", err
	}
	outputs, err := json.Marshal(r.Outputs)
	if err != nil {
		return "", err
	}
	var name string
	err = s.Pool.QueryRow(ctx, `SELECT name FROM item_relations
		WHERE (inputs @> $1::jsonb AND inputs <@ $1::jsonb AND outputs @> $2::jsonb AND outputs <@ $2::jsonb)
		   OR (reversible AND inputs @> $2::jsonb AND inputs <@ $2::jsonb AND outputs @> $1::jsonb AND outputs <@ $1::jsonb)
		LIMIT 1`, inputs, outputs).Scan(&name)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return name, err
}

// ItemNames resolves ids to names; an id with no item is left out of the map.
func (s *Store) ItemNames(ctx context.Context, ids []int) (map[int]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT item_id, name FROM items WHERE item_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var id int
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

const proposalCols = `proposal_id, run_id, created_at, last_seen_at, kind, title, detail, evidence,
	recipe, recipe_check, times_proposed, status, decided_at, decision_note`

// Proposals lists proposals, open ones first, then the most recently seen. An empty
// status means all.
func (s *Store) Proposals(ctx context.Context, status string, limit int) ([]Proposal, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+proposalCols+` FROM orchestrator.proposals
		WHERE ($1 = '' OR status = $1)
		ORDER BY (status = 'open') DESC, last_seen_at DESC LIMIT $2`, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Proposal{}
	for rows.Next() {
		var p Proposal
		if err := rows.Scan(&p.ProposalID, &p.RunID, &p.CreatedAt, &p.LastSeenAt, &p.Kind, &p.Title, &p.Detail, &p.Evidence,
			&p.Recipe, &p.RecipeCheck, &p.TimesProposed, &p.Status, &p.DecidedAt, &p.DecisionNote); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ErrProposalNotFound means no proposal has that id.
var ErrProposalNotFound = errors.New("no such proposal")

// DecideProposal records the operator's call. Re-deciding is allowed: it is their
// list. Accepting records intent only; a recipe still has to be added to ge-data's
// seed, where a person checks it.
func (s *Store) DecideProposal(ctx context.Context, id int64, status, note string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE orchestrator.proposals
		SET status=$2, decision_note=nullif($3,''), decided_at=now() WHERE proposal_id=$1`, id, status, note)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrProposalNotFound
	}
	return nil
}
