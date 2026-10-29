package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

// Processing recipes: the dashboard's Processing page. Read-only views over
// ge-data's process_* schema; see store/process.go.

// processFail maps a store error to a response: 503 while the ge-data
// schema isn't applied (a deploy-order state, not a bug), 500 otherwise.
func processFail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrProcessUnavailable) {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeErr(w, 500, err.Error())
}

// processID parses the {id} path value as an int4 recipe_id. Out of range
// is a 404 (no such recipe), not an encode error from the driver.
func processID(w http.ResponseWriter, r *http.Request) (int, bool) {
	n, err := strconv.ParseInt(r.PathValue("id"), 10, 32)
	if errors.Is(err, strconv.ErrRange) {
		writeErr(w, 404, "no processing recipe with that id")
		return 0, false
	}
	if err != nil {
		writeErr(w, 400, "id must be an integer recipe_id")
		return 0, false
	}
	return int(n), true
}

// processDays parses ?days (default 7, 1..90) into a cutoff.
func processDays(r *http.Request) (time.Time, bool) {
	days := 7
	if d := r.URL.Query().Get("days"); d != "" {
		n, err := strconv.Atoi(d)
		if err != nil || n < 1 || n > 90 {
			return time.Time{}, false
		}
		days = n
	}
	return time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour), true
}

func (s *Server) listProcess(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ProcessQuotes(r.Context())
	if err != nil {
		processFail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) getProcess(w http.ResponseWriter, r *http.Request) {
	id, ok := processID(w, r)
	if !ok {
		return
	}
	d, err := s.Store.ProcessDetailByID(r.Context(), id)
	if err != nil {
		processFail(w, err)
		return
	}
	if d == nil {
		writeErr(w, 404, "no processing recipe with that id")
		return
	}
	writeJSON(w, 200, d)
}

var processBuckets = map[string]struct {
	interval string
	hours    int
}{"1h": {"1 hour", 1}, "6h": {"6 hours", 6}, "1d": {"1 day", 24}}

// maxHistoryBuckets keeps one response to a few hundred points, like ge-mcp.
const maxHistoryBuckets = 400

func (s *Server) processHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := processID(w, r)
	if !ok {
		return
	}
	since, ok := processDays(r)
	if !ok {
		writeErr(w, 400, "days must be 1..90")
		return
	}
	b := r.URL.Query().Get("bucket")
	if b == "" {
		b = "1h"
	}
	bucket, ok := processBuckets[b]
	if !ok {
		writeErr(w, 400, "bucket must be 1h, 6h or 1d")
		return
	}
	if time.Since(since).Hours()/float64(bucket.hours) > maxHistoryBuckets {
		writeErr(w, 400, "days/bucket exceeds 400 points: use bucket=6h beyond 16 days")
		return
	}
	points, err := s.Store.ProcessHistory(r.Context(), id, since, bucket.interval)
	if err != nil {
		processFail(w, err)
		return
	}
	if points == nil {
		writeErr(w, 404, "no processing recipe with that id")
		return
	}
	writeJSON(w, 200, points)
}

func (s *Server) processTop(w http.ResponseWriter, r *http.Request) {
	since, ok := processDays(r)
	if !ok {
		writeErr(w, 400, "days must be 1..90")
		return
	}
	metric := r.URL.Query().Get("metric")
	if metric == "" {
		metric = "gp_hr"
	}
	switch metric {
	case "gp_hr", "gp_hr_insta", "margin_slow":
	default:
		writeErr(w, 400, "metric must be gp_hr, gp_hr_insta or margin_slow")
		return
	}
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 100 {
			writeErr(w, 400, "limit must be 1..100")
			return
		}
		limit = n
	}
	coverage := 50
	if c := r.URL.Query().Get("min_coverage_pct"); c != "" {
		n, err := strconv.Atoi(c)
		if err != nil || n < 0 || n > 100 {
			writeErr(w, 400, "min_coverage_pct must be 0..100")
			return
		}
		coverage = n
	}
	rows, err := s.Store.ProcessTop(r.Context(), metric, since, limit, coverage)
	if err != nil {
		processFail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}
