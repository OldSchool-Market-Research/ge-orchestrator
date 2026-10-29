package api

import (
	"fmt"
	"regexp"
	"strings"
)

// Failure kinds for GET /api/runs. A failed run's fail_reason is either a
// short orchestrator-side reason ("orphaned by orchestrator restart",
// "ingest: ...") or the agent's stderr tail wrapped by the runner:
//
//	ge-agent failed: exit status 1; tail: ge-agent: <ts> <line> | ge-agent: <ts> <line> | ...
//
// The tail is mostly progress noise ("turn N: stop=...", "tool X (err=false,
// N bytes)"); what ended the run is one of ge-agent's terminal errors
// (internal/loop), checked in this precedence:
//
//	restart     orphaned by orchestrator restart
//	rejected    "report rejected N times in a row for the same reason: <why>"
//	            (an earlier single "report rejected: ..." line is NOT the end
//	            of the run; only the N-in-a-row fail-fast line is)
//	max_turns   "MAX_TURNS (N, incl. any grace) exhausted without a valid report"
//	max_tokens  "model ended without submit_report" after max_tokens turns
//	tool_error  the final line is a tool failure: the agent's fatal
//	            "tool <name>: <error>" or a "tool <name> (err=true, ...)" line
//	            ("(err=false" lines are ordinary successful calls)
//	other       anything else
const (
	failRestart   = "restart"
	failRejected  = "rejected"
	failMaxTurns  = "max_turns"
	failMaxTokens = "max_tokens"
	failToolError = "tool_error"
	failOther     = "other"
)

var (
	reRejectedRun = regexp.MustCompile(`report rejected \d+ times in a row for the same reason: `)
	reMaxTurns    = regexp.MustCompile(`MAX_TURNS \((\d+)[^)]*\) exhausted`)
	reStop        = regexp.MustCompile(`stop=(\w+)`)
	reArrayIndex  = regexp.MustCompile(`\[\d+\]`)
	// One tail line's prefix: the runner's wrapper (first line only) and the
	// agent's log.Printf timestamp.
	reLinePrefix = regexp.MustCompile(`^(?:.*?; tail: )?ge-agent: \d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\s+`)
	reToolError  = regexp.MustCompile(`^tool (\S+?)(?: \(err=true\b|: )`)
	reTurnPrefix = regexp.MustCompile(`^turn \d+: `)
)

const tailSep = " | "

// classifyFail maps a failed run's fail_reason to a stable kind plus a short
// detail for the dashboard. Pure; see the precedence table above.
func classifyFail(reason string) (kind, detail string) {
	reason = strings.TrimSpace(reason)

	if strings.Contains(reason, "orphaned by orchestrator restart") {
		return failRestart, ""
	}

	if locs := reRejectedRun.FindAllStringIndex(reason, -1); len(locs) > 0 {
		why := reason[locs[len(locs)-1][1]:]
		if i := strings.Index(why, tailSep+"ge-agent: "); i >= 0 {
			why = why[:i] // defensive: the fail-fast line is normally the last one
		}
		why = reArrayIndex.ReplaceAllString(why, "[]")
		return failRejected, truncRunes(strings.TrimSpace(why), 160)
	}

	if m := reMaxTurns.FindAllStringSubmatch(reason, -1); len(m) > 0 {
		return failMaxTurns, fmt.Sprintf("MAX_TURNS (%s)", m[len(m)-1][1])
	}

	if strings.Contains(reason, "model ended without submit_report") {
		if stops := reStop.FindAllStringSubmatch(reason, -1); len(stops) > 0 && stops[len(stops)-1][1] == "max_tokens" {
			return failMaxTokens, ""
		}
	}

	last := reason
	if i := strings.LastIndex(reason, tailSep); i >= 0 {
		last = reason[i+len(tailSep):]
	}
	last = reLinePrefix.ReplaceAllString(strings.TrimSpace(last), "")
	if m := reToolError.FindStringSubmatch(last); m != nil {
		return failToolError, m[1]
	}

	// An agent tail's first 120 runes are the runner's wrapper and the
	// oldest, timestamped tail line; the last line is what ended the run
	// ("turn 1: llm http 429: ...", "context deadline exceeded"), and without
	// its timestamp and turn number equal causes group together.
	if strings.Contains(reason, "ge-agent: ") {
		return failOther, truncRunes(strings.TrimSpace(reTurnPrefix.ReplaceAllString(last, "")), 120)
	}
	return failOther, strings.TrimSpace(truncRunes(reason, 120))
}

// truncRunes cuts s to at most n runes (never mid-rune).
func truncRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}
