package controller

import (
	"database/sql"
	"testing"

	"lira/internal/config"
	"lira/internal/model"

	_ "modernc.org/sqlite"
)

// Registration is open only to bootstrap the first (admin) account, then closes.
func TestRegistrationClosesAfterFirstUser(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+dir+"/t.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	store := model.New(db)
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	c := &Controller{store: store, cfg: config.Config{MaxUsers: 2}}
	if !c.registrationOpen() {
		t.Fatal("registration should be open on an empty database")
	}
	if _, err := store.CreateUser("admin", "h", "admin"); err != nil {
		t.Fatal(err)
	}
	if c.registrationOpen() {
		t.Fatal("registration should close once a user exists")
	}
}

// hashToken is deterministic (lets invite lookup by hash) and non-identity.
func TestHashTokenStable(t *testing.T) {
	a, b := hashToken("abc"), hashToken("abc")
	if a != b {
		t.Errorf("hashToken not deterministic: %q vs %q", a, b)
	}
	if a == "abc" || hashToken("abc") == hashToken("abd") {
		t.Error("hashToken should hash, and differ per input")
	}
}
