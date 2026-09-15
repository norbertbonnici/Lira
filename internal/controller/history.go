package controller

import (
	"context"
	"log"
	"net/http"
	"time"

	"lira/internal/model"
	"lira/internal/view"
)

// navSeries converts snapshots (ascending) into plain chart data for the view.
func navSeries(snaps []model.NavSnapshot) view.ChartSeries {
	var s view.ChartSeries
	for _, n := range snaps {
		label := n.TakenAt
		if len(label) >= 10 {
			label = label[:10] // YYYY-MM-DD
		}
		s.Labels = append(s.Labels, label)
		s.Cash = append(s.Cash, n.CashEUR)
		s.Inv = append(s.Inv, n.InvEUR)
		s.Total = append(s.Total, n.NavEUR)
	}
	return s
}

func reversedSnaps(s []model.NavSnapshot) []model.NavSnapshot {
	out := make([]model.NavSnapshot, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}

// NavHistory renders the net-worth history page (chart + table + period change).
func (c *Controller) NavHistory(w http.ResponseWriter, r *http.Request) {
	snaps, _ := c.store.ListNavSnapshots(0)
	cash, inv, total, err := c.store.ComputeNAV()
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"Snapshots":   reversedSnaps(snaps), // table newest-first
		"Chart":       navSeries(snaps),
		"HasHistory":  len(snaps) >= 2,
		"Count":       len(snaps),
		"Cash":        cash,
		"Investments": inv,
		"NAV":         total,
	}
	if len(snaps) >= 1 {
		first, last := snaps[0], snaps[len(snaps)-1]
		change := last.NavEUR - first.NavEUR
		data["First"], data["Last"], data["Change"] = first, last, change
		if first.NavEUR != 0 {
			data["ChangePct"] = change / first.NavEUR * 100
		}
	}
	c.render(w, r, "history.html", data)
}

// SnapshotNow records a manual NAV snapshot, then returns to the history page.
func (c *Controller) SnapshotNow(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if cash, inv, total, err := c.store.ComputeNAV(); err == nil {
		_ = c.store.RecordNavSnapshot(cash, inv, total)
	}
	http.Redirect(w, r, "/history", http.StatusSeeOther)
}

// RefreshNow triggers an on-demand price + FX fetch, then returns to holdings.
func (c *Controller) RefreshNow(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if c.refresh != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := c.refresh(ctx); err != nil {
			log.Printf("manual refresh: %v", err)
		} else {
			_ = c.store.MarkAutoFetched() // count as today's fetch so auto won't repeat
		}
	}
	http.Redirect(w, r, "/holdings", http.StatusSeeOther)
}
