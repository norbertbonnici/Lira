package controller

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"lira/internal/model"
)

// A transfer between currencies needs the amount received; each leg then holds
// its own currency's figure. Same-currency transfers are unchanged.
func TestTransferBetweenCurrencies(t *testing.T) {
	c, store := testController(t)
	_ = store.CreateAccount("Euro", "checking", "EUR", 1000, false)
	_ = store.CreateAccount("Dollar", "checking", "USD", 0, false)
	_ = store.CreateAccount("Euro2", "savings", "EUR", 0, false)
	ids := map[string]string{}
	accts, _ := store.ListAccounts()
	for _, a := range accts {
		ids[a.Name] = strconv.FormatInt(a.ID, 10)
	}
	transfer := func(from, to, amount, received string) *httptest.ResponseRecorder {
		r := withUser(postForm("/transfer", url.Values{"from_id": {ids[from]}, "to_id": {ids[to]},
			"amount": {amount}, "received": {received}, "date": {"2026-10-01"}}),
			&model.Session{MFAPassed: true}, &model.User{ID: 1, Username: "a", Role: "admin"})
		w := httptest.NewRecorder()
		c.TransferCreate(w, r)
		return w
	}
	balances := func() map[string]float64 {
		out := map[string]float64{}
		accts, _ := store.ListAccounts()
		for _, a := range accts {
			out[a.Name] = a.Balance
		}
		return out
	}

	if w := transfer("Euro", "Dollar", "100", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "amount received in USD") {
		t.Errorf("EUR→USD without the received amount: status %d, want the form back with an error", w.Code)
	}
	if b := balances(); b["Euro"] != 1000 || b["Dollar"] != 0 {
		t.Fatalf("refused transfer still moved money: %v", b)
	}
	if w := transfer("Euro", "Dollar", "100", "108.50"); w.Code != http.StatusSeeOther {
		t.Fatalf("EUR→USD with received: status %d", w.Code)
	}
	if b := balances(); b["Euro"] != 900 || b["Dollar"] != 108.5 {
		t.Errorf("after EUR→USD: %v want Euro 900, Dollar 108.50", b)
	}
	// Same currency: a stray received figure is ignored.
	transfer("Euro", "Euro2", "50", "999")
	if b := balances(); b["Euro"] != 850 || b["Euro2"] != 50 {
		t.Errorf("after EUR→EUR: %v want Euro 850, Euro2 50", b)
	}
}
