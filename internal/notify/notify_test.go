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

// measured builds a calibration row whose factor came from a real sample;
// defaultRow is the below-sample shape (components nil, conservative
// default factor) that must NOT deflate the gate.
func measured(f float64) *store.CalibrationRow {
	p := 0.5
	return &store.CalibrationRow{Factor: f, PSurvive: &p}
}

func defaultRow(f float64) *store.CalibrationRow {
	return &store.CalibrationRow{Factor: f}
}

func TestClears(t *testing.T) {
	n := New(Config{MinPer1hGp: 250_000, MaxChecksPerHour: 2})
	lowTouch := &store.AttentionSpec{ChecksPerHour: 0.5, MaxUnattendedHours: 12}
	busy := &store.AttentionSpec{ChecksPerHour: 6, MaxUnattendedHours: 1}
	proj := func(v int64) *int64 { return &v }

	cases := []struct {
		name     string
		st       store.SidecarStrategy
		cal      *store.CalibrationRow
		wantPer  int64
		wantPing bool
	}{
		{"clears on claim", fixture(300_000, nil, lowTouch), nil, 300_000, true},
		{"below floor", fixture(100_000, nil, lowTouch), nil, 100_000, false},
		{"projection outranks claim", fixture(900_000, proj(120_000), lowTouch), nil, 120_000, false},
		{"projection clears", fixture(100_000, proj(400_000), lowTouch), nil, 400_000, true},
		{"too many checks", fixture(900_000, nil, busy), nil, 900_000, false},
		{"no spec never pings", fixture(900_000, nil, nil), nil, 900_000, false},
		{"exactly at both bounds", fixture(250_000, nil, &store.AttentionSpec{ChecksPerHour: 2, MaxUnattendedHours: 4}), nil, 250_000, true},
		// A MEASURED factor deflates the projection before the gate — a 900k
		// raw projection at factor 0.2 is a 180k ping-worthiness.
		{"measured factor silences a hot projection", fixture(100_000, proj(900_000), lowTouch), measured(0.2), 180_000, false},
		{"measured but still worth it", fixture(100_000, proj(2_000_000), lowTouch), measured(0.2), 400_000, true},
		// The below-sample DEFAULT factor must not gate: 0.125 would turn the
		// 250k bar into an effective 2M raw bar and the topic goes silent.
		{"default factor does not deflate", fixture(100_000, proj(400_000), lowTouch), defaultRow(0.125), 400_000, true},
		{"nil row, no correction", fixture(300_000, nil, lowTouch), nil, 300_000, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			per, calibrated, ok := n.Clears(c.st, c.cal)
			if per != c.wantPer || ok != c.wantPing {
				t.Fatalf("Clears = (%d, %v, %v), want (%d, _, %v)", per, calibrated, ok, c.wantPer, c.wantPing)
			}
			wantCal := c.cal != nil && (c.cal.PSurvive != nil || c.cal.PaceRatio != nil)
			if calibrated != wantCal {
				t.Fatalf("calibrated = %v, want %v", calibrated, wantCal)
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

	n.StrategyShipped(context.Background(), fixture(300_000, nil, spec), nil)
	if calls != 1 {
		t.Fatalf("want 1 ntfy POST, got %d", calls)
	}
	if !strings.Contains(gotTitle, "Adamantite bar") || !strings.Contains(gotTitle, "300k") {
		t.Fatalf("title = %q", gotTitle)
	}
	for _, want := range []string{"F-adamantite-bar-20260803", "buy 1908 -> sell 1987", "0.5 checks/hr", "21M gp", "uncalibrated"} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("body missing %q:\n%s", want, gotBody)
		}
	}

	n.StrategyShipped(context.Background(), fixture(100_000, nil, spec), nil)
	if calls != 1 {
		t.Fatal("below-bar strategy must not POST")
	}
}

func TestStrategyConfirmedPosts(t *testing.T) {
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
	st := store.Strategy{
		Sid:        "F-adamantite-bar-20260803",
		Title:      "Adamantite bar volume flip",
		EntryPrice: 1908, ExitPrice: 1987,
		Items:         []byte(`[{"name":"Adamantite bar","id":2361}]`),
		AttentionSpec: &store.AttentionSpec{ChecksPerHour: 0.5, MaxUnattendedHours: 12},
	}

	n.StrategyConfirmed(context.Background(), st, 300_000)
	if calls != 1 {
		t.Fatalf("want 1 ntfy POST, got %d", calls)
	}
	if !strings.Contains(gotTitle, "CONFIRMED") || !strings.Contains(gotTitle, "Adamantite bar") {
		t.Fatalf("title = %q", gotTitle)
	}
	if !strings.Contains(gotBody, "realized") {
		t.Fatalf("body = %q", gotBody)
	}

	n.StrategyConfirmed(context.Background(), st, 100_000)
	if calls != 1 {
		t.Fatal("below-bar confirm must not POST")
	}
	busy := st
	busy.AttentionSpec = &store.AttentionSpec{ChecksPerHour: 6, MaxUnattendedHours: 1}
	n.StrategyConfirmed(context.Background(), busy, 900_000)
	if calls != 1 {
		t.Fatal("over-attention confirm must not POST")
	}
}

func TestGp(t *testing.T) {
	for v, want := range map[int64]string{987: "987", 47_000: "47k", 1_234_567: "1.2M", 21_032_000: "21M", 250_000: "250k"} {
		if got := gp(v); got != want {
			t.Errorf("gp(%d) = %q, want %q", v, got, want)
		}
	}
}
