package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// The API stores only YYYY-MM-DD dates: a timestamp or another layout would
// silently drop out of month grouping and date-range reports.
func TestAPIRejectsBadDates(t *testing.T) {
	c, store := testController(t)
	uid, _ := store.CreateUser("api", "h", "admin")
	u, _ := store.GetUserByID(uid)
	_ = store.CreateAccount("Bank", "checking", "EUR", 0, false)
	accts, _ := store.ListAccounts()
	acct := strconv.FormatInt(accts[0].ID, 10)

	post := func(h apiHandler, body string) int {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), u)
		return w.Code
	}
	for _, d := range []string{"2026-10-31T10:00:00Z", "26/10/2026", "2026-02-30"} {
		if code := post(c.APICreateTransaction, `{"accountId":`+acct+`,"date":"`+d+`","amount":-5}`); code != 422 {
			t.Errorf("transaction date %q: status %d want 422", d, code)
		}
		if code := post(c.APICreatePocket, `{"date":"`+d+`","amount":-5}`); code != 422 {
			t.Errorf("pocket date %q: status %d want 422", d, code)
		}
	}
	if code := post(c.APICreateTransaction, `{"accountId":`+acct+`,"date":"2026-10-31","amount":-5}`); code != 201 {
		t.Errorf("valid transaction: status %d want 201", code)
	}
	if code := post(c.APICreatePocket, `{"amount":-5}`); code != 201 {
		t.Errorf("pocket with default date: status %d want 201", code)
	}
	if txs, _ := store.ListTransactions(10); len(txs) != 1 {
		t.Errorf("stored %d transactions want 1", len(txs))
	}
	if n, _ := store.ListPocketEntries(uid); len(n) != 1 {
		t.Errorf("stored %d pocket entries want 1", len(n))
	}
}

// The iOS add-transaction picker reads /categories and sends the chosen id;
// an unknown id is a 422, not a 500 from the foreign key.
func TestAPICategoriesAndCategorizedCreate(t *testing.T) {
	c, store := testController(t)
	uid, _ := store.CreateUser("api", "h", "admin")
	u, _ := store.GetUserByID(uid)
	_ = store.CreateAccount("Bank", "checking", "EUR", 0, false)
	accts, _ := store.ListAccounts()
	acct := strconv.FormatInt(accts[0].ID, 10)
	if err := store.CreateCategory("Groceries", "expense"); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	c.APICategories(w, httptest.NewRequest(http.MethodGet, "/", nil), u)
	if w.Code != 200 {
		t.Fatalf("categories: status %d", w.Code)
	}
	var cats []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cats); err != nil {
		t.Fatal(err)
	}
	var groceries int64
	for _, cat := range cats {
		if cat.Name == "Groceries" && cat.Kind == "expense" {
			groceries = cat.ID
		}
	}
	if groceries == 0 {
		t.Fatalf("Groceries missing from %+v", cats)
	}

	post := func(body string) int {
		w := httptest.NewRecorder()
		c.APICreateTransaction(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), u)
		return w.Code
	}
	if code := post(`{"accountId":` + acct + `,"date":"2026-11-05","amount":-5,"categoryId":999999}`); code != 422 {
		t.Errorf("unknown categoryId: status %d want 422", code)
	}
	if code := post(`{"accountId":` + acct + `,"date":"2026-11-05","amount":-5,"categoryId":` + strconv.FormatInt(groceries, 10) + `}`); code != 201 {
		t.Fatalf("categorized create: status %d want 201", code)
	}
	txs, _ := store.ListTransactions(10)
	if len(txs) != 1 || !txs[0].CategoryID.Valid || txs[0].CategoryID.Int64 != groceries {
		t.Errorf("stored %+v, want one Groceries transaction", txs)
	}
}
