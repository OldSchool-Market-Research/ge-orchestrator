package api

import (
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

// Item lookup: the dashboard's item browser and per-item pages. Read-only
// over public.items / prices_1m / prices_5m; see store/items.go.

// itemsCache keeps the all-items list for itemsTTL. The query is ~170 ms on
// prod and prices move at most once a minute, so a page of reloads (or a few
// tabs) costs one query instead of one each.
type itemsCache struct {
	mu   sync.Mutex
	at   time.Time
	rows []store.ItemRow
}

const itemsTTL = 30 * time.Second

func (s *Server) listItems(w http.ResponseWriter, r *http.Request) {
	s.items.mu.Lock()
	defer s.items.mu.Unlock()
	if s.items.rows == nil || time.Since(s.items.at) > itemsTTL {
		rows, err := s.Store.Items(r.Context())
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		s.items.rows, s.items.at = rows, time.Now()
	}
	writeJSON(w, 200, s.items.rows)
}

// itemID parses the {id} path value as an int4 item_id; out of range is a
// 404 (no such item), not a driver encode error.
func itemID(w http.ResponseWriter, r *http.Request) (int, bool) {
	n, err := strconv.ParseInt(r.PathValue("id"), 10, 32)
	if errors.Is(err, strconv.ErrRange) {
		writeErr(w, 404, "no item with that id")
		return 0, false
	}
	if err != nil {
		writeErr(w, 400, "id must be an integer item_id")
		return 0, false
	}
	return int(n), true
}

func (s *Server) getItem(w http.ResponseWriter, r *http.Request) {
	id, ok := itemID(w, r)
	if !ok {
		return
	}
	d, err := s.Store.ItemByID(r.Context(), id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if d == nil {
		writeErr(w, 404, "no item with that id")
		return
	}
	writeJSON(w, 200, d)
}

// itemRanges maps ?days to a bucket width that keeps every chart at a few
// hundred points (1d of 5m blocks = 288, 7d hourly = 168, 30d at 4h = 180,
// 90d daily = 90).
var itemRanges = map[int]string{1: "5 minutes", 7: "1 hour", 30: "4 hours", 90: "1 day"}

func (s *Server) itemHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := itemID(w, r)
	if !ok {
		return
	}
	days := 7
	if d := r.URL.Query().Get("days"); d != "" {
		n, err := strconv.Atoi(d)
		if err != nil {
			writeErr(w, 400, "days must be 1, 7, 30 or 90")
			return
		}
		days = n
	}
	bucket, ok := itemRanges[days]
	if !ok {
		writeErr(w, 400, "days must be 1, 7, 30 or 90")
		return
	}
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	points, err := s.Store.ItemHistory(r.Context(), id, since, bucket)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, points)
}
