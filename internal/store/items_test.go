package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestItemReads exercises the item-lookup queries against a database with
// ge-data init/01 applied and some prices. Skips without a DSN.
func TestItemReads(t *testing.T) {
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

	items, err := s.Items(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Skip("no items in this database")
	}
	var priced *ItemRow
	for i := range items {
		if items[i].High != nil && items[i].Low != nil {
			priced = &items[i]
			break
		}
	}
	if priced == nil {
		t.Skip("no priced items in this database")
	}
	if priced.Margin == nil {
		t.Errorf("item %d has both prices but no margin", priced.ItemID)
	}

	d, err := s.ItemByID(ctx, priced.ItemID)
	if err != nil || d == nil || d.ItemID != priced.ItemID || d.Recipes == nil {
		t.Fatalf("detail for %d: %+v, %v", priced.ItemID, d, err)
	}
	if d, err := s.ItemByID(ctx, -1); err != nil || d != nil {
		t.Fatalf("unknown item: want (nil, nil), got (%v, %v)", d, err)
	}
	pts, err := s.ItemHistory(ctx, priced.ItemID, time.Now().Add(-90*24*time.Hour), "1 day")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pts {
		if p.HighVol == 0 && p.AvgHigh != nil {
			t.Errorf("bucket %v: avg_high %v with zero high volume", p.Bucket, *p.AvgHigh)
		}
	}
}
