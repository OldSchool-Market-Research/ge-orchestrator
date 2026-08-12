// Package notify pings the operator over ntfy when a freshly shipped
// strategy clears their standing "worth my time" bar: projected gp/hr above
// a floor, at an attention cadence that fits their day. The bar exists so
// the operator can ignore the dashboard until their phone tells them a
// strategy is worth acting on — a ping that fires on noise trains them to
// mute it, so the gate errs quiet.
package notify

import (
	"context"
	"encoding/json"
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
// when the vetter computed one (else the agent's claim), weighted by the
// archetype's calibration factor ONLY when that factor is measured
// (either component came from a real sample). A below-sample archetype
// carries the conservative default 0.125 — gating on that turns the
// operator's 250k bar into an effective 2M raw bar and the topic goes
// silent forever, which trains the operator to stop trusting the silence.
// Below sample, the harness projection (already haircut for slippage and
// participation) is the honest gate, and the ping body says so.
// Kinds without an attention contract (V/U) never ping — the bar is about
// fitting trades into the operator's day, and those kinds carry no cadence
// to judge. F, B and C all carry attention_spec (agent >= 0.7.0).
func (n *Notifier) Clears(st store.SidecarStrategy, cal *store.CalibrationRow) (per1h int64, calibrated, ok bool) {
	per1h = st.ExpectedValue.Per1hGp
	if st.HarnessProjectedPer1h != nil {
		per1h = *st.HarnessProjectedPer1h
	}
	measured := cal != nil && (cal.PSurvive != nil || cal.PaceRatio != nil)
	if measured && cal.Factor > 0 {
		per1h = int64(cal.Factor * float64(per1h))
		calibrated = true
	}
	if st.AttentionSpec == nil || st.AttentionSpec.ChecksPerHour > n.Cfg.MaxChecksPerHour {
		return per1h, calibrated, false
	}
	return per1h, calibrated, per1h >= n.Cfg.MinPer1hGp
}

// StrategyShipped pings for one accepted strategy if it clears the bar.
// Failures are logged, never returned — a ping must not fail an ingest.
func (n *Notifier) StrategyShipped(ctx context.Context, st store.SidecarStrategy, cal *store.CalibrationRow) {
	per1h, calibrated, ok := n.Clears(st, cal)
	if !ok || n.Cfg.URL == "" {
		return
	}
	title, body := format(st, per1h, calibrated)
	n.post(ctx, st.ID, title, body, "moneybag")
	log.Printf("notify: pinged for %s (%s gp/hr projected)", st.ID, gp(per1h))
}

// StrategyConfirmed pings when a strategy closes CONFIRMED — the record's
// rarest and most actionable event: an edge that survived its whole eval
// window at a realized pace the confirm bar accepted. Gate: the realized
// pace clears the operator bar, and the attention contract (when present)
// fits their day. Confirms are rare by construction; no rate limiting.
func (n *Notifier) StrategyConfirmed(ctx context.Context, st store.Strategy, realizedPer1h int64) {
	if n.Cfg.URL == "" || realizedPer1h < n.Cfg.MinPer1hGp {
		return
	}
	if st.AttentionSpec != nil && st.AttentionSpec.ChecksPerHour > n.Cfg.MaxChecksPerHour {
		return
	}
	name := firstItemName(st.Items)
	title := fmt.Sprintf("GE CONFIRMED: %s - %s gp/hr realized", name, gp(realizedPer1h))
	lines := []string{
		fmt.Sprintf("%s: %s", st.Sid, st.Title),
		fmt.Sprintf("buy %d -> sell %d", st.EntryPrice, st.ExitPrice),
		fmt.Sprintf("survived its full window; realized %s gp/hr (haircut)", gp(realizedPer1h)),
	}
	if st.AttentionSpec != nil {
		lines = append(lines, fmt.Sprintf("attention: %.1f checks/hr, safe unattended %.0fh",
			st.AttentionSpec.ChecksPerHour, st.AttentionSpec.MaxUnattendedHours))
	}
	n.post(ctx, st.Sid, title, strings.Join(lines, "\n"), "white_check_mark")
	log.Printf("notify: confirm ping for %s (%s gp/hr realized)", st.Sid, gp(realizedPer1h))
}

// post fires one ntfy request. Failures are logged, never returned.
func (n *Notifier) post(ctx context.Context, sid, title, body, tags string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.Cfg.URL, strings.NewReader(body))
	if err != nil {
		log.Printf("notify: %s: %v", sid, err)
		return
	}
	req.Header.Set("Title", title)
	req.Header.Set("Tags", tags)
	resp, err := n.Client.Do(req)
	if err != nil {
		log.Printf("notify: %s: %v", sid, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("notify: %s: ntfy returned %s", sid, resp.Status)
	}
}

// firstItemName digs the display name out of the stored items JSON (the
// eval package has its own copy against its own needs).
func firstItemName(raw json.RawMessage) string {
	var items []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 || items[0].Name == "" {
		return "unknown item"
	}
	return items[0].Name
}

// format renders the ping. The title must survive an HTTP header (ASCII);
// the body is UTF-8 and carries what the operator needs to decide from
// their phone: the offers, the capital, and the attention contract.
func format(st store.SidecarStrategy, per1h int64, calibrated bool) (title, body string) {
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
	if calibrated {
		lines = append(lines, fmt.Sprintf("claim %s gp/hr, calibrated %s gp/hr", gp(st.ExpectedValue.Per1hGp), gp(per1h)))
	} else {
		lines = append(lines, fmt.Sprintf("claim %s gp/hr, harness projection %s gp/hr (uncalibrated: below sample)", gp(st.ExpectedValue.Per1hGp), gp(per1h)))
	}
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
