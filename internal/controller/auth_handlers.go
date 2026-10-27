package controller

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"lira/internal/model"
)

// registrationOpen reports whether the public /register page should work. It is
// open ONLY to bootstrap the very first account (the admin) on a fresh database;
// once any user exists it closes for good and further members join by invite.
func (c *Controller) registrationOpen() bool {
	n, err := c.store.CountUsers()
	if err != nil {
		return false
	}
	return n == 0
}

func isLocked(u *model.User) bool {
	if u.LockedUntil == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, u.LockedUntil)
	if err != nil {
		return false
	}
	return time.Now().Before(t)
}

// ---------- Register ----------

func (c *Controller) Register(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		c.render(w, r, "auth_register.html", map[string]any{"Open": c.registrationOpen()})
		return
	}
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if !c.registrationOpen() {
		c.render(w, r, "auth_register.html", map[string]any{"Open": false})
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	pw := r.FormValue("password")
	confirm := r.FormValue("confirm")

	fail := func(msg string) {
		c.render(w, r, "auth_register.html", map[string]any{"Open": true, "Error": msg, "Username": username})
	}
	if username == "" {
		fail("Username is required.")
		return
	}
	if len(pw) < 8 {
		fail("Password must be at least 8 characters.")
		return
	}
	if pw != confirm {
		fail("Passwords do not match.")
		return
	}
	hash, err := hashPassword(pw)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// The only public registration is the first account, which becomes admin.
	// The cap of 1 is checked in the INSERT itself, so two racing sign-ups on a
	// fresh install can't both become admin.
	uid, err := c.store.CreateUserCapped(username, hash, "admin", 1)
	if errors.Is(err, model.ErrUserLimit) {
		c.render(w, r, "auth_register.html", map[string]any{"Open": false})
		return
	}
	if err != nil {
		fail("That username is taken.")
		return
	}
	// Log them in. 2FA is not yet enrolled, so Protect() pushes them to setup.
	sess, err := c.store.CreateSession(uid, true, c.cfg.SessionTTL)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	c.setCookie(w, sessionCookie, sess.Token, c.cfg.SessionTTL)
	http.Redirect(w, r, "/2fa/setup", http.StatusSeeOther)
}

// ---------- Invite acceptance ----------

// InviteAccept handles the invite link: GET renders the join form, POST creates
// a member account bound to a valid, unused, unexpired invite, then logs in and
// pushes through 2FA setup (same as bootstrap registration).
func (c *Controller) InviteAccept(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	iv, err := c.store.GetValidInvite(hashToken(token))
	if err != nil {
		c.render(w, r, "invite.html", map[string]any{"Invalid": true})
		return
	}

	if r.Method == http.MethodGet {
		c.render(w, r, "invite.html", map[string]any{"Token": token, "Label": iv.Label})
		return
	}
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	// Honour the overall account cap.
	if n, err := c.store.CountUsers(); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	} else if n >= c.cfg.MaxUsers {
		c.render(w, r, "invite.html", map[string]any{"Invalid": true})
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	pw := r.FormValue("password")
	confirm := r.FormValue("confirm")
	fail := func(msg string) {
		c.render(w, r, "invite.html", map[string]any{
			"Token": token, "Label": iv.Label, "Error": msg, "Username": username})
	}
	switch {
	case username == "":
		fail("Username is required.")
		return
	case len(pw) < 8:
		fail("Password must be at least 8 characters.")
		return
	case pw != confirm:
		fail("Passwords do not match.")
		return
	}
	hash, err := hashPassword(pw)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Claim the invite and create the account together (the earlier checks are
	// only for a friendly early answer): concurrent posts of one link create at
	// most one account, and never past MaxUsers.
	uid, err := c.store.AcceptInvite(iv.ID, username, hash, c.cfg.MaxUsers)
	if errors.Is(err, model.ErrInviteUsed) || errors.Is(err, model.ErrUserLimit) {
		c.render(w, r, "invite.html", map[string]any{"Invalid": true})
		return
	}
	if err != nil {
		fail("That username is taken.")
		return
	}
	sess, err := c.store.CreateSession(uid, true, c.cfg.SessionTTL)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	c.setCookie(w, sessionCookie, sess.Token, c.cfg.SessionTTL)
	http.Redirect(w, r, "/2fa/setup", http.StatusSeeOther)
}

// ---------- Login ----------

// dummyHash is compared against when there is no real hash to check (unknown or
// locked user), so those refusals cost the same bcrypt time as a real one.
const dummyHash = "$2a$12$...................................................."

func (c *Controller) Login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		c.render(w, r, "auth_login.html", map[string]any{"CanRegister": c.registrationOpen()})
		return
	}
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	pw := r.FormValue("password")

	// One message for every refusal — unknown user, wrong password, or locked —
	// so the lockout can't be used to confirm that a username exists.
	invalid := func() {
		c.render(w, r, "auth_login.html", map[string]any{
			"Error":    "Invalid username or password. After repeated failures, sign-in pauses for 15 minutes.",
			"Username": username, "CanRegister": c.registrationOpen()})
	}

	user, err := c.store.GetUserByUsername(username)
	if err != nil {
		// Dummy compare to keep timing roughly uniform for unknown users.
		_ = checkPassword(dummyHash, pw)
		invalid()
		return
	}
	// Reserve this guess against the lockout budget before checking it, so
	// parallel requests can't all pass a stale "not locked" read (the claim
	// counts as a failure and sets the lockout when it hits the limit).
	if ok, err := c.store.ClaimLoginAttempt(user.ID, maxFailed, lockoutWindow); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	} else if !ok {
		// Locked: same answer and same bcrypt cost as an unknown username.
		_ = checkPassword(dummyHash, pw)
		invalid()
		return
	}
	if !checkPassword(user.PasswordHash, pw) {
		invalid()
		return
	}
	// The password alone only clears the shared lockout counter when there is no
	// second factor to pass. With TOTP on, it just gives back its own claim and
	// TwoFAVerify resets the counter after a good code; resetting here would let
	// a password holder re-login after every 4 wrong codes and brute-force the
	// TOTP without ever hitting the lockout.
	if user.TOTPEnabled {
		_ = c.store.RefundLoginAttempt(user.ID, maxFailed)
	} else {
		_ = c.store.ResetFailedLogin(user.ID)
	}

	// mfaPassed only when the user has no TOTP yet (they'll be sent to setup);
	// otherwise they must clear the verify step.
	sess, err := c.store.CreateSession(user.ID, !user.TOTPEnabled, c.cfg.SessionTTL)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	c.setCookie(w, sessionCookie, sess.Token, c.cfg.SessionTTL)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---------- Logout ----------

