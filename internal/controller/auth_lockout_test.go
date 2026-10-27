package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"lira/internal/model"
)

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
	c, store := testController(t)
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
	c, store := testController(t)
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

// totpUser creates a user with TOTP enabled and returns its id and secret.
func totpUser(t *testing.T, store *model.Store, name string) (int64, string) {
	t.Helper()
	hash, _ := hashPassword("pw-" + name)
	uid, err := store.CreateUser(name, hash, "admin")
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := generateTOTPSecret()
	_ = store.SetTOTPSecret(uid, secret)
	_ = store.EnableTOTP(uid)
	return uid, secret
}

// runParallel releases n copies of fn at once and waits for them all.
func runParallel(n int, fn func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
}

// A burst of parallel wrong 2FA codes gets exactly maxFailed evaluations; the
// rest are refused by the lockout instead of each being checked.
func TestParallelTwoFactorGuessesHitLockout(t *testing.T) {
	c, store := testController(t)
	uid, _ := totpUser(t, store, "alice")
	sess, _ := store.CreateSession(uid, false, time.Hour)
	runParallel(30, func(int) {
		user, _ := store.GetUserByID(uid)
		r := withUser(postForm("/2fa/verify", url.Values{"code": {"000000x"}}), sess, user)
		c.TwoFAVerify(httptest.NewRecorder(), r)
	})
	if u, _ := store.GetUserByID(uid); u.FailedLogins != maxFailed || !isLocked(u) {
		t.Errorf("after 30 parallel guesses: failed_logins=%d locked=%v want %d and locked",
			u.FailedLogins, isLocked(u), maxFailed)
	}
}

// One TOTP code submitted from two half-logged-in sessions at once passes once.
func TestParallelTOTPReplayPassesOnce(t *testing.T) {
	c, store := testController(t)
	uid, secret := totpUser(t, store, "bob")
	key, _ := b32.DecodeString(secret)
	code := hotp(key, uint64(time.Now().Unix()/30))
	sessions := make([]*model.Session, 4)
	for i := range sessions {
		sessions[i], _ = store.CreateSession(uid, false, time.Hour)
	}
	runParallel(len(sessions), func(i int) {
		user, _ := store.GetUserByID(uid)
		r := withUser(postForm("/2fa/verify", url.Values{"code": {code}}), sessions[i], user)
		c.TwoFAVerify(httptest.NewRecorder(), r)
	})
	passed := 0
	for _, s := range sessions {
		if got, _ := store.GetSession(s.Token); got.MFAPassed {
			passed++
		}
	}
	if passed != 1 {
		t.Errorf("same TOTP code passed 2FA in %d sessions want 1", passed)
	}
}

// One invite link posted several times at once creates one account.
func TestParallelInviteAcceptCreatesOneUser(t *testing.T) {
	c, store := testController(t)
	c.cfg.MaxUsers = 10
	admin, _ := store.CreateUser("admin", "h", "admin")
	_ = store.CreateInvite(hashToken("tok-1"), "x", admin, time.Now().Add(time.Hour))
	runParallel(4, func(i int) {
		r := postForm("/invite/tok-1", url.Values{"username": {fmt.Sprintf("m%d", i)},
			"password": {"longenough"}, "confirm": {"longenough"}})
		r.SetPathValue("token", "tok-1")
		c.InviteAccept(httptest.NewRecorder(), r)
	})
	if n, _ := store.CountUsers(); n != 2 {
		t.Errorf("users after 4 parallel uses of one invite = %d want 2", n)
	}
}

// A locked account answers exactly like an unknown username, so the lockout
// can't confirm which usernames exist.
func TestLockedLoginLooksLikeUnknownUser(t *testing.T) {
	c, store := testController(t)
	hash, _ := hashPassword("pw")
	_, _ = store.CreateUser("carol", hash, "admin")
	login := func(user, pw string) string {
		w := httptest.NewRecorder()
		c.Login(w, postForm("/login", url.Values{"username": {user}, "password": {pw}}))
		return strings.ReplaceAll(w.Body.String(), user, "USER")
	}
	for i := 0; i < maxFailed; i++ {
		login("carol", "wrong")
	}
	locked, unknown := login("carol", "wrong"), login("nobody", "wrong")
	if strings.Contains(locked, "locked") || locked != unknown {
		t.Error("a locked account's login page differs from an unknown username's")
	}
}

// A member posting the token form without from=pocket must not get the
// admin-only Settings page (member list, invites, budget).
func TestTokenCreateNeverShowsSettingsToMembers(t *testing.T) {
	c, store := testController(t)
	uid, _ := store.CreateUser("member", "h", "member")
	r := withUser(postForm("/tokens", url.Values{"name": {"phone"}}),
		&model.Session{MFAPassed: true}, &model.User{ID: uid, Username: "member", Role: "member"})
	w := httptest.NewRecorder()
	c.TokenCreate(w, r)
	if strings.Contains(w.Body.String(), "Invite") || strings.Contains(w.Body.String(), "<h1 class=\"display\">Settings") {
		t.Error("member saw the Settings page after creating a token")
	}
	if !strings.Contains(w.Body.String(), "lira_") {
		t.Error("the new token wasn't shown on the page the member got")
	}

	r = withUser(postForm("/tokens/1/delete", url.Values{}),
		&model.Session{MFAPassed: true}, &model.User{ID: uid, Username: "member", Role: "member"})
	w = httptest.NewRecorder()
	c.TokenRevoke(w, r)
	if loc := w.Header().Get("Location"); loc != "/pocket" {
		t.Errorf("member revoke redirects to %q want /pocket", loc)
	}
}
