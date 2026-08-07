package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

func fixture(per1h int64, projected *int64, spec *store.AttentionSpec) store.SidecarStrategy {
	st := store.SidecarStrategy{
		ID:              "F-adamantite-bar-20260803",
		Archetype:       "F",
		Title:           "Adamantite bar volume flip",
		EntryPrice:      1908,
		ExitPrice:       1987,
		CapitalRequired: 21_032_000,
		AttentionSpec:   spec,
	}
	st.Items = append(st.Items, struct {
		Name     string `json:"name"`
		ID       int    `json:"id"`
		BuyLimit *int64 `json:"buy_limit"`
		Members  *bool  `json:"members"`
	}{Name: "Adamantite bar", ID: 2361})
	st.ExpectedValue.Per1hGp = per1h
	st.HarnessProjectedPer1h = projected
	return st
}

func TestClears(t *testing.T) {
	n := New(Config{MinPer1hGp: 250_000, MaxChecksPerHour: 2})
	lowTouch := &store.AttentionSpec{ChecksPerHour: 0.5, MaxUnattendedHours: 12}
	busy := &store.AttentionSpec{ChecksPerHour: 6, MaxUnattendedHours: 1}
	proj := func(v int64) *int64 { return &v }

	cases := []struct {
		name     string
		st       store.SidecarStrategy
		factor   float64
		wantPer  int64
		wantPing bool
	}{
		{"clears on claim", fixture(300_000, nil, lowTouch), 1, 300_000, true},
		{"below floor", fixture(100_000, nil, lowTouch), 1, 100_000, false},
		{"projection outranks claim", fixture(900_000, proj(120_000), lowTouch), 1, 120_000, false},
		{"projection clears", fixture(100_000, proj(400_000), lowTouch), 1, 400_000, true},
		{"too many checks", fixture(900_000, nil, busy), 1, 900_000, false},
		{"no spec never pings", fixture(900_000, nil, nil), 1, 900_000, false},
		{"exactly at both bounds", fixture(250_000, nil, &store.AttentionSpec{ChecksPerHour: 2, MaxUnattendedHours: 4}), 1, 250_000, true},
		// Calibration: the record's factor deflates the projection before the
		// gate — a 900k raw projection at factor 0.2 is a 180k ping-worthiness.
		{"factor silences a hot projection", fixture(100_000, proj(900_000), lowTouch), 0.2, 180_000, false},
		{"factor-deflated but still worth it", fixture(100_000, proj(2_000_000), lowTouch), 0.2, 400_000, true},
		{"factor 0 means unmeasured, no correction", fixture(300_000, nil, lowTouch), 0, 300_000, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			per, ok := n.Clears(c.st, c.factor)
			if per != c.wantPer || ok != c.wantPing {
				t.Fatalf("Clears = (%d, %v), want (%d, %v)", per, ok, c.wantPer, c.wantPing)
			}
		})
	}
}

func TestStrategyShippedPosts(t *testing.T) {
	var gotTitle, gotBody string
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotTitle = r.Header.Get("Title")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	}))
	defer srv.Close()

	n := New(Config{URL: srv.URL, MinPer1hGp: 250_000, MaxChecksPerHour: 2})
	spec := &store.AttentionSpec{ChecksPerHour: 0.5, MaxUnattendedHours: 12}

	n.StrategyShipped(context.Background(), fixture(300_000, nil, spec), 1)
	if calls != 1 {
		t.Fatalf("want 1 ntfy POST, got %d", calls)
	}
	if !strings.Contains(gotTitle, "Adamantite bar") || !strings.Contains(gotTitle, "300k") {
		t.Fatalf("title = %q", gotTitle)
	}
	for _, want := range []string{"F-adamantite-bar-20260803", "buy 1908 -> sell 1987", "0.5 checks/hr", "21M gp"} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("body missing %q:\n%s", want, gotBody)
		}
	}

	n.StrategyShipped(context.Background(), fixture(100_000, nil, spec), 1)
	if calls != 1 {
		t.Fatal("below-bar strategy must not POST")
	}
}

func TestGp(t *testing.T) {
	for v, want := range map[int64]string{987: "987", 47_000: "47k", 1_234_567: "1.2M", 21_032_000: "21M", 250_000: "250k"} {
		if got := gp(v); got != want {
			t.Errorf("gp(%d) = %q, want %q", v, got, want)
		}
	}
}
