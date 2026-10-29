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
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, "id must be an integer recipe_id")
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

var processBuckets = map[string]string{"1h": "1 hour", "6h": "6 hours", "1d": "1 day"}

func (s *Server) processHistory(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, "id must be an integer recipe_id")
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
	points, err := s.Store.ProcessHistory(r.Context(), id, since, bucket)
	if err != nil {
		processFail(w, err)
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
	rows, err := s.Store.ProcessTop(r.Context(), metric, since, limit)
	if err != nil {
		processFail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}
