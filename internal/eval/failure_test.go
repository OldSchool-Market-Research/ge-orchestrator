package eval

import (
	"encoding/json"
	"testing"

	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

func evalWithChecks(t *testing.T, checks map[string]bool) store.Evaluation {
	t.Helper()
	raw, err := json.Marshal(checks)
	if err != nil {
		t.Fatal(err)
	}
	return store.Evaluation{Checks: raw}
}

func TestDominantFailureMode(t *testing.T) {
	cases := []struct {
		name  string
		evals []store.Evaluation
		want  string // "" = nil expected
	}{
		{"no evals", nil, ""},
		{"legacy empty checks", []store.Evaluation{{}}, ""},
		{"all healthy", []store.Evaluation{
			{Checks: json.RawMessage(`{"margin_ok":true,"vol_ok":true}`)},
		}, ""},
		{"single margin failure", []store.Evaluation{
			{Checks: json.RawMessage(`{"margin_ok":false,"vol_ok":true}`)},
		}, "margin_collapse"},
		{"count dominates: volume fails twice, margin once", []store.Evaluation{
			{Checks: json.RawMessage(`{"margin_ok":false,"vol_ok":false}`)},
			{Checks: json.RawMessage(`{"margin_ok":true,"vol_ok":false}`)},
		}, "volume_dried"},
		{"tie breaks by priority: stop beats margin", []store.Evaluation{
			{Checks: json.RawMessage(`{"kill_price_ok":false,"margin_ok":false}`)},
		}, "stopped_out"},
		{"kindred checks pool per mode name but count separately", []store.Evaluation{
			{Checks: json.RawMessage(`{"buy_leg_fresh":false,"margin_ok":false}`)},
			{Checks: json.RawMessage(`{"buy_leg_fresh":false,"margin_ok":true}`)},
		}, "leg_stale"},
		{"unmapped check alone stays unlabeled", []store.Evaluation{
			{Checks: json.RawMessage(`{"out_of_window":false}`)},
		}, ""},
		{"unreadable checks skipped, rest still counted", []store.Evaluation{
			{Checks: json.RawMessage(`not-json`)},
			{Checks: json.RawMessage(`{"exit_reachable":false}`)},
		}, "exit_not_printing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dominantFailureMode(tc.evals)
			switch {
			case tc.want == "" && got != nil:
				t.Fatalf("want nil, got %q", *got)
			case tc.want != "" && got == nil:
				t.Fatalf("want %q, got nil", tc.want)
			case tc.want != "" && *got != tc.want:
				t.Fatalf("want %q, got %q", tc.want, *got)
			}
		})
	}
}

func TestEvalWithChecksHelperRoundtrips(t *testing.T) {
	e := evalWithChecks(t, map[string]bool{"margin_ok": false})
	if got := dominantFailureMode([]store.Evaluation{e}); got == nil || *got != "margin_collapse" {
		t.Fatalf("helper roundtrip failed: %v", got)
	}
}
