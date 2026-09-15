package controller

import (
	"net/http"
	"strings"
	"time"

	"lira/internal/model"
)

// inviteTTL is how long a freshly minted invite link stays valid.
const inviteTTL = 7 * 24 * time.Hour

// baseURL returns the public origin used to build absolute invite links. It
// prefers the configured LIRA_BASE_URL and otherwise derives it from the
// request (honouring a reverse proxy's X-Forwarded-Proto).
func (c *Controller) baseURL(r *http.Request) string {
	if c.cfg.BaseURL != "" {
		return c.cfg.BaseURL
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// renderSettings draws the settings page with the current household, members,
// and pending invites, merging in any one-off extras (e.g. a fresh invite link).
func (c *Controller) renderSettings(w http.ResponseWriter, r *http.Request, extra map[string]any) {
	hh, _ := c.store.GetHousehold()
	members, _ := c.store.ListUsers()
	invites, _ := c.store.ListPendingInvites()
	_, _, _, available := c.budgetSummary()
	var tokens []model.APIToken
	if u := currentUser(r); u != nil {
		tokens, _ = c.store.ListAPITokens(u.ID)
	}
	data := map[string]any{
		"Household": hh,
		"Members":   members,
		"Invites":   invites,
		"Available": available, // budget hint for the savings field
		"Tokens":    tokens,    // API access tokens (current user's own)
	}
	for k, v := range extra {
		data[k] = v
	}
	c.render(w, r, "settings.html", data)
}

// Settings renders the admin settings page.
func (c *Controller) Settings(w http.ResponseWriter, r *http.Request) {
	c.renderSettings(w, r, nil)
}

// HouseholdRename updates the household's display name.
func (c *Controller) HouseholdRename(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if name := strings.TrimSpace(r.FormValue("name")); name != "" {
		_ = c.store.RenameHousehold(name)
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// SavingsUpdate stores the planned monthly saving used by the NAV forecast.
func (c *Controller) SavingsUpdate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	v, ok := formMoney(r.FormValue("monthly_savings"))
	if !ok { // unparseable input: keep the existing value rather than zeroing it
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if v < 0 {
		v = 0
	}
	_ = c.store.SetMonthlySavings(v)
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// InviteCreate mints a one-time invite link. The plaintext token is shown once
// in the response (only its hash is stored), so this does NOT redirect.
func (c *Controller) InviteCreate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if n, err := c.store.CountUsers(); err == nil && n >= c.cfg.MaxUsers {
		c.renderSettings(w, r, map[string]any{
			"Error": "Your household is already full — raise LIRA_MAX_USERS to invite more."})
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	token, err := randToken(24)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	admin := currentUser(r)
	if err := c.store.CreateInvite(hashToken(token), label, admin.ID, time.Now().Add(inviteTTL)); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	c.renderSettings(w, r, map[string]any{
		"InviteLink": c.baseURL(r) + "/invite/" + token,
		"Flash":      "Invite link created — copy it now, it won't be shown again.",
	})
}

// InviteRevoke deletes a pending invite so its link stops working.
func (c *Controller) InviteRevoke(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	_ = c.store.DeleteInvite(pathID(r))
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}