func (c *Controller) Logout(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if ck, err := r.Cookie(sessionCookie); err == nil {
		_ = c.store.DeleteSession(ck.Value)
	}
	c.clearCookie(w, sessionCookie)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---------- 2FA setup ----------

func (c *Controller) TwoFASetup(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	sess := currentSession(r)

	if user.TOTPEnabled {
		if sess.MFAPassed {
			http.Redirect(w, r, "/", http.StatusFound)
		} else {
			http.Redirect(w, r, "/2fa/verify", http.StatusFound)
		}
		return
	}

	// Ensure a pending secret exists and is stable across the setup view.
	secret := user.TOTPSecret
	if secret == "" {
		s, err := generateTOTPSecret()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := c.store.SetTOTPSecret(user.ID, s); err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		secret = s
	}

	if r.Method == http.MethodGet {
		c.render(w, r, "auth_2fa_setup.html", map[string]any{
			"Secret":  groupSecret(secret),
			"OtpURL":  otpauthURL(c.cfg.Issuer, user.Username, secret),
			"Account": user.Username,
		})
		return
	}

	// POST: confirm a code, then enable + issue recovery codes.
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	step, ok := validateTOTP(secret, r.FormValue("code"), time.Now().Unix())
	if !ok {
		c.render(w, r, "auth_2fa_setup.html", map[string]any{
			"Secret":  groupSecret(secret),
			"OtpURL":  otpauthURL(c.cfg.Issuer, user.Username, secret),
			"Account": user.Username,
			"Error":   "That code didn't match. Check your authenticator and try again.",
		})
		return
	}
	if err := c.store.EnableTOTP(user.ID); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	// Burn the enrolment code so it can't be replayed at the first verify.
	_ = c.store.SetTOTPLastStep(user.ID, step)
	codes, err := generateRecoveryCodes(10)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	hashes := make([]string, 0, len(codes))
	for _, code := range codes {
		h, err := hashPassword(code)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		hashes = append(hashes, h)
	}
	if err := c.store.StoreRecoveryCodes(user.ID, hashes); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	_ = c.store.SetSessionMFAPassed(sess.Token)

	c.render(w, r, "auth_2fa_setup.html", map[string]any{
		"Done":          true,
		"RecoveryCodes": codes,
	})
}

// ---------- 2FA verify ----------

func (c *Controller) TwoFAVerify(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	sess := currentSession(r)

	if !user.TOTPEnabled {
		http.Redirect(w, r, "/2fa/setup", http.StatusFound)
		return
	}
	if sess.MFAPassed {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	// Throttle 2FA guessing the same way as login: a holder of the (first-factor)
	// password otherwise gets unlimited tries at the TOTP code and recovery codes.
	if isLocked(user) {
		c.render(w, r, "auth_2fa_verify.html", map[string]any{
			"Error": "Too many attempts. Try again shortly."})
		return
	}
	if r.Method == http.MethodGet {
		c.render(w, r, "auth_2fa_verify.html", nil)
		return
	}
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	// Reserve the guess first (see Login): parallel guesses can't outrun the lockout.
	if ok, err := c.store.ClaimLoginAttempt(user.ID, maxFailed, lockoutWindow); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	} else if !ok {
		c.render(w, r, "auth_2fa_verify.html", map[string]any{
			"Error": "Too many attempts. Try again shortly."})
		return
	}
	code := strings.TrimSpace(r.FormValue("code"))
	step, ok := validateTOTP(user.TOTPSecret, code, time.Now().Unix())
	if ok {
		// Consume the step atomically: a code already used in this (or a later)
		// step is a replay, even when both copies arrive at once.
		ok, _ = c.store.ConsumeTOTPStep(user.ID, step)
	}
	if !ok {
		// Fall back to a single-use recovery code (also consumed atomically).
		rcs, _ := c.store.ListUnusedRecoveryCodes(user.ID)
		for _, rc := range rcs {
			if checkPassword(rc.Hash, code) {
				ok, _ = c.store.UseRecoveryCode(rc.ID)
				break
			}
		}
	}
	if !ok { // the claim already counted this failure (and any lockout)
		c.render(w, r, "auth_2fa_verify.html", map[string]any{"Error": "Invalid code."})
		return
	}
	_ = c.store.ResetFailedLogin(user.ID) // clear the shared lockout counter
	_ = c.store.SetSessionMFAPassed(sess.Token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
