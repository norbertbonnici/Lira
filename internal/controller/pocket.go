package controller

import (
	"net/http"
	"strings"
	"time"
)

// Pocket money is PRIVATE per user: every handler scopes its store calls to
// currentUser(r).ID, so one member can never see or touch another's fund (nor can
// an admin — there is no override path). It is a standalone side-pot and does not
// feed NAV, accounts, or any shared/household view.

// Pocket renders the logged-in user's own pocket-money fund.
func (c *Controller) Pocket(w http.ResponseWriter, r *http.Request) {
	c.renderPocket(w, r, nil)
}

// renderPocket draws the pocket page (also a per-user home for API tokens, so
// non-admin members can mint their own). extra merges in one-offs (e.g. a freshly
// created token to show once).
func (c *Controller) renderPocket(w http.ResponseWriter, r *http.Request, extra map[string]any) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	entries, _ := c.store.ListPocketEntries(u.ID)
	balance, _ := c.store.PocketBalance(u.ID)
	cats, _ := c.store.PocketByCategory(u.ID)
	tokens, _ := c.store.ListAPITokens(u.ID)

	var toppedUp, spent, topCat float64
	for _, e := range entries {
		if e.Amount >= 0 {
			toppedUp += e.Amount
		} else {
			spent += -e.Amount
		}
	}
	for _, ct := range cats {
		if ct.Amount > topCat {
			topCat = ct.Amount
		}
	}

	data := map[string]any{
		"Entries":    entries,
		"Balance":    balance,
		"ToppedUp":   toppedUp,
		"Spent":      spent,
		"Categories": cats,
		"TopCat":     topCat,
		"DateVal":    time.Now().Format("2006-01-02"),
		"Tokens":     tokens,
	}
	for k, v := range extra {
		data[k] = v
	}
	c.render(w, r, "pocket.html", data)
}

// PocketAdd records a top-up or spend on the current user's own fund.
func (c *Controller) PocketAdd(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	amount := parseFloat(r.FormValue("amount"))
	if amount < 0 {
		amount = -amount // work with the magnitude; sign comes from the kind
	}
	if r.FormValue("kind") == "spend" {
		amount = -amount
	}
	if amount != 0 {
		date := strings.TrimSpace(r.FormValue("date"))
		if date == "" {
			date = time.Now().Format("2006-01-02")
		}
		_ = c.store.AddPocketEntry(u.ID, date, amount,
			strings.TrimSpace(r.FormValue("category")), strings.TrimSpace(r.FormValue("note")))
	}
	http.Redirect(w, r, "/pocket", http.StatusSeeOther)
}

// PocketDelete removes one of the current user's own entries (scoped by user_id
// in the store, so a guessed id can't touch another user's row).
func (c *Controller) PocketDelete(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	_ = c.store.DeletePocketEntry(u.ID, pathID(r))
	http.Redirect(w, r, "/pocket", http.StatusSeeOther)
}
