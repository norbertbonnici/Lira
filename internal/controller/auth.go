package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"time"

	"lira/internal/model"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie = "lira_session"
	csrfCookie    = "lira_csrf"
	maxFailed     = 5
	lockoutWindow = 15 * time.Minute
)

// ---------- Passwords ----------

// SHA-256 pre-hash avoids bcrypt's 72-byte truncation, then bcrypt (cost 12).
func hashPassword(pw string) (string, error) {
	sum := sha256.Sum256([]byte(pw))
	b := []byte(base64.StdEncoding.EncodeToString(sum[:]))
	h, err := bcrypt.GenerateFromPassword(b, 12)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

func checkPassword(hash, pw string) bool {
	sum := sha256.Sum256([]byte(pw))
	b := []byte(base64.StdEncoding.EncodeToString(sum[:]))
	return bcrypt.CompareHashAndPassword([]byte(hash), b) == nil
}

func randToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashToken returns a stable hash of an opaque token, suitable for storing in
// place of the plaintext (e.g. invite tokens). The token has high entropy, so a
// plain SHA-256 (no salt) is sufficient and lets us look it up by equality.
func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ---------- Cookies ----------

func (c *Controller) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	ck := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   c.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	}
	if ttl > 0 {
		ck.Expires = time.Now().Add(ttl)
		ck.MaxAge = int(ttl.Seconds())
	}
	http.SetCookie(w, ck)
}

func (c *Controller) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: c.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

// ---------- CSRF (double-submit cookie) ----------

func (c *Controller) ensureCSRF(w http.ResponseWriter, r *http.Request) string {
	if ck, err := r.Cookie(csrfCookie); err == nil && ck.Value != "" {
		return ck.Value
	}
	tok, err := randToken(24)
	if err != nil {
		// crypto/rand failing is catastrophic; fail loudly (caught by the
		// recover middleware as a 500) rather than emit a form with an empty,
		// guaranteed-invalid token.
		panic("controller: generate CSRF token: " + err.Error())
	}
	// Match the session lifetime so the token doesn't expire mid-session and
	// reject an otherwise-valid POST.
	c.setCookie(w, csrfCookie, tok, c.cfg.SessionTTL)
	return tok
}

func (c *Controller) checkCSRF(r *http.Request) bool {
	ck, err := r.Cookie(csrfCookie)
	if err != nil || ck.Value == "" {
		return false
	}
	form := r.FormValue("csrf_token")
	if form == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(ck.Value), []byte(form)) == 1
}

// ---------- Request context ----------

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxSession
)

func currentUser(r *http.Request) *model.User {
	u, _ := r.Context().Value(ctxUser).(*model.User)
	return u
}

func currentSession(r *http.Request) *model.Session {
	s, _ := r.Context().Value(ctxSession).(*model.Session)
	return s
}

func withUser(r *http.Request, sess *model.Session, user *model.User) *http.Request {
	ctx := context.WithValue(r.Context(), ctxUser, user)
	ctx = context.WithValue(ctx, ctxSession, sess)
	return r.WithContext(ctx)
}

// load resolves the session cookie to a (session, user) pair, or (nil, nil).
func (c *Controller) load(r *http.Request) (*model.Session, *model.User) {
	ck, err := r.Cookie(sessionCookie)
	if err != nil || ck.Value == "" {
		return nil, nil
	}
	sess, err := c.store.GetSession(ck.Value)
	if err != nil {
		return nil, nil
	}
	user, err := c.store.GetUserByID(sess.UserID)
	if err != nil {
		return nil, nil
	}
	return sess, user
}

// ---------- Middleware ----------

// Protect requires a fully authenticated session: logged in, 2FA enrolled, and
// 2FA satisfied for this session.
func (c *Controller) Protect(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, user := c.load(r)
		if sess == nil || user == nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if !user.TOTPEnabled {
			http.Redirect(w, r, "/2fa/setup", http.StatusFound)
			return
		}
		if !sess.MFAPassed {
			http.Redirect(w, r, "/2fa/verify", http.StatusFound)
			return
		}
		next(w, withUser(r, sess, user))
	}
}

// SemiAuth requires a session + user but not the 2FA gates (used by the 2FA
// setup/verify pages themselves).
func (c *Controller) SemiAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, user := c.load(r)
		if sess == nil || user == nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, withUser(r, sess, user))
	}
}

// AdminOnly wraps a handler that is already behind Protect, additionally
// requiring the admin role. Non-admins get 403.
func (c *Controller) AdminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u := currentUser(r); u == nil || u.Role != "admin" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}
