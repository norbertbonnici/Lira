package controller

import "net/http"

// API tokens are managed in the (admin) Settings page, but the endpoints stay
// per-user (Protect, not AdminOnly) and scoped by currentUser, so the token list
// and revoke only ever touch the acting user's own tokens. Only the SHA-256 hash
// is stored; the plaintext is shown exactly once, right after creation.

func (c *Controller) TokenCreate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u := currentUser(r)
	raw, err := randToken(24)
	if err != nil {
		http.Error(w, "could not generate token", http.StatusInternalServerError)
		return
	}
	plain := "lira_" + raw
	if err := c.store.CreateAPIToken(u.ID, r.FormValue("name"), hashToken(plain)); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	// Re-render the originating page with the plaintext shown once (it can't be
	// recovered). Pocket is the per-user home members reach; Settings is admin.
	if r.FormValue("from") == "pocket" {
		c.renderPocket(w, r, map[string]any{"NewToken": plain})
		return
	}
	c.renderSettings(w, r, map[string]any{"NewToken": plain})
}

func (c *Controller) TokenRevoke(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u := currentUser(r)
	_ = c.store.DeleteAPIToken(u.ID, pathID(r))
	dest := "/settings"
	if r.FormValue("from") == "pocket" {
		dest = "/pocket"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}
