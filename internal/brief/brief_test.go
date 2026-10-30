package brief

import (
	"strings"
	"testing"
	"time"

	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

func fp(v float64) *float64 { return &v }

func TestWriteCalibration(t *testing.T) {
	var b strings.Builder
	writeCalibration(&b, []store.CalibrationRow{
		{Archetype: "F", Factor: 0.21, PSurvive: fp(0.42), NSurvived: 14, NClosed: 33, PaceRatio: nil, NPace: 4},
		{Archetype: "C", Factor: 0.125, PSurvive: nil, NClosed: 2, PaceRatio: nil},
	}, []store.FailureModeCount{
		{Archetype: "F", Mode: "margin_collapse", N: 19},
		{Archetype: "F", Mode: "leg_stale", N: 7},
		{Archetype: "C", Mode: "margin_collapse", N: 1},
	})
	out := b.String()
	for _, want := range []string{
		"so pre-filter F with it",
		"never dismiss a conversion on its factor alone",
		"factor 0.21 = p_survive 0.42 (14 of 33 closed alive) x pace default (below sample gate, n=4)",
		// 400,000 / 0.21 ≈ 1,904,761 raw gp/cycle
		"~1,904,761 gp/cycle",
		"- C: factor 0.12 = p_survive default (below sample gate, n=2)",
		"F closes by failure mode, last 14d: margin_collapse 19, leg_stale 7.",
		"C closes by failure mode, last 14d: margin_collapse 1.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("calibration section missing %q in:\n%s", want, out)
		}
	}
	// C's calibrated floor is not enforced: no implied-raw-claim sentence on its lines.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "- C") && strings.Contains(line, "floor implies") {
			t.Errorf("C line must not carry a floor translation: %q", line)
		}
	}
	// C leads F, whatever order the rows arrive in.
	if strings.Index(out, "- C: factor") > strings.Index(out, "- F: factor") {
		t.Errorf("C must render before F:\n%s", out)
	}
}

func TestCalOrderPutsKindsAfterTheirLane(t *testing.T) {
	got := calOrder([]store.CalibrationRow{
		{Archetype: "F"}, {Archetype: "C:set"}, {Archetype: "C"}, {Archetype: "C:decant"},
	})
	var keys []string
	for _, r := range got {
		keys = append(keys, r.Archetype)
	}
	if s := strings.Join(keys, " "); s != "C C:decant C:set F" {
		t.Fatalf("order %q", s)
	}
}

func TestTrackAdviceReadsSurvival(t *testing.T) {
	dead := &store.CalibrationRow{PSurvive: fp(0), NSurvived: 0, NClosed: 81, SurviveHours: 6}
	if got := trackAdvice(81, 1.15, dead); !strings.Contains(got, "0 of 81") || strings.Contains(got, "keep mining") {
		t.Errorf("a dead lane must not read as holding up: %q", got)
	}
	live := &store.CalibrationRow{PSurvive: fp(0.77), NSurvived: 134, NClosed: 174, SurviveHours: 6}
	if got := trackAdvice(174, 0.9, live); !strings.Contains(got, "keep mining") {
		t.Errorf("a surviving lane above the bar: %q", got)
	}
	if got := trackAdvice(174, 0.4, live); !strings.Contains(got, "have not held") {
		t.Errorf("a surviving lane under the bar: %q", got)
	}
	if got := trackAdvice(3, 1.15, dead); !strings.Contains(got, "insufficient") {
		t.Errorf("a thin sample: %q", got)
	}
	if got := trackAdvice(20, 0.9, nil); !strings.Contains(got, "keep mining") {
		t.Errorf("no calibration row: %q", got)
	}
}

