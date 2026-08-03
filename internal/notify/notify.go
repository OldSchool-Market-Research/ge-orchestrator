// Package notify pings the operator over ntfy when a freshly shipped
// strategy clears their standing "worth my time" bar: projected gp/hr above
// a floor, at an attention cadence that fits their day. The bar exists so
// the operator can ignore the dashboard until their phone tells them a
// strategy is worth acting on — a ping that fires on noise trains them to
// mute it, so the gate errs quiet.
package notify

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/osrs-ge/ge-orchestrator/internal/store"
)

// Config comes from the GE_ORCH_NTFY_URL / GE_ORCH_NOTIFY_* env vars.
type Config struct {
	URL              string  // ntfy topic URL (e.g. https://ntfy.jade.rip/osrs); empty disables
	MinPer1hGp       int64   // projected gp/hr floor for a ping
	MaxChecksPerHour float64 // max GE visits/hr the operator will give a strategy
}

type Notifier struct {
	Cfg    Config
	Client *http.Client
}

func New(cfg Config) *Notifier {
	return &Notifier{Cfg: cfg, Client: &http.Client{Timeout: 10 * time.Second}}
}

// Clears applies the operator bar to one accepted strategy and returns the
// gp/hr figure the decision used: the harness's own ship-time projection
// when the vetter computed one, else the agent's claim. Kinds without an
// attention contract (V/C/U) never ping — the bar is about fitting flips
// into the operator's day, and those kinds carry no cadence to judge.
func (n *Notifier) Clears(st store.SidecarStrategy) (int64, bool) {
	per1h := st.ExpectedValue.Per1hGp
	if st.HarnessProjectedPer1h != nil {
		per1h = *st.HarnessProjectedPer1h
	}
	if st.AttentionSpec == nil || st.AttentionSpec.ChecksPerHour > n.Cfg.MaxChecksPerHour {
		return per1h, false
	}
	return per1h, per1h >= n.Cfg.MinPer1hGp
}

// StrategyShipped pings for one accepted strategy if it clears the bar.
// Failures are logged, never returned — a ping must not fail an ingest.
func (n *Notifier) StrategyShipped(ctx context.Context, st store.SidecarStrategy) {
	per1h, ok := n.Clears(st)
	if !ok || n.Cfg.URL == "" {
		return
	}
	title, body := format(st, per1h)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.Cfg.URL, strings.NewReader(body))
	if err != nil {
		log.Printf("notify: %s: %v", st.ID, err)
		return
	}
	req.Header.Set("Title", title)
	req.Header.Set("Tags", "moneybag")
	resp, err := n.Client.Do(req)
	if err != nil {
		log.Printf("notify: %s: %v", st.ID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("notify: %s: ntfy returned %s", st.ID, resp.Status)
		return
	}
	log.Printf("notify: pinged for %s (%s gp/hr projected)", st.ID, gp(per1h))
}

// format renders the ping. The title must survive an HTTP header (ASCII);
// the body is UTF-8 and carries what the operator needs to decide from
// their phone: the offers, the capital, and the attention contract.
func format(st store.SidecarStrategy, per1h int64) (title, body string) {
	name := st.ID
	if len(st.Items) > 0 {
		name = st.Items[0].Name
	}
	title = fmt.Sprintf("GE: %s - %s gp/hr", name, gp(per1h))
	lines := []string{
		fmt.Sprintf("%s: %s", st.ID, st.Title),
		fmt.Sprintf("buy %d -> sell %d, capital %s gp", st.EntryPrice, st.ExitPrice, gp(st.CapitalRequired)),
	}
	if st.AttentionSpec != nil {
		lines = append(lines, fmt.Sprintf("attention: %.1f checks/hr, safe unattended %.0fh",
			st.AttentionSpec.ChecksPerHour, st.AttentionSpec.MaxUnattendedHours))
	}
	lines = append(lines, fmt.Sprintf("claim %s gp/hr, projected %s gp/hr", gp(st.ExpectedValue.Per1hGp), gp(per1h)))
	return title, strings.Join(lines, "\n")
}

// gp renders an amount the way a player reads one: 1.2M, 470k, 987.
func gp(v int64) string {
	f := float64(v)
	switch {
	case v >= 10_000_000:
		return fmt.Sprintf("%.0fM", f/1e6)
	case v >= 1_000_000:
		return fmt.Sprintf("%.1fM", f/1e6)
	case v >= 10_000:
		return fmt.Sprintf("%.0fk", f/1e3)
	default:
		return fmt.Sprintf("%d", v)
	}
}
