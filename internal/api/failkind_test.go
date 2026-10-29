package api

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The tails below are real prod fail_reason values (runs named in each case),
// cut to their last few lines; the head of each tail is the same kind of
// progress noise.
func TestClassifyFail(t *testing.T) {
	const wrap = "ge-agent failed: exit status 1; tail: "
	cFloor := "strategies[0].expected_value.per_cycle_gp: must be >= 200000 for archetype C — below the floor does not ship, dismiss it instead (budget-size the batch first: margin x conversions/4h, not margin x 1)"

	cases := []struct {
		name, reason, kind, detail string
	}{
		{
			name:   "restart (run 8666)",
			reason: "orphaned by orchestrator restart",
			kind:   "restart",
		},
		{
			name: "rejected after earlier single rejections (run 8625)",
			reason: wrap + "ge-agent: 2026/10/28 04:39:00 report rejected: strategies[1].title: required | " +
				"ge-agent: 2026/10/28 04:39:57 turn 80: stop=tool_use in=16490 out=16384 pruned=388620B | " +
				"ge-agent: 2026/10/28 04:39:57 report rejected: strategies[1].title: required | " +
				"ge-agent: 2026/10/28 04:39:57 report rejected 3 times in a row for the same reason: strategies[1].title: required",
			kind:   "rejected",
			detail: "strategies[].title: required",
		},
		{
			name: "rejected, regex-bearing reason (run 8651)",
			reason: wrap + "ge-agent: 2026/10/29 06:50:04 turn 71: stop=tool_use in=16532 out=16384 pruned=174143B | " +
				`ge-agent: 2026/10/29 06:50:04 report rejected: strategies[2].id: must match ^[FBSVCUH]-<item-slug>-<yyyymmdd>$ (e.g. "F-adamantite-bar-20260720") | ` +
				`ge-agent: 2026/10/29 06:50:04 report rejected 3 times in a row for the same reason: strategies[2].id: must match ^[FBSVCUH]-<item-slug>-<yyyymmdd>$ (e.g. "F-adamantite-bar-20260720")`,
			kind:   "rejected",
			detail: `strategies[].id: must match ^[FBSVCUH]-<item-slug>-<yyyymmdd>$ (e.g. "F-adamantite-bar-20260720")`,
		},
		{
			name: "rejected, long reason truncated to 160 runes (run 8528)",
			reason: wrap + "ge-agent: 2026/10/24 05:21:45 turn 49: stop=tool_use in=489 out=14476 pruned=452050B | " +
				"ge-agent: 2026/10/24 05:21:45 report rejected: " + cFloor + " | " +
				"ge-agent: 2026/10/24 05:21:45 report rejected 3 times in a row for the same reason: " + cFloor,
			kind:   "rejected",
			detail: strings.TrimSpace(truncRunes(strings.Replace(cFloor, "[0]", "[]", 1), 160)),
		},
		{
			name: "max_turns, err=false-only tail (run 8650)",
			reason: wrap + "ge-agent: 2026/10/29 05:37:03   tool combo_quote (err=false, 1419 bytes) | " +
				"ge-agent: 2026/10/29 05:37:04 turn 80: stop=tool_use in=1165 out=40 pruned=220974B | " +
				"ge-agent: 2026/10/29 05:37:04   tool combo_quote (err=false, 1280 bytes) | " +
				"ge-agent: 2026/10/29 05:37:04 MAX_TURNS (80, incl. any grace) exhausted without a valid report",
			kind:   "max_turns",
			detail: "MAX_TURNS (80)",
		},
		{
			name: "max_turns after a single earlier rejection (run 8573)",
			reason: wrap + "ge-agent: 2026/10/26 01:21:46 report rejected: strategies[0].size.units_used: must be a positive integer | " +
				"ge-agent: 2026/10/26 01:23:16 turn 84: stop=tool_use in=16452 out=16384 pruned=432539B | " +
				"ge-agent: 2026/10/26 01:23:16 report rejected: strategies[0].title: required | " +
				"ge-agent: 2026/10/26 01:23:16 MAX_TURNS (84, incl. any grace) exhausted without a valid report",
			kind:   "max_turns",
			detail: "MAX_TURNS (84)",
		},
		{
			name: "max_turns whose last turn hit max_tokens (run 8527)",
			reason: wrap + "ge-agent: 2026/10/24 04:45:01   tool liquidity (err=false, 338 bytes) | " +
				"ge-agent: 2026/10/24 04:46:43 turn 80: stop=max_tokens in=1410 out=16384 pruned=317457B | " +
				"ge-agent: 2026/10/24 04:46:43 MAX_TURNS (80, incl. any grace) exhausted without a valid report",
			kind:   "max_turns",
			detail: "MAX_TURNS (80)",
		},
		{
			name: "max_turns with a non-fatal err=true call just before (run 1246)",
			reason: wrap + "ge-agent: 2026/09/13 17:05:33 turn 80: stop=tool_use in=2375 out=117 pruned=271935B | " +
				"ge-agent: 2026/09/13 17:05:33   tool lookup_item (err=true, 68 bytes) | " +
				"ge-agent: 2026/09/13 17:05:33 MAX_TURNS (80, incl. any grace) exhausted without a valid report",
			kind:   "max_turns",
			detail: "MAX_TURNS (80)",
		},
		{
			name: "max_tokens (run 8654)",
			reason: wrap + "ge-agent: 2026/10/29 09:54:09 turn 47: stop=max_tokens in=7931 out=16384 pruned=288724B | " +
				"ge-agent: 2026/10/29 09:56:02 turn 48: stop=max_tokens in=1900 out=16384 pruned=295503B | " +
				"ge-agent: 2026/10/29 09:57:55 turn 49: stop=max_tokens in=46 out=16384 pruned=295503B | " +
				"ge-agent: 2026/10/29 09:57:55 model ended without submit_report after 2 nudges",
			kind: "max_tokens",
		},
		{
			name: "fatal tool call (run 1160)",
			reason: wrap + "ge-agent: 2026/08/25 18:33:03   tool liquidity (err=false, 324 bytes) | " +
				"ge-agent: 2026/08/25 18:33:06 turn 4: stop=tool_use in=1693 out=300 pruned=2663B | " +
				"ge-agent: 2026/08/25 18:33:36 tool combo_screen: call combo_screen: internal error: ERROR: canceling statement due to statement timeout (SQLSTATE 57014)",
			kind:   "tool_error",
			detail: "combo_screen",
		},
		{
			name: "fatal unknown tool (run 847)",
			reason: wrap + "ge-agent: 2026/08/13 17:52:44   tool lookup_item (err=false, 224 bytes) | " +
				"ge-agent: 2026/08/13 17:53:52 turn 20: stop=tool_use in=1799 out=10484 | " +
				"ge-agent: 2026/08/13 17:53:52 tool write_file: call write_file: invalid params: tool 'write_file' not found: tool not found",
			kind:   "tool_error",
			detail: "write_file",
		},
		{
			name: "tail ending on an err=true call",
			reason: wrap + "ge-agent: 2026/10/19 10:26:27   tool item_history (err=false, 3816 bytes) | " +
				"ge-agent: 2026/10/19 10:26:27   tool item_history (err=true, 68 bytes)",
			kind:   "tool_error",
			detail: "item_history",
		},
		{
			name: "tail ending on an err=false call is not a tool error",
			reason: wrap + "ge-agent: 2026/10/19 10:26:27   tool item_history (err=true, 68 bytes) | " +
				"ge-agent: 2026/10/19 10:26:27   tool item_history (err=false, 3937 bytes)",
			kind:   "other",
			detail: "ge-agent failed: exit status 1; tail: ge-agent: 2026/10/19 10:26:27   tool item_history (err=true, 68 bytes) | ge-agent:",
		},
		{
			name: "model gave up after end_turn, not max_tokens (run 1184)",
			reason: wrap + "ge-agent: 2026/09/11 03:54:29 turn 61: stop=end_turn in=181 out=325 pruned=406070B | " +
				"ge-agent: 2026/09/11 03:54:29 model ended without submit_report after 2 nudges",
			kind:   "other",
			detail: "ge-agent failed: exit status 1; tail: ge-agent: 2026/09/11 03:54:29 turn 61: stop=end_turn in=181 out=325 pruned=406070B",
		},
		{
			name: "LLM overloaded (run 8525)",
			reason: wrap + "ge-agent: 2026/10/24 03:03:59 llm: attempt 3/4 failed (llm http 529: overloaded_error: overloaded_error (529)), retrying in 15s | " +
				"ge-agent: 2026/10/24 03:04:15 turn 1: llm http 529: overloaded_error: overloaded_error (529)",
			kind:   "other",
			detail: "ge-agent failed: exit status 1; tail: ge-agent: 2026/10/24 03:03:59 llm: attempt 3/4 failed (llm http 529: overloaded_er",
		},
		{
			name:   "ingest failure (run 534)",
			reason: `ingest: ERROR: invalid byte sequence for encoding "UTF8": 0xe2 0x80 0x0a (SQLSTATE 22021)`,
			kind:   "other",
			detail: `ingest: ERROR: invalid byte sequence for encoding "UTF8": 0xe2 0x80 0x0a (SQLSTATE 22021)`,
		},
		{
			name:   "no report",
			reason: "ge-agent exited without a report",
			kind:   "other",
			detail: "ge-agent exited without a report",
		},
		{
			name: "empty",
			kind: "other",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, detail := classifyFail(c.reason)
			if kind != c.kind || detail != c.detail {
				t.Errorf("classifyFail = (%q, %q), want (%q, %q)", kind, detail, c.kind, c.detail)
			}
			if utf8.RuneCountInString(detail) > 160 {
				t.Errorf("detail is %d runes, max 160", utf8.RuneCountInString(detail))
			}
		})
	}
}
