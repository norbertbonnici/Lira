package controller

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"lira/internal/config"
	"lira/internal/model"
	"lira/internal/view"

	_ "modernc.org/sqlite"
)

// newTestController wires a controller to a fresh migrated store and the real
// templates, for handler-level tests.
func newTestController(t *testing.T) (*Controller, *model.Store) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/t.db?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	store := model.New(db)
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return New(store, view.New(), config.Config{MaxUsers: 2, SessionTTL: time.Hour}), store
}

// postForm builds a CSRF-valid form POST.
func postForm(path string, vals url.Values) *http.Request {
	vals.Set("csrf_token", "tok")
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(vals.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: "tok"})
	return r
}

// A correct password must not clear the failures racked up at the 2FA step,
// otherwise re-logging in after every few wrong codes gives unlimited TOTP
// guesses.
func TestPasswordLoginDoesNotResetTwoFactorLockout(t *testing.T) {
	c, store := newTestController(t)
	hash, err := hashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := store.CreateUser("alice", hash, "admin")
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := generateTOTPSecret()
	_ = store.SetTOTPSecret(uid, secret)
	_ = store.EnableTOTP(uid)

	login := func() *model.Session {
		t.Helper()
		w := httptest.NewRecorder()
		c.Login(w, postForm("/login", url.Values{"username": {"alice"}, "password": {"correct horse"}}))
		for _, ck := range w.Result().Cookies() {
			if ck.Name == sessionCookie {
				sess, err := store.GetSession(ck.Value)
				if err != nil {
					t.Fatal(err)
				}
				return sess
			}
		}
		t.Fatalf("login gave no session (status %d)", w.Code)
		return nil
	}
	wrongCode := func(sess *model.Session) {
		t.Helper()
		user, _ := store.GetUserByID(uid)
		r := withUser(postForm("/2fa/verify", url.Values{"code": {"not-a-code"}}), sess, user)
		c.TwoFAVerify(httptest.NewRecorder(), r)
	}

	for round := 0; round < 3; round++ {
		sess := login()
		for i := 0; i < maxFailed-1; i++ {
			wrongCode(sess)
		}
		if u, _ := store.GetUserByID(uid); isLocked(u) {
			return // locked out within the budget of maxFailed wrong codes: good
		}
	}
	u, _ := store.GetUserByID(uid)
	t.Fatalf("not locked after %d wrong 2FA codes across re-logins (failed_logins=%d)",
		3*(maxFailed-1), u.FailedLogins)
}

// Without 2FA the password is the only factor, so a correct one still clears
// earlier failures.
func TestPasswordLoginResetsCounterWithoutTwoFactor(t *testing.T) {
	c, store := newTestController(t)
	hash, _ := hashPassword("pw")
	uid, _ := store.CreateUser("bob", hash, "admin")
	for i := 0; i < maxFailed-1; i++ {
		c.Login(httptest.NewRecorder(), postForm("/login", url.Values{"username": {"bob"}, "password": {"wrong"}}))
	}
	c.Login(httptest.NewRecorder(), postForm("/login", url.Values{"username": {"bob"}, "password": {"pw"}}))
	if u, _ := store.GetUserByID(uid); u.FailedLogins != 0 {
		t.Errorf("failed_logins = %d want 0 after a good password (no 2FA)", u.FailedLogins)
	}
}
