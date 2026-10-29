package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
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
