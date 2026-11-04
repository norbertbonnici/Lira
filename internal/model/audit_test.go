package model

import (
	"database/sql"
	"reflect"
	"testing"
)

func tx(id, acct int64, date string, amt float64, payee string) Transaction {
	return Transaction{ID: id, AccountID: acct, Date: date, Amount: amt, Payee: payee}
}

func cat(t Transaction, id int64, name, kind string) Transaction {
	t.CategoryID = sql.NullInt64{Int64: id, Valid: true}
	t.CategoryName, t.CategoryKind = name, kind
	return t
}

func TestNormalizePayee(t *testing.T) {
	cases := map[string]string{
		"CARD 1234 LIDL MT 03/11": "card lidl mt",
		"  Lidl   MT ":            "lidl mt",
		"1234/56":                 "",
		"Café-Rio":                "café rio",
	}
	for in, want := range cases {
		if got := NormalizePayee(in); got != want {
			t.Errorf("NormalizePayee(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindDuplicates(t *testing.T) {
	transfer := tx(9, 1, "2026-03-01", -50, "Lidl")
	transfer.TransferID = sql.NullInt64{Int64: 1, Valid: true}
	txs := []Transaction{
		tx(1, 1, "2026-03-01", -50, "Lidl"),
		tx(2, 1, "2026-03-01", -50.001, "lidl "), // same cents + payee (case/space-insensitive)
		tx(3, 2, "2026-03-01", -50, "Lidl"),      // other account: not a duplicate
		tx(4, 1, "2026-03-03", -50, "Lidl"),      // 2 days later
		tx(5, 1, "2026-03-01", -51, "Lidl"),      // different amount
		transfer,                                 // transfer legs ignored
	}
	got := FindDuplicates(txs, 0)
	if len(got) != 1 || !reflect.DeepEqual(got[0].IDs, []int64{1, 2}) {
		t.Fatalf("exact: got %+v", got)
	}
	got = FindDuplicates(txs, 2)
	if len(got) != 1 || !reflect.DeepEqual(got[0].IDs, []int64{1, 2, 4}) {
		t.Fatalf("window 2: got %+v", got)
	}
	if got := FindDuplicates(txs[:1], 0); len(got) != 0 {
		t.Fatalf("single row should not be a duplicate: %+v", got)
	}
}

func TestSuggestCategories(t *testing.T) {
	txs := []Transaction{
		cat(tx(1, 1, "2026-01-01", -20, "CARD 11 LIDL"), 3, "Groceries", "expense"),
		cat(tx(2, 1, "2026-01-08", -25, "CARD 22 LIDL"), 3, "Groceries", "expense"),
		cat(tx(3, 1, "2026-01-09", -5, "CARD 33 LIDL"), 7, "Other", "expense"),
		cat(tx(4, 1, "2026-01-10", 5, "CARD 44 LIDL"), 2, "Other income", "income"), // refund history
		cat(tx(9, 1, "2026-01-11", 4, "CARD 45 LIDL"), 2, "Other income", "income"),
		cat(tx(10, 1, "2026-01-12", 6, "CARD 46 LIDL"), 2, "Other income", "income"),
		cat(tx(11, 1, "2026-01-01", 900, "Savings"), 1, "Salary", "income"), // thin history: 2 rows
		cat(tx(12, 1, "2026-01-02", 800, "Savings"), 2, "Other income", "income"),
		cat(tx(13, 1, "2026-01-03", -7, "Kiosk"), 3, "Groceries", "expense"), // split history: 2/4 = 0.5
		cat(tx(14, 1, "2026-01-04", -7, "Kiosk"), 3, "Groceries", "expense"),
		cat(tx(15, 1, "2026-01-05", -7, "Kiosk"), 6, "Dining", "expense"),
		cat(tx(16, 1, "2026-01-06", -7, "Kiosk"), 7, "Other", "expense"),
		tx(5, 1, "2026-02-01", -30, "CARD 55 LIDL"),
		tx(6, 1, "2026-02-02", -10, "CARD 66 LIDL"),
		tx(7, 1, "2026-02-03", 3, "CARD 77 LIDL"), // positive → matched against refund history
		tx(8, 1, "2026-02-04", -9, "New Shop"),    // no history
		tx(17, 1, "2026-02-05", 1000, "Savings"),  // below SuggestMinHistory
		tx(18, 1, "2026-02-06", -7, "Kiosk"),      // below SuggestMinConfidence
	}
	got := SuggestCategories(txs)
	if len(got) != 5 {
		t.Fatalf("want 3 groups, got %+v", got)
	}
	g := got[0]
	if g.Count != 2 || g.Total != -40 || g.SuggestedID != 3 || g.Suggested != "Groceries" || g.Confidence != 0.67 || g.Basis != 3 {
		t.Errorf("lidl spend group: %+v", g)
	}
	byPayee := map[string]UncategorizedGroup{}
	for _, g := range got[1:] {
		byPayee[g.Payee] = g
	}
	if r := byPayee["CARD 77 LIDL"]; r.SuggestedID != 2 || r.Confidence != 1 {
		t.Errorf("refund group should suggest the income category: %+v", r)
	}
	for _, p := range []string{"New Shop", "Savings", "Kiosk"} {
		if n := byPayee[p]; n.SuggestedID != 0 || n.Confidence != 0 || n.Basis != 0 || n.Count != 1 {
			t.Errorf("%s: weak or missing history should give no suggestion: %+v", p, n)
		}
	}
}

func TestFindAnomalies(t *testing.T) {
	txs := []Transaction{
		tx(1, 1, "2026-01-01", -10, "Cafe"),
		tx(2, 1, "2026-01-02", -12, "Cafe"),
		tx(3, 1, "2026-01-03", -11, "Cafe"),
		tx(4, 1, "2026-01-04", -110, "Cafe"), // extra digit
		tx(5, 1, "2026-01-05", -30, "Cafe"),  // 3× median would be ~33: not flagged
		cat(tx(6, 1, "2026-01-06", 40, "Shop"), 3, "Groceries", "expense"),
		cat(tx(7, 1, "2026-01-07", -40, "Employer"), 1, "Salary", "income"),
		tx(8, 1, "2026-12-01", -5, "Later"),
		tx(9, 1, "2026-01-09", 0, "Nothing"),
	}
	got := FindAnomalies(txs, "2026-06-01")
	kinds := map[int64]string{}
	for _, a := range got {
		kinds[a.Tx.ID] = a.Kind
	}
	want := map[int64]string{4: "outlier", 6: "sign_mismatch", 7: "sign_mismatch", 8: "future_dated", 9: "zero_amount"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("anomalies = %v, want %v", kinds, want)
	}
	if got[0].Tx.ID != 8 {
		t.Errorf("findings should be newest first, got %d first", got[0].Tx.ID)
	}
}

func TestSearchTransactions(t *testing.T) {
	s := testStore(t)
	if err := s.CreateAccount("Main", "current", "EUR", 0, false); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAccount("Savings", "savings", "USD", 0, false); err != nil {
		t.Fatal(err)
	}
	accts, _ := s.ListAccounts()
	a1, a2 := accts[0].ID, accts[1].ID
	if accts[0].Name != "Main" {
		a1, a2 = a2, a1
	}
	if err := s.CreateCategory("Groceries", "expense"); err != nil {
		t.Fatal(err)
	}
	cats, _ := s.ListCategories()
	gid := cats[0].ID
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.CreateTransaction(a1, "2026-01-05", -20, &gid, "Lidl", "weekly shop"))
	must(s.CreateTransaction(a1, "2026-01-10", -500, nil, "Landlord", "rent 100%"))
	must(s.CreateTransaction(a1, "2026-02-01", 2000, nil, "Employer", ""))
	must(s.CreateTransfer(a1, a2, "2026-02-02", 100, 100, "to savings"))

	count := func(f TxFilter) int {
		t.Helper()
		txs, err := s.SearchTransactions(f)
		if err != nil {
			t.Fatal(err)
		}
		n, err := s.CountTransactions(f)
		if err != nil {
			t.Fatal(err)
		}
		if f.Limit == 0 && n != len(txs) {
			t.Fatalf("count %d != rows %d for %+v", n, len(txs), f)
		}
		return len(txs)
	}
	cases := []struct {
		f    TxFilter
		want int
	}{
		{TxFilter{}, 3}, // transfers excluded by default
		{TxFilter{IncludeTransfers: true}, 5},
		{TxFilter{From: "2026-01-06", To: "2026-01-31"}, 1},
		{TxFilter{AccountID: a2, IncludeTransfers: true}, 1},
		{TxFilter{CategoryID: gid}, 1},
		{TxFilter{Uncategorized: true}, 2},
		{TxFilter{Query: "LIDL"}, 1},
		{TxFilter{Query: "100%"}, 1}, // LIKE wildcards are escaped
		{TxFilter{Query: "%"}, 1},
		{TxFilter{MinAbs: 400}, 2},
		{TxFilter{MinAbs: 10, MaxAbs: 600}, 2},
		{TxFilter{Kind: "income"}, 1},
		{TxFilter{Kind: "expense"}, 2},
		{TxFilter{Limit: 2}, 2},
		{TxFilter{Limit: 2, Offset: 2}, 1},
	}
	for _, c := range cases {
		if got := count(c.f); got != c.want {
			t.Errorf("%+v: got %d rows, want %d", c.f, got, c.want)
		}
	}
	txs, _ := s.SearchTransactions(TxFilter{CategoryID: gid})
	if tx := txs[0]; tx.CategoryName != "Groceries" || tx.CategoryKind != "expense" || tx.AccountCurrency != "EUR" || tx.AccountName != "Main" {
		t.Errorf("joined fields: %+v", tx)
	}
	txs, _ = s.SearchTransactions(TxFilter{})
	if txs[0].Date != "2026-02-01" {
		t.Errorf("newest first: got %s", txs[0].Date)
	}
}
