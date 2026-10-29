package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestProcessReads exercises the processing queries against a database with
// ge-data init/01-06 applied (and some prices/snapshots). Skips without a DSN,
// and skips when the processing schema isn't there.
func TestProcessReads(t *testing.T) {
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

	quotes, err := s.ProcessQuotes(ctx)
	if errors.Is(err, ErrProcessUnavailable) {
		t.Skip("processing schema not applied")
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(quotes) == 0 {
		t.Fatal("no process quotes")
	}

	d, err := s.ProcessDetailByID(ctx, quotes[0].RecipeID)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || len(d.Legs) == 0 || d.Quote.RecipeID != quotes[0].RecipeID {
		t.Fatalf("detail for recipe %d: %+v", quotes[0].RecipeID, d)
	}
	if d, err := s.ProcessDetailByID(ctx, -1); err != nil || d != nil {
		t.Fatalf("unknown recipe: want (nil, nil), got (%v, %v)", d, err)
	}

	since := time.Now().Add(-7 * 24 * time.Hour)
	if _, err := s.ProcessHistory(ctx, quotes[0].RecipeID, since, "1 day"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessTop(ctx, "gp_hr", since, 5, 50); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessTop(ctx, "gp_hr; DROP TABLE items", since, 5, 50); err == nil {
		t.Fatal("ProcessTop accepted a metric outside the closed set")
	}
}

// TestProcessErr pins the deploy-state mapping without a database: a missing
// relation or a missing grant is "unavailable" (503), anything else passes
// through (500).
func TestProcessErr(t *testing.T) {
	for code, want := range map[string]bool{"42P01": true, "42501": true, "42703": true, "23505": false} {
		got := errors.Is(processErr(&pgconn.PgError{Code: code}), ErrProcessUnavailable)
		if got != want {
			t.Errorf("code %s: unavailable=%v, want %v", code, got, want)
		}
	}
	if processErr(errors.New("boom")) == ErrProcessUnavailable {
		t.Error("a non-Postgres error mapped to unavailable")
	}
}

// TestProcessMalformedLeg: ge-data 06 prices a recipe with a qty-less leg as
// unpriceable (NULL margins). The detail must scan it (nil qty, nil margin),
// not fail, and nil must not become 0.
func TestProcessMalformedLeg(t *testing.T) {
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

	var id int
	err = s.Pool.QueryRow(ctx, `INSERT INTO process_recipes (name, category, inputs, outputs, throughput, source)
		VALUES ('store test: no qty', 'test', '[{"item_id":561}]', '[{"item_id":557,"qty":1}]', 'unknown', 'store_test')
		RETURNING recipe_id`).Scan(&id)
	if err != nil {
		t.Skipf("processing schema not writable here: %v", err)
	}
	defer s.Pool.Exec(ctx, `DELETE FROM process_recipes WHERE recipe_id = $1`, id)

	d, err := s.ProcessDetailByID(ctx, id)
	if err != nil {
		t.Fatalf("malformed leg must scan, got %v", err)
	}
	if d.Quote.MarginSlow != nil || d.Quote.MarginInsta != nil {
		t.Fatalf("margins must stay NULL, got %v / %v", d.Quote.MarginSlow, d.Quote.MarginInsta)
	}
	var sawNilQty bool
	for _, l := range d.Legs {
		if l.Qty == nil {
			sawNilQty = true
		}
	}
	if !sawNilQty {
		t.Fatalf("expected a leg with nil qty, got %+v", d.Legs)
	}
	if pts, err := s.ProcessHistory(ctx, -1, time.Now().Add(-time.Hour), "1 hour"); err != nil || pts != nil {
		t.Fatalf("unknown recipe history: want (nil, nil), got (%v, %v)", pts, err)
	}
}
