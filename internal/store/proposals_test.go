package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestProposalFingerprint(t *testing.T) {
	recipe := func(in, out []RelationLeg) SidecarProposal {
		return SidecarProposal{Kind: "recipe", Title: "any wording", Recipe: &ProposedRecipe{Inputs: in, Outputs: out}}
	}
	a := recipe([]RelationLeg{{1, 2}, {3, 1}}, []RelationLeg{{9, 1}})
	b := recipe([]RelationLeg{{3, 1}, {1, 2}}, []RelationLeg{{9, 1}})
	b.Title = "a different title entirely"
	if proposalFingerprint(a) != proposalFingerprint(b) {
		t.Error("the same legs are the same recipe, whatever the wording and order")
	}
	if proposalFingerprint(a) == proposalFingerprint(recipe([]RelationLeg{{1, 3}, {3, 1}}, []RelationLeg{{9, 1}})) {
		t.Error("a different quantity is a different recipe")
	}
	lane := func(title string) string { return proposalFingerprint(SidecarProposal{Kind: "lane", Title: title}) }
	if lane("Processing  lane!") != lane("processing lane") {
		t.Error("case and punctuation don't make a new idea")
	}
	if lane("x") == proposalFingerprint(SidecarProposal{Kind: "tool", Title: "x"}) {
		t.Error("the kind is part of the idea")
	}
}

// Needs a migrated scratch DB: set STORE_TEST_DSN; skipped otherwise.
func TestProposalLifecycle(t *testing.T) {
	dsn := os.Getenv("STORE_TEST_DSN")
	if dsn == "" {
		t.Skip("STORE_TEST_DSN not set")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool.Close()
	cleanup := func() { s.Pool.Exec(ctx, `DELETE FROM orchestrator.proposals`) }
	cleanup()
	defer cleanup()

	lane := SidecarProposal{Kind: "lane", Title: "Processing lane", Detail: "d", Evidence: "e"}
	if isNew, err := s.InsertProposal(ctx, 1, lane, ""); err != nil || !isNew {
		t.Fatalf("first filing: %v %v", isNew, err)
	}
	if isNew, err := s.InsertProposal(ctx, 2, lane, ""); err != nil || isNew {
		t.Fatalf("a repeat is not new: %v %v", isNew, err)
	}
	recipe := SidecarProposal{Kind: "recipe", Title: "r", Detail: "d", Recipe: &ProposedRecipe{
		Inputs: []RelationLeg{{101, 1}}, Outputs: []RelationLeg{{102, 2}}}}
	if _, err := s.InsertProposal(ctx, 2, recipe, "margin now 5 gp"); err != nil {
		t.Fatal(err)
	}
	list, err := s.Proposals(ctx, "", 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("%d: %v", len(list), err)
	}
	byKind := map[string]Proposal{}
	for _, p := range list {
		byKind[p.Kind] = p
	}
	if byKind["lane"].TimesProposed != 2 || byKind["lane"].RunID != 2 {
		t.Errorf("lane %+v", byKind["lane"])
	}
	if byKind["recipe"].RecipeCheck == nil || *byKind["recipe"].RecipeCheck != "margin now 5 gp" || !strings.Contains(string(byKind["recipe"].Recipe), "102") {
		t.Errorf("recipe %+v", byKind["recipe"])
	}
	if byKind["lane"].RecipeCheck != nil {
		t.Errorf("no check on a lane: %v", *byKind["lane"].RecipeCheck)
	}

	id := byKind["lane"].ProposalID
	if err := s.DecideProposal(ctx, id, "rejected", "not now"); err != nil {
		t.Fatal(err)
	}
	lane.Evidence = "new evidence"
	if _, err := s.InsertProposal(ctx, 3, lane, ""); err != nil {
		t.Fatal(err)
	}
	rejected, err := s.Proposals(ctx, "rejected", 10)
	if err != nil || len(rejected) != 1 || rejected[0].TimesProposed != 3 || rejected[0].DecisionNote == nil || *rejected[0].DecisionNote != "not now" {
		t.Fatalf("a repeat counts but doesn't reopen: %+v %v", rejected, err)
	}
	if rejected[0].Evidence != "new evidence" || rejected[0].RunID != 3 {
		t.Errorf("a repeat must show its own evidence and run: %+v", rejected[0])
	}
	lane.Evidence = ""
	if _, err := s.InsertProposal(ctx, 4, lane, ""); err != nil {
		t.Fatal(err)
	}
	if again, err := s.Proposals(ctx, "rejected", 10); err != nil || again[0].Evidence != "new evidence" {
		t.Errorf("a repeat with no evidence must keep what's on file: %+v %v", again, err)
	}
	if err := s.DecideProposal(ctx, 99999999, "accepted", ""); !errors.Is(err, ErrProposalNotFound) {
		t.Errorf("unknown id: %v", err)
	}

	// A recipe already in the table is recognized in each direction it can run.
	seeded := func(reversible bool) (string, ProposedRecipe) {
		var name string
		var in, out json.RawMessage
		if err := s.Pool.QueryRow(ctx, `SELECT name, inputs, outputs FROM item_relations WHERE reversible = $1 LIMIT 1`,
			reversible).Scan(&name, &in, &out); err != nil {
			t.Skipf("no recipes seeded: %v", err)
		}
		var r ProposedRecipe
		json.Unmarshal(in, &r.Inputs)
		json.Unmarshal(out, &r.Outputs)
		return name, r
	}
	name, r := seeded(true)
	if got, err := s.RecipeOnFile(ctx, r); err != nil || got != name {
		t.Errorf("forward: %q %v, want %q", got, err, name)
	}
	if got, err := s.RecipeOnFile(ctx, ProposedRecipe{Inputs: r.Outputs, Outputs: r.Inputs}); err != nil || got != name {
		t.Errorf("reverse: %q %v, want %q", got, err, name)
	}
	if got, err := s.RecipeOnFile(ctx, ProposedRecipe{Inputs: []RelationLeg{{424242, 1}}, Outputs: r.Outputs}); err != nil || got != "" {
		t.Errorf("not on file: %q %v", got, err)
	}
	oneWay, r := seeded(false)
	if got, err := s.RecipeOnFile(ctx, r); err != nil || got != oneWay {
		t.Errorf("one-way forward: %q %v, want %q", got, err, oneWay)
	}
	if got, err := s.RecipeOnFile(ctx, ProposedRecipe{Inputs: r.Outputs, Outputs: r.Inputs}); err != nil || got != "" {
		t.Errorf("the reverse of a one-way recipe is new: %q %v", got, err)
	}
}
