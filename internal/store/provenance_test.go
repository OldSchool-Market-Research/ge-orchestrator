package store

import (
	"encoding/json"
	"testing"
)

func provStrategy(id, archetype string, item int) SidecarStrategy {
	st := SidecarStrategy{ID: id, Archetype: archetype}
	st.Items = []struct {
		Name     string `json:"name"`
		ID       int    `json:"id"`
		BuyLimit *int64 `json:"buy_limit"`
		Members  *bool  `json:"members"`
	}{{Name: "x", ID: item}}
	return st
}

func TestAttribute(t *testing.T) {
	assigned := []Signal{
		{SignalID: 1, Kind: "volume", ItemID: 100},
		{SignalID: 2, Kind: "vflip", ItemID: 100},
		{SignalID: 3, Kind: "combo", ItemID: 200, Metrics: json.RawMessage(`{"relation_id":7}`)},
		{SignalID: 4, Kind: "vflip", ItemID: 300},
	}
	relation := func(st SidecarStrategy, id int) SidecarStrategy {
		st.RelationID = &id
		return st
	}
	cases := []struct {
		name     string
		st       SidecarStrategy
		verdicts []SignalVerdict
		hits     []string
		signal   int64 // 0 = none
		lens     string
	}{
		{"the shipped verdict names it, over any item match",
			provStrategy("F-a", "F", 100),
			[]SignalVerdict{{SignalID: 4, Verdict: "shipped", Reason: "shipped as F-a"}}, nil, 4, "vflip"},
		{"a dismissed verdict naming it is no source",
			provStrategy("F-b", "F", 999),
			[]SignalVerdict{{SignalID: 4, Verdict: "dismissed", Reason: "unlike F-b"}}, nil, 0, "free"},
		{"item match prefers its own lane's kind",
			provStrategy("F-c", "F", 100), nil, nil, 2, "vflip"},
		{"item match in another lane still counts",
			provStrategy("V-d", "V", 300), nil, nil, 4, "vflip"},
		{"a conversion matches on its recipe",
			relation(provStrategy("C-e", "C", 555), 7), nil, nil, 3, "combo"},
		{"its lane's screen, when no signal matches",
			provStrategy("F-f", "F", 999), nil, []string{"seasonal", "vflip"}, 0, "sweep:vflip"},
		{"another lane's screen is not a source",
			provStrategy("C-g", "C", 999), nil, []string{"seasonal", "vflip"}, 0, "free"},
		{"the same-hour volume lens answers to V",
			provStrategy("V-h", "V", 999), nil, []string{"volume_how"}, 0, "sweep:volume_how"},
		{"a signal the run dismissed is no source",
			provStrategy("V-j", "V", 300),
			[]SignalVerdict{{SignalID: 4, Verdict: "dismissed", Reason: "spike"}}, nil, 0, "free"},
		{"nothing at all",
			provStrategy("F-i", "F", 999), nil, nil, 0, "free"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, lens := attribute(c.st, assigned, c.verdicts, c.hits)
			if lens != c.lens {
				t.Errorf("lens %q, want %q", lens, c.lens)
			}
			switch {
			case c.signal == 0 && got != nil:
				t.Errorf("signal %d, want none", *got)
			case c.signal != 0 && (got == nil || *got != c.signal):
				t.Errorf("signal %v, want %d", got, c.signal)
			}
		})
	}
}

func TestAttributeProcessRecipe(t *testing.T) {
	recipe := func(id int) *int { return &id }
	st := provStrategy("P-red-leather-20261030", "P", 2507)
	st.RecipeID = recipe(16)
	assigned := []Signal{{SignalID: 7, Kind: "process", ItemID: 9999, Metrics: json.RawMessage(`{"recipe_id":16}`)}}
	if got, lens := attribute(st, assigned, nil, nil); got == nil || *got != 7 || lens != "process" {
		t.Errorf("a signal for the same recipe: %v %q", got, lens)
	}
	if _, lens := attribute(st, nil, nil, []string{"process"}); lens != "sweep:process" {
		t.Errorf("the brief's P rows: %q", lens)
	}
	if _, lens := attribute(st, nil, nil, []string{"combo"}); lens != "free" {
		t.Errorf("another lane's rows: %q", lens)
	}
}