func TestWriteGraveyard(t *testing.T) {
	var b strings.Builder
	writeGraveyard(&b, nil)
	if b.Len() != 0 {
		t.Errorf("empty graveyard must render nothing, got %q", b.String())
	}
	writeGraveyard(&b, []store.GraveyardRow{
		{ItemID: 561, ItemName: "Nature rune", Archetype: "F", Kills: 5, EstRealizedGp: -1_204_332,
			LastKilledAt: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)},
	})
	out := b.String()
	for _, want := range []string{
		"### Graveyard",
		"- Nature rune [F] (item_id 561): 5 kills, -1,204,332 gp realized, last killed 08-05",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("graveyard section missing %q in:\n%s", want, out)
		}
	}
}

func TestDismissalLine(t *testing.T) {
	at := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	reason := "margin was a spike — persistence 0.08"
	if got := dismissalLine(&at, &reason); got != "  - previously dismissed 08-05: margin was a spike — persistence 0.08\n" {
		t.Errorf("dismissalLine = %q", got)
	}
	empty := "   "
	if got := dismissalLine(&at, &empty); got != "" {
		t.Errorf("blank reason must render nothing, got %q", got)
	}
	if got := dismissalLine(nil, &reason); got != "" {
		t.Errorf("nil time must render nothing, got %q", got)
	}
	// Long reasons truncate on rune boundaries — multibyte dashes at the cut
	// point must not produce invalid UTF-8 (the run-534 ingest failure mode).
	long := strings.Repeat("é", 300)
	got := dismissalLine(&at, &long)
	if !strings.HasSuffix(strings.TrimSuffix(got, "\n"), "…") {
		t.Errorf("long reason must end with ellipsis: %q", got)
	}
	if !strings.Contains(got, strings.Repeat("é", 220)) || strings.Contains(got, strings.Repeat("é", 221)) {
		t.Errorf("truncation must keep exactly 220 runes: %d bytes", len(got))
	}
}

func TestGroupNegative(t *testing.T) {
	cases := map[int64]string{
		-120433:    "-120,433", // regression: the sign must not count as a digit
		-1_204_332: "-1,204,332",
		-5:         "-5",
		157_464_000: "157,464,000",
	}
	for n, want := range cases {
		if got := group(n); got != want {
			t.Errorf("group(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestHasPrecomputedScans(t *testing.T) {
	both := "### Latest market sweep\n- " + sweepTitleVolumeHow + " (as of 10-30 12:00):\n  - {}\n- " + sweepTitleSeasonal + " (as of 10-30 12:00):\n  - {}\n"
	if !HasPrecomputedScans(both) {
		t.Error("both lenses rendered")
	}
	if HasPrecomputedScans("### Latest market sweep\n- " + sweepTitleSeasonal + ":\n") {
		t.Error("a missing lens means the agent keeps its tool")
	}
	if HasPrecomputedScans("") {
		t.Error("no sweep at all")
	}
	// ge-agent's refusal quotes these words; moving them breaks its pointer.
	for _, quoted := range []string{"volume anomalies vs the same hour of the week", "hour-of-week amplitude"} {
		if !strings.Contains(both, quoted) {
			t.Errorf("title lost %q", quoted)
		}
	}
}

func TestSweepHeadingMarksStaleScans(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	fresh := "- " + sweepTitleSeasonal + " (as of 10-30 11:00):\n"
	if got := sweepHeading(sweepTitleSeasonal, now.Add(-time.Hour), now); got != fresh {
		t.Errorf("fresh heading %q", got)
	}
	stale := sweepHeading(sweepTitleVolumeHow, now.Add(-3*time.Hour), now)
	if !strings.Contains(stale, "STALE") || !strings.Contains(stale, sweepTitleVolumeHow) {
		t.Errorf("stale heading %q", stale)
	}
	// A stale scan can't stand in for the tool: the proxy must not refuse it.
	if HasPrecomputedScans(fresh + stale) {
		t.Error("a stale lens must not count as precomputed")
	}
	if !HasPrecomputedScans(fresh + sweepHeading(sweepTitleVolumeHow, now.Add(-time.Hour), now)) {
		t.Error("both fresh")
	}
}
