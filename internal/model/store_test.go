package model

import (
	"database/sql"
	"os"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+dir+"/t.db?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	s := New(db)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func TestHouseholdAndAdminBootstrap(t *testing.T) {
	s := testStore(t)
	if err := s.EnsureHousehold(); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureHousehold(); err != nil { // idempotent
		t.Fatal(err)
	}
	hh, err := s.GetHousehold()
	if err != nil || hh.Name != "Household" {
		t.Fatalf("default household: %v %+v", err, hh)
	}
	if err := s.RenameHousehold("Example Household"); err != nil {
		t.Fatal(err)
	}
	if hh, _ = s.GetHousehold(); hh.Name != "Example Household" {
		t.Errorf("rename failed: %q", hh.Name)
	}

	// EnsureAdmin promotes the earliest user when none is admin yet.
	if _, err := s.CreateUser("a", "h", "member"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser("b", "h", "member"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureAdmin(); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.GetUserByUsername("a"); u.Role != "admin" {
		t.Errorf("earliest user not promoted: %q", u.Role)
	}
	if u, _ := s.GetUserByUsername("b"); u.Role != "member" {
		t.Errorf("second user should stay member: %q", u.Role)
	}
}

func TestAutoFetchMeta(t *testing.T) {
	s := testStore(t)
	if s.AutoFetchedToday() {
		t.Error("fresh store should report not fetched today")
	}
	if err := s.MarkAutoFetched(); err != nil {
		t.Fatal(err)
	}
	if !s.AutoFetchedToday() {
		t.Error("after MarkAutoFetched, should report fetched today")
	}
	// Stale (yesterday) marker → not today.
	if err := s.SetMeta("market_last_fetch", "2020-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if s.AutoFetchedToday() {
		t.Error("old marker should not count as today")
	}
	// GetMeta round-trip + missing key.
	if v, _ := s.GetMeta("nope"); v != "" {
		t.Errorf("missing key = %q want empty", v)
	}
}

func TestDedupAndBackup(t *testing.T) {
	s := testStore(t)
	_ = s.CreateAccount("Example Bank", "checking", "EUR", 0, false)
	accts, _ := s.ListAccounts()
	acct := accts[0].ID
	_ = s.CreateTransaction(acct, "2026-05-10", -12.50, nil, "Lidl", "")

	// TxDedupKey is case-insensitive on payee and amount-formatted.
	if TxDedupKey("2026-05-10", -12.50, " lidl ") != TxDedupKey("2026-05-10", -12.5, "Lidl") {
		t.Error("TxDedupKey should normalize payee case/space and amount")
	}
	keys, _ := s.AccountTxKeys(acct)
	if !keys[TxDedupKey("2026-05-10", -12.50, "Lidl")] {
		t.Errorf("existing tx key not found: %v", keys)
	}
	if keys[TxDedupKey("2026-05-11", -12.50, "Lidl")] {
		t.Error("non-existent key should be absent")
	}

	// Backup writes a non-empty SQLite file.
	path := t.TempDir() + "/backup.db"
	if err := s.Backup(path); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Errorf("backup file missing/empty: %v", err)
	}
}

func TestSpendingAnalytics(t *testing.T) {
	s := testStore(t)
	_ = s.EnsureDefaultCategories()
	_ = s.CreateAccount("Example Bank", "savings", "EUR", 0, false)
	_ = s.CreateAccount("Sav", "savings", "EUR", 0, false)
	accts, _ := s.ListAccounts()
	var acct, sav int64
	for _, a := range accts {
		if a.Name == "Example Bank" {
			acct = a.ID
		} else if a.Name == "Sav" {
			sav = a.ID
		}
	}
	cats, _ := s.ListCategories()
	catID := func(name string) *int64 {
		for _, c := range cats {
			if c.Name == name {
				id := c.ID
				return &id
			}
		}
		return nil
	}
	_ = s.CreateTransaction(acct, "2026-05-10", 3000, catID("Salary"), "pay", "")
	_ = s.CreateTransaction(acct, "2026-05-12", -100, catID("Groceries"), "Lidl", "")
	_ = s.CreateTransaction(acct, "2026-05-15", -50, catID("Groceries"), "Aldi", "")
	_ = s.CreateTransaction(acct, "2026-05-20", -40, catID("Dining"), "Pizza", "")
	_ = s.CreateTransfer(acct, sav, "2026-05-25", 200, "savings move") // must be excluded

	m, _ := s.MonthlyTotals()
	if may := m["2026-05"]; may.Income != 3000 || may.Expense != 190 {
		t.Errorf("May totals = %+v want income 3000 / expense 190 (transfer excluded)", may)
	}

	ct, _ := s.SpendByCategory("2026-05-01", "2026-05-31")
	if len(ct) == 0 || ct[0].Category != "Groceries" || ct[0].Amount != 150 {
		t.Errorf("top category = %+v want Groceries/150", ct)
	}
}

func TestCreateTransfer(t *testing.T) {
	s := testStore(t)
	_ = s.CreateAccount("A", "savings", "EUR", 1000, false)
	_ = s.CreateAccount("B", "savings", "EUR", 0, false)
	accts, _ := s.ListAccounts()
	var aID, bID int64
	for _, a := range accts {
		if a.Name == "A" {
			aID = a.ID
		} else if a.Name == "B" {
			bID = a.ID
		}
	}
	if err := s.CreateTransfer(aID, bID, "2026-06-01", 250, "rent pot"); err != nil {
		t.Fatal(err)
	}
	accts, _ = s.ListAccounts()
	for _, a := range accts {
		if a.Name == "A" && a.Balance != 750 {
			t.Errorf("A balance = %v want 750", a.Balance)
		}
		if a.Name == "B" && a.Balance != 250 {
			t.Errorf("B balance = %v want 250", a.Balance)
		}
	}
	txs, _ := s.ListTransactions(10)
	if len(txs) != 2 {
		t.Fatalf("txs = %d want 2", len(txs))
	}
	if !txs[0].TransferID.Valid || txs[0].TransferID.Int64 != txs[1].TransferID.Int64 {
		t.Errorf("legs not grouped by a shared transfer_id: %+v / %+v", txs[0].TransferID, txs[1].TransferID)
	}
	if _, _, total, _ := s.ComputeNAV(); total != 1000 {
		t.Errorf("NAV = %v want 1000 (transfer is net-zero)", total)
	}
}

// Deleting either leg of a transfer removes both, so NAV can't drift; ordinary
// transactions still delete one row at a time.
func TestDeleteTransferLegRemovesBoth(t *testing.T) {
	s := testStore(t)
	_ = s.CreateAccount("A", "savings", "EUR", 1000, false)
	_ = s.CreateAccount("B", "savings", "EUR", 0, false)
	accts, _ := s.ListAccounts()
	aID, bID := accts[0].ID, accts[1].ID
	if err := s.CreateTransfer(aID, bID, "2026-06-01", 300, ""); err != nil {
		t.Fatal(err)
	}
	_ = s.CreateTransaction(aID, "2026-06-02", -20, nil, "Coffee", "")
	_ = s.CreateTransaction(aID, "2026-06-03", -30, nil, "Lunch", "")

	txs, _ := s.ListTransactions(10)
	var leg, coffee int64
	for _, tx := range txs {
		if tx.TransferID.Valid && tx.Amount < 0 {
			leg = tx.ID
		}
		if tx.Payee == "Coffee" {
			coffee = tx.ID
		}
	}
	if err := s.DeleteTransaction(leg); err != nil {
		t.Fatal(err)
	}
	txs, _ = s.ListTransactions(10)
	for _, tx := range txs {
		if tx.TransferID.Valid {
			t.Errorf("orphaned transfer leg left behind: %+v", tx)
		}
	}
	if len(txs) != 2 {
		t.Errorf("txs = %d want 2 (both legs gone, other rows kept)", len(txs))
	}
	if _, _, total, _ := s.ComputeNAV(); total != 950 {
		t.Errorf("NAV = %v want 950 (1000 - 20 - 30, transfer fully undone)", total)
	}

	if err := s.DeleteTransaction(coffee); err != nil {
		t.Fatal(err)
	}
	if txs, _ = s.ListTransactions(10); len(txs) != 1 || txs[0].Payee != "Lunch" {
		t.Errorf("deleting a plain transaction removed the wrong rows: %+v", txs)
	}
}

func TestHoldingGain(t *testing.T) {
	if g, p := HoldingGain(1200, 1000); g != 200 || p != 20 {
		t.Errorf("gain = %v / %v%% want 200 / 20", g, p)
	}
	if g, p := HoldingGain(800, 1000); g != -200 || p != -20 {
		t.Errorf("loss = %v / %v%% want -200 / -20", g, p)
	}
	if _, p := HoldingGain(500, 0); p != 0 {
		t.Errorf("pct with zero cost = %v want 0", p)
	}
}

func TestBuildProjection(t *testing.T) {
	rows := BuildProjection(1000, 100, 12, 2) // 12%/yr ≈ 1%/mo
	if len(rows) != 3 {                       // year 0,1,2
		t.Fatalf("len = %d want 3", len(rows))
	}
	if rows[0].Value != 1000 || rows[0].Contributed != 1000 {
		t.Errorf("year 0 = %+v", rows[0])
	}
	// After 1yr: contributed = 1000 + 12*100 = 2200; value must exceed it (growth>0).
	if rows[1].Contributed != 2200 {
		t.Errorf("contributed yr1 = %v want 2200", rows[1].Contributed)
	}
	if rows[1].Value <= rows[1].Contributed {
		t.Errorf("value %v should exceed contributed %v (positive return)", rows[1].Value, rows[1].Contributed)
	}
	if rows[2].Growth <= rows[1].Growth {
		t.Error("growth should compound (year 2 > year 1)")
	}

	// Zero return → value equals contributions exactly.
	flat := BuildProjection(0, 100, 0, 1)
	if flat[1].Value != 1200 || flat[1].Growth != 0 {
		t.Errorf("zero-return yr1 = %+v want value 1200 / growth 0", flat[1])
	}
}

func TestBuildNavForecast(t *testing.T) {
	// €10k start, +€1k planned / +€500 actual, no growth, 24 months.
	pts := BuildNavForecast(10000, 1000, 500, 0, 24)
	if len(pts) != 25 {
		t.Fatalf("len = %d want 25", len(pts))
	}
	if pts[0].Planned != 10000 || pts[0].Actual != 10000 {
		t.Errorf("month 0 = %+v want both 10000", pts[0])
	}
	// Linear with 0% return: planned = 10000 + 24*1000 = 34000; actual = 22000.
	if pts[24].Planned != 34000 {
		t.Errorf("planned m24 = %v want 34000", pts[24].Planned)
	}
	if pts[24].Actual != 22000 {
		t.Errorf("actual m24 = %v want 22000", pts[24].Actual)
	}
	// With growth, the balance must exceed the linear (contributions-only) total.
	g := BuildNavForecast(10000, 1000, 1000, 12, 24)
	if g[24].Planned <= 34000 {
		t.Errorf("with 12%% return, m24 %v should exceed linear 34000", g[24].Planned)
	}
}

func TestAPITokens(t *testing.T) {
	s := testStore(t)
	alice, _ := s.CreateUser("alice", "h", "admin")
	bob, _ := s.CreateUser("bob", "h", "member")

	if err := s.CreateAPIToken(alice, "iPhone", "hash-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAPIToken(bob, "iPad", "hash-b"); err != nil {
		t.Fatal(err)
	}

	// A token resolves to its owner (and stamps last_used).
	u, err := s.UserByAPITokenHash("hash-a")
	if err != nil || u == nil || u.ID != alice || u.Username != "alice" {
		t.Fatalf("hash-a should resolve to alice: %+v %v", u, err)
	}
	if _, err := s.UserByAPITokenHash("nope"); err == nil {
		t.Error("unknown token hash should error")
	}

	// Listing is per-user; revoke is scoped by user_id.
	at, _ := s.ListAPITokens(alice)
	if len(at) != 1 || at[0].Name != "iPhone" {
		t.Fatalf("alice tokens = %+v", at)
	}
	_ = s.DeleteAPIToken(bob, at[0].ID) // bob can't revoke alice's token
	if u, _ := s.UserByAPITokenHash("hash-a"); u == nil {
		t.Error("cross-user revoke must be a no-op")
	}
	_ = s.DeleteAPIToken(alice, at[0].ID)
	if _, err := s.UserByAPITokenHash("hash-a"); err == nil {
		t.Error("alice's own revoke should remove the token")
	}
}

func TestPocketMoneyIsPrivatePerUser(t *testing.T) {
	s := testStore(t)
	alice, err := s.CreateUser("alice", "h", "member")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.CreateUser("bob", "h", "member")
	if err != nil {
		t.Fatal(err)
	}

	// Alice tops up €100 and spends €30 on beers; Bob tops up €50.
	_ = s.AddPocketEntry(alice, "2026-06-01", 100, "Allowance", "")
	_ = s.AddPocketEntry(alice, "2026-06-02", -30, "Beers", "with the boys")
	_ = s.AddPocketEntry(bob, "2026-06-01", 50, "Allowance", "")

	// Each user sees only their own entries and balance.
	aEntries, _ := s.ListPocketEntries(alice)
	if len(aEntries) != 2 {
		t.Fatalf("alice entries = %d want 2 (no leakage from bob)", len(aEntries))
	}
	if ab, _ := s.PocketBalance(alice); ab != 70 {
		t.Errorf("alice balance = %v want 70", ab)
	}
	if bb, _ := s.PocketBalance(bob); bb != 50 {
		t.Errorf("bob balance = %v want 50", bb)
	}

	// Category breakdown is private and only counts spends.
	cats, _ := s.PocketByCategory(alice)
	if len(cats) != 1 || cats[0].Category != "Beers" || cats[0].Amount != 30 {
		t.Errorf("alice breakdown = %+v want [Beers 30]", cats)
	}

	// Bob cannot delete Alice's entry even with its id (scoped by user_id).
	target := aEntries[len(aEntries)-1].ID // an alice row
	_ = s.DeletePocketEntry(bob, target)
	if after, _ := s.ListPocketEntries(alice); len(after) != 2 {
		t.Error("bob was able to delete alice's pocket entry — privacy breach")
	}
	// Alice can delete her own.
	_ = s.DeletePocketEntry(alice, target)
	if after, _ := s.ListPocketEntries(alice); len(after) != 1 {
		t.Errorf("alice delete failed: %d entries left", len(after))
	}
}

func TestBudgetStatus(t *testing.T) {
	cases := []struct {
		actual, budget float64
		wantPct        int
		wantState      string
	}{
		{150, 400, 38, "ok"},
		{360, 400, 90, "low"},   // exactly 90%
		{505, 400, 126, "over"}, // the groceries example: over budget
		{0, 400, 0, "ok"},
		{50, 0, 0, "none"}, // no budget set
	}
	for _, c := range cases {
		pct, state := BudgetStatus(c.actual, c.budget)
		if pct != c.wantPct || state != c.wantState {
			t.Errorf("BudgetStatus(%v,%v) = %d/%q want %d/%q", c.actual, c.budget, pct, state, c.wantPct, c.wantState)
		}
	}
}

func TestSpendingAccountsAndCategorySpend(t *testing.T) {
	s := testStore(t)
	_ = s.EnsureDefaultCategories()
	// One normal account (counts in NAV) and one spending account (excluded).
	_ = s.CreateAccount("Savings", "savings", "EUR", 1000, false)
	_ = s.CreateAccount("Daily", "checking", "EUR", 500, true)
	accts, _ := s.ListAccounts()
	var daily, savings int64
	for _, a := range accts {
		if a.Name == "Daily" {
			daily = a.ID
		} else {
			savings = a.ID
		}
	}
	if n, _ := s.CountSpendingAccounts(); n != 1 {
		t.Fatalf("spending accounts = %d want 1", n)
	}
	// NAV excludes the €500 spending account → only the €1000 savings counts.
	if _, _, total, _ := s.ComputeNAV(); total != 1000 {
		t.Errorf("NAV = %v want 1000 (spending account excluded)", total)
	}

	cats, _ := s.ListCategories()
	var groceries *int64
	for _, c := range cats {
		if c.Name == "Groceries" {
			id := c.ID
			groceries = &id
		}
	}
	if groceries == nil {
		t.Fatal("no Groceries category seeded")
	}
	// The groceries example: 150 + 300 + 55 from the spending account; plus a €40
	// grocery spend from the savings account that must NOT count when scoped.
	_ = s.CreateTransaction(daily, "2026-06-01", -150, groceries, "Lidl", "")
	_ = s.CreateTransaction(daily, "2026-06-24", -300, groceries, "Aldi", "")
	_ = s.CreateTransaction(daily, "2026-06-30", -55, groceries, "Spar", "")
	_ = s.CreateTransaction(savings, "2026-06-15", -40, groceries, "Other", "")

	scoped, _ := s.CategorySpend("2026-06-01", "2026-06-31", true)
	if got := scoped[*groceries]; got != 505 {
		t.Errorf("spending-scoped groceries = %v want 505", got)
	}
	all, _ := s.CategorySpend("2026-06-01", "2026-06-31", false)
	if got := all[*groceries]; got != 545 {
		t.Errorf("all-accounts groceries = %v want 545", got)
	}
}

func TestBuildBurnDown(t *testing.T) {
	// €100k, +€2k/mo, a €80k spend at month 2, over 4 months.
	pts := BuildBurnDown(100000, 2000, nil, map[int]float64{2: 80000}, 4)
	want := []float64{100000, 102000, 24000, 26000, 28000} // m0..m4
	if len(pts) != len(want) {
		t.Fatalf("len = %d want %d", len(pts), len(want))
	}
	for i, w := range want {
		if pts[i].Balance != w {
			t.Errorf("month %d balance = %v want %v", i, pts[i].Balance, w)
		}
	}
	if pts[2].Spend != 80000 {
		t.Errorf("month 2 spend = %v want 80000", pts[2].Spend)
	}

	// Overspending drives the balance negative (a useful signal, not an error).
	neg := BuildBurnDown(1000, 0, nil, map[int]float64{1: 5000}, 2)
	if neg[1].Balance != -4000 {
		t.Errorf("negative balance = %v want -4000", neg[1].Balance)
	}

	// Recurring per-month outflows (upcoming loan repayments) draw the balance
	// down without registering as lump-spend dots.
	withOuts := BuildBurnDown(10000, 1000, map[int]float64{2: 400, 3: 400}, nil, 3)
	wantOuts := []float64{10000, 11000, 11600, 12200} // +1000, then +600 net
	for i, w := range wantOuts {
		if withOuts[i].Balance != w {
			t.Errorf("outs month %d balance = %v want %v", i, withOuts[i].Balance, w)
		}
	}
	if withOuts[2].Spend != 0 {
		t.Errorf("recurring outflow must not appear as a spend dot, got %v", withOuts[2].Spend)
	}
}

func TestMonthsToTarget(t *testing.T) {
	if m := MonthsToTarget(10000, 1000, 0, 10000, 100); m != 0 {
		t.Errorf("already-there = %d want 0", m)
	}
	// 0 + 1000/mo, no growth → 50000 at month 50.
	if m := MonthsToTarget(0, 1000, 0, 50000, 600); m != 50 {
		t.Errorf("months = %d want 50", m)
	}
	// Unreachable: no monthly, no growth, below target.
	if m := MonthsToTarget(1000, 0, 0, 50000, 600); m != -1 {
		t.Errorf("unreachable = %d want -1", m)
	}
	// Growth alone can reach it eventually.
	if m := MonthsToTarget(10000, 0, 12, 11000, 600); m <= 0 {
		t.Errorf("growth-only should reach target, got %d", m)
	}
}

func TestComputeLoan(t *testing.T) {
	// €20k borrowed (budget 25k − 5k deposit) at 6% APR over 60 months.
	p := ComputeLoan(25000, 5000, 6, 60)
	if p.Principal != 20000 {
		t.Errorf("principal = %v want 20000", p.Principal)
	}
	// Standard amortization: monthly ≈ €386.66, total interest ≈ €3199.36.
	if p.Monthly < 386.6 || p.Monthly > 386.7 {
		t.Errorf("monthly = %.4f want ~386.66", p.Monthly)
	}
	if p.TotalInterest < 3199 || p.TotalInterest > 3200 {
		t.Errorf("total interest = %.2f want ~3199", p.TotalInterest)
	}
	if d := p.TotalRepaid - (p.Monthly * 60); d > 1e-6 || d < -1e-6 {
		t.Errorf("total repaid %.4f != monthly*60", p.TotalRepaid)
	}
	if d := p.TotalCost - (p.Deposit + p.TotalRepaid); d > 1e-6 || d < -1e-6 {
		t.Errorf("total cost %.4f != deposit+repaid", p.TotalCost)
	}

	// 0% APR → straight split, no interest.
	z := ComputeLoan(1200, 0, 0, 12)
	if z.Monthly != 100 || z.TotalInterest != 0 {
		t.Errorf("0%% loan = %+v want monthly 100 / interest 0", z)
	}

	// Deposit covers the whole budget → nothing to borrow.
	none := ComputeLoan(1000, 1500, 5, 36)
	if none.Principal != 0 || none.Monthly != 0 || none.Deposit != 1000 {
		t.Errorf("over-deposited loan = %+v want principal/monthly 0, deposit capped to 1000", none)
	}

	// Term 0 → no loan modelled.
	off := ComputeLoan(5000, 0, 5, 0)
	if off.Monthly != 0 || off.TotalInterest != 0 {
		t.Errorf("term-0 loan = %+v want zeros", off)
	}
}

func TestFinanceWithLoan(t *testing.T) {
	// Budget 25k, deposit 5k, 6% over 60 months. The plan's monthly becomes the
	// loan payment, Financed becomes the deposit, MonthsLeft becomes the term.
	base := ProjectPlan{Target: 25000, Spent: 0, Remaining: 25000, Financed: 10000, ToSave: 15000, MonthlyToSave: 1250, MonthsLeft: 12}
	got := FinanceWithLoan(base, 5000, 6, 60, "", time.Time{})
	loan := ComputeLoan(25000, 5000, 6, 60)
	if got.Financed != 5000 {
		t.Errorf("Financed = %v want 5000 (deposit)", got.Financed)
	}
	if got.MonthlyToSave != loan.Monthly {
		t.Errorf("MonthlyToSave = %v want loan monthly %v", got.MonthlyToSave, loan.Monthly)
	}
	if got.MonthsLeft != 60 {
		t.Errorf("MonthsLeft = %v want 60 (term)", got.MonthsLeft)
	}
	if got.Overdue {
		t.Error("loan-financed plan should not be overdue")
	}
	// Budget/Spent are preserved from the aggregate.
	if got.Target != 25000 || got.Remaining != 25000 {
		t.Errorf("Target/Remaining changed: %+v", got)
	}
}

func TestMonthlyAmount(t *testing.T) {
	cases := []struct {
		amt  float64
		freq string
		want float64
	}{
		{12, "monthly", 12},
		{600, "yearly", 50},
		{30, "quarterly", 10},
		{120, "weekly", 120 * 52 / 12},
		{99, "unknown", 99}, // defaults to monthly
	}
	for _, c := range cases {
		if got := MonthlyAmount(c.amt, c.freq); got != c.want {
			t.Errorf("MonthlyAmount(%v,%q) = %v want %v", c.amt, c.freq, got, c.want)
		}
	}
}

func TestNextDueDate(t *testing.T) {
	now := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	if d, ok := NextDueDate(15, now); !ok || d != "2026-06-15" { // later this month
		t.Errorf("due 15 = %q ok=%v", d, ok)
	}
	if d, _ := NextDueDate(5, now); d != "2026-07-05" { // already passed → next month
		t.Errorf("due 5 = %q want 2026-07-05", d)
	}
	if d, _ := NextDueDate(10, now); d != "2026-06-10" { // today counts (on or after)
		t.Errorf("due 10 (today) = %q", d)
	}
	apr := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC) // 30-day month: 31 clamps to 30
	if d, _ := NextDueDate(31, apr); d != "2026-04-30" {
		t.Errorf("clamp = %q want 2026-04-30", d)
	}
	if _, ok := NextDueDate(0, now); ok {
		t.Error("0 should be unset")
	}
}

func TestOperatingCostsAndIncome(t *testing.T) {
	s := testStore(t)
	if err := s.EnsureHousehold(); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMonthlyIncome(4000); err != nil {
		t.Fatal(err)
	}
	if hh, _ := s.GetHousehold(); hh.MonthlyIncome != 4000 {
		t.Errorf("income = %v want 4000", hh.MonthlyIncome)
	}

	if err := s.CreateOperatingCost("Netflix", "Subscriptions", 12, "EUR", "monthly", 15, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOperatingCost("Car insurance", "Insurance", 600, "EUR", "yearly", 0, nil, true); err != nil {
		t.Fatal(err)
	}
	costs, _ := s.ListOperatingCosts()
	if len(costs) != 2 {
		t.Fatalf("costs = %d want 2", len(costs))
	}

	// Toggle one inactive, then update it.
	id := costs[0].ID
	if err := s.SetOperatingCostActive(id, false); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.GetOperatingCost(id); c.Active {
		t.Error("cost should be inactive after toggle")
	}
	if err := s.UpdateOperatingCost(id, "Netflix Premium", "Subscriptions", 18, "EUR", "monthly", 20, nil, true); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.GetOperatingCost(id); c.Name != "Netflix Premium" || c.Amount != 18 || !c.Active {
		t.Errorf("updated cost = %+v", c)
	}

	if err := s.DeleteOperatingCost(id); err != nil {
		t.Fatal(err)
	}
	if costs, _ = s.ListOperatingCosts(); len(costs) != 1 {
		t.Errorf("after delete, costs = %d want 1", len(costs))
	}
}

func TestFXAndNAV(t *testing.T) {
	s := testStore(t)
	// A USD account (1000 USD) and a EUR account (500 EUR).
	if err := s.CreateAccount("US", "savings", "USD", 1000, false); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAccount("EU", "savings", "EUR", 500, false); err != nil {
		t.Fatal(err)
	}

	// No FX rate yet → USD treated as EUR (rate 1).
	cash, _, total, err := s.ComputeNAV()
	if err != nil {
		t.Fatal(err)
	}
	if cash != 1500 || total != 1500 {
		t.Fatalf("pre-FX NAV cash=%v total=%v want 1500", cash, total)
	}

	// Set USD→EUR = 0.9; NAV recomputes: 1000*0.9 + 500 = 1400.
	if err := s.UpsertFXRate("usd", 0.9); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetFXRates(); r["USD"] != 0.9 || r["EUR"] != 1 {
		t.Errorf("rates = %+v", r)
	}
	cash, _, total, _ = s.ComputeNAV()
	if total != 1400 {
		t.Errorf("post-FX NAV = %v want 1400", total)
	}

	// Upsert again (update path).
	if err := s.UpsertFXRate("USD", 0.8); err != nil {
		t.Fatal(err)
	}
	if _, _, total, _ = s.ComputeNAV(); total != 1300 {
		t.Errorf("updated-FX NAV = %v want 1300", total)
	}

	// DistinctCurrencies excludes EUR; only USD remains.
	ccys, _ := s.DistinctCurrencies()
	if len(ccys) != 1 || ccys[0] != "USD" {
		t.Errorf("DistinctCurrencies = %v want [USD]", ccys)
	}
}

func TestHoldingPriceUpdate(t *testing.T) {
	s := testStore(t)
	if err := s.CreateHolding("AAPL", "Apple", "stock", 10, 100, "USD", nil); err != nil {
		t.Fatal(err)
	}
	syms, _ := s.DistinctHoldingSymbols()
	if len(syms) != 1 || syms[0] != "AAPL" {
		t.Fatalf("symbols = %v", syms)
	}
	// case-insensitive update
	if err := s.UpdateHoldingPrice("aapl", 311.78); err != nil {
		t.Fatal(err)
	}
	holds, _ := s.ListHoldings()
	if holds[0].Price != 311.78 || holds[0].Value < 3117.79 || holds[0].Value > 3117.81 {
		t.Errorf("holding after price update = %+v", holds[0])
	}
}

func TestNavSnapshots(t *testing.T) {
	s := testStore(t)

	// EnsureDailySnapshot writes one row on an empty DB, and is a no-op same day.
	if err := s.EnsureDailySnapshot(); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureDailySnapshot(); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListNavSnapshots(0)
	if len(got) != 1 {
		t.Fatalf("after two EnsureDailySnapshot calls, rows = %d want 1 (daily dedupe)", len(got))
	}

	// Manual snapshots always insert; ListNavSnapshots returns ascending by time.
	if err := s.RecordNavSnapshot(100, 50, 150); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordNavSnapshot(200, 100, 300); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListNavSnapshots(0)
	if len(got) != 3 {
		t.Fatalf("rows = %d want 3", len(got))
	}
	last := got[len(got)-1]
	if last.NavEUR != 300 || last.CashEUR != 200 || last.InvEUR != 100 {
		t.Errorf("last snapshot = %+v want nav 300 / cash 200 / inv 100", last)
	}
	if asc := got[1].TakenAt <= got[2].TakenAt; !asc {
		t.Error("ListNavSnapshots should be ascending by taken_at")
	}

	// limit returns the most recent N (still ascending).
	if lim, _ := s.ListNavSnapshots(2); len(lim) != 2 || lim[1].NavEUR != 300 {
		t.Errorf("limit(2) = %+v want 2 rows ending at 300", lim)
	}
}

func TestComputePlan(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// €2200 items + 10% buffer = €2420; 40% financed; ~6 months to Dec 1.
	p := ComputePlan(2200, 0, 40, 10, "2026-12-01", now)
	if p.Target != 2420 {
		t.Errorf("target = %v want 2420", p.Target)
	}
	approx := func(got, want float64) bool { return got-want < 0.001 && want-got < 0.001 }
	if !approx(p.Financed, 968) {
		t.Errorf("financed = %v want 968 (40%%)", p.Financed)
	}
	if !approx(p.ToSave, 1452) {
		t.Errorf("toSave = %v want 1452", p.ToSave)
	}
	if p.MonthsLeft != 6 {
		t.Errorf("monthsLeft = %d want 6", p.MonthsLeft)
	}
	if got := p.MonthlyToSave; got < 241 || got > 243 {
		t.Errorf("monthlyToSave = %v want ~242", got)
	}

	// Past target date → overdue, whole remainder due now, no division.
	od := ComputePlan(1000, 0, 0, 0, "2026-01-01", now)
	if !od.Overdue || od.MonthlyToSave != 1000 {
		t.Errorf("overdue plan = %+v", od)
	}

	// No date → lump sum, not overdue.
	nd := ComputePlan(1000, 0, 25, 0, "", now)
	if nd.Overdue || nd.MonthlyToSave != 750 {
		t.Errorf("no-date plan = %+v", nd)
	}
}

func TestProjectAndPurchase(t *testing.T) {
	s := testStore(t)
	if err := s.CreateAccount("Example Bank", "savings", "EUR", 5000, false); err != nil {
		t.Fatal(err)
	}
	accts, _ := s.ListAccounts()
	acctID := accts[0].ID

	pid, err := s.CreateProjectInput(ProjectInput{Name: "Kitchen", TargetDate: "2026-12-01", Status: "saving", FinancePct: 40, BufferPct: 10, FinanceMethod: "save"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProjectItem(pid, nil, "Oven", 1200, true); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProjectItem(pid, nil, "Chairs", 600, false); err != nil {
		t.Fatal(err)
	}

	p, _ := s.GetProject(pid)
	if p.ItemsTotal != 1800 {
		t.Errorf("items total = %v want 1800", p.ItemsTotal)
	}

	items, _ := s.ListProjectItems(pid)
	// Pay the oven in full for €1250 from the account (over the estimate → done).
	if err := s.PayItem(items[0].ID, acctID, "2026-06-01", 1250, nil, false); err != nil {
		t.Fatal(err)
	}

	// A negative, project-tagged transaction must now exist and reduce the balance.
	accts, _ = s.ListAccounts()
	if accts[0].Balance != 3750 { // 5000 − 1250
		t.Errorf("balance = %v want 3750", accts[0].Balance)
	}
	p, _ = s.GetProject(pid)
	if p.Spent != 1250 {
		t.Errorf("spent = %v want 1250", p.Spent)
	}
	it, _ := s.GetProjectItem(items[0].ID)
	if !it.Purchased || !it.TransactionID.Valid {
		t.Errorf("item not linked to a transaction: %+v", it)
	}

	if n, _ := s.CountUnpurchasedItems(pid); n != 1 {
		t.Errorf("unpurchased = %d want 1", n)
	}
}

func TestPayItemDepositThenBalance(t *testing.T) {
	s := testStore(t)
	if err := s.CreateAccount("Example Bank", "savings", "EUR", 5000, false); err != nil {
		t.Fatal(err)
	}
	accts, _ := s.ListAccounts()
	acctID := accts[0].ID

	pid, err := s.CreateProjectInput(ProjectInput{Name: "Kitchen", TargetDate: "2026-12-01", Status: "saving", FinanceMethod: "save"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProjectItem(pid, nil, "Oven", 1000, true); err != nil {
		t.Fatal(err)
	}
	items, _ := s.ListProjectItems(pid)
	id := items[0].ID

	// Deposit of €200: not flagged final and below the estimate → item stays open,
	// but the deposit already draws down spent + cash.
	if err := s.PayItem(id, acctID, "2026-06-01", 200, nil, false); err != nil {
		t.Fatal(err)
	}
	it, _ := s.GetProjectItem(id)
	if it.Purchased {
		t.Errorf("a deposit should not complete the item")
	}
	if it.AmountPaid != 200 || it.Remaining() != 800 {
		t.Errorf("after deposit: paid=%v remaining=%v want 200/800", it.AmountPaid, it.Remaining())
	}
	if it.ActualCost.Valid {
		t.Errorf("actual_cost should stay NULL until done, got %+v", it.ActualCost)
	}
	if p, _ := s.GetProject(pid); p.Spent != 200 {
		t.Errorf("spent after deposit = %v want 200", p.Spent)
	}
	if accts, _ = s.ListAccounts(); accts[0].Balance != 4800 {
		t.Errorf("balance after deposit = %v want 4800", accts[0].Balance)
	}

	// Pay the €800 balance → reaches the estimate → completes automatically.
	if err := s.PayItem(id, acctID, "2026-07-01", 800, nil, false); err != nil {
		t.Fatal(err)
	}
	it, _ = s.GetProjectItem(id)
	if !it.Purchased || it.AmountPaid != 1000 {
		t.Errorf("after balance: purchased=%v paid=%v want true/1000", it.Purchased, it.AmountPaid)
	}
	if !it.ActualCost.Valid || it.ActualCost.Float64 != 1000 {
		t.Errorf("actual_cost = %+v want 1000", it.ActualCost)
	}
	if p, _ := s.GetProject(pid); p.Spent != 1000 {
		t.Errorf("spent = %v want 1000", p.Spent)
	}
	if pays, _ := s.ListItemPayments(id); len(pays) != 2 {
		t.Errorf("payments recorded = %d want 2", len(pays))
	}
	if n, _ := s.CountUnpurchasedItems(pid); n != 0 {
		t.Errorf("unpurchased = %d want 0", n)
	}
}

func TestSpentSelfHealsOnPaymentDelete(t *testing.T) {
	s := testStore(t)
	if err := s.CreateAccount("Example Bank", "savings", "EUR", 5000, false); err != nil {
		t.Fatal(err)
	}
	accts, _ := s.ListAccounts()
	pid, _ := s.CreateProjectInput(ProjectInput{Name: "Kitchen", FinanceMethod: "save"})
	if err := s.CreateProjectItem(pid, nil, "Oven", 1000, true); err != nil {
		t.Fatal(err)
	}
	items, _ := s.ListProjectItems(pid)
	id := items[0].ID
	if err := s.PayItem(id, accts[0].ID, "2026-06-01", 200, nil, false); err != nil { // deposit
		t.Fatal(err)
	}
	if err := s.PayItem(id, accts[0].ID, "2026-07-01", 800, nil, false); err != nil { // balance
		t.Fatal(err)
	}
	if p, _ := s.GetProject(pid); p.Spent != 1000 {
		t.Fatalf("spent after both payments = %v want 1000", p.Spent)
	}

	// Delete the deposit transaction from the ledger. Because spent/paid are derived
	// (not a stored copy), they must self-correct rather than stay overstated.
	pays, _ := s.ListItemPayments(id) // newest first: [balance, deposit]
	deposit := pays[len(pays)-1]
	if err := s.DeleteTransaction(deposit.ID); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GetProject(pid); p.Spent != 800 {
		t.Errorf("spent after deleting the €200 deposit = %v want 800", p.Spent)
	}
	if it, _ := s.GetProjectItem(id); it.AmountPaid != 800 || it.Remaining() != 200 {
		t.Errorf("item after delete: paid=%v remaining=%v want 800/200", it.AmountPaid, it.Remaining())
	}
}

func TestMigrateBackfillsLegacyPaymentLink(t *testing.T) {
	s := testStore(t)
	if err := s.CreateAccount("Example Bank", "savings", "EUR", 5000, false); err != nil {
		t.Fatal(err)
	}
	accts, _ := s.ListAccounts()
	pid, _ := s.CreateProjectInput(ProjectInput{Name: "X", FinanceMethod: "save"})
	if err := s.CreateProjectItem(pid, nil, "Oven", 1000, false); err != nil {
		t.Fatal(err)
	}
	items, _ := s.ListProjectItems(pid)
	if err := s.PayItem(items[0].ID, accts[0].ID, "2026-06-01", 1000, nil, true); err != nil {
		t.Fatal(err)
	}
	// Simulate a legacy purchase: the item still links its transaction via
	// transaction_id, but the payment row predates project_item_id (so it's NULL).
	if _, err := s.db.Exec(`UPDATE transactions SET project_item_id=NULL WHERE project_item_id IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GetProject(pid); p.Spent != 0 {
		t.Fatalf("precondition: derived spent should be 0 with the link broken, got %v", p.Spent)
	}
	// Re-running Migrate reconnects the payment to its item via the transaction_id link.
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GetProject(pid); p.Spent != 1000 {
		t.Errorf("spent after relink backfill = %v want 1000", p.Spent)
	}
}

func TestPayItemFinalUnderEstimate(t *testing.T) {
	s := testStore(t)
	if err := s.CreateAccount("Example Bank", "savings", "EUR", 5000, false); err != nil {
		t.Fatal(err)
	}
	accts, _ := s.ListAccounts()
	pid, _ := s.CreateProjectInput(ProjectInput{Name: "Living room", FinanceMethod: "save"})
	if err := s.CreateProjectItem(pid, nil, "Sofa", 1000, true); err != nil {
		t.Fatal(err)
	}
	items, _ := s.ListProjectItems(pid)
	// Got a discount: paid €900 and explicitly marked it the final payment.
	if err := s.PayItem(items[0].ID, accts[0].ID, "2026-06-01", 900, nil, true); err != nil {
		t.Fatal(err)
	}
	it, _ := s.GetProjectItem(items[0].ID)
	if !it.Purchased {
		t.Errorf("the final flag should complete the item even under the estimate")
	}
	if it.AmountPaid != 900 {
		t.Errorf("amount_paid = %v want 900", it.AmountPaid)
	}
}

func TestAggregatePhases(t *testing.T) {
	a := ProjectPlan{Target: 1000, Spent: 200, Financed: 400, ToSave: 600, MonthlyToSave: 100, MonthsLeft: 6}
	b := ProjectPlan{Target: 500, Spent: 500, Financed: 200, ToSave: 300, MonthlyToSave: 50, MonthsLeft: 3, Overdue: true}

	// b is done (fully purchased) → counts toward budget/spent but not monthly/overdue.
	agg := AggregatePhases([]ProjectPlan{a, b}, []bool{false, true})
	if agg.Target != 1500 || agg.Spent != 700 || agg.Financed != 600 || agg.ToSave != 900 {
		t.Errorf("sums wrong: %+v", agg)
	}
	if agg.MonthlyToSave != 100 {
		t.Errorf("monthly = %v want 100 (done phase excluded)", agg.MonthlyToSave)
	}
	if agg.MonthsLeft != 6 {
		t.Errorf("monthsLeft = %d want 6 (max)", agg.MonthsLeft)
	}
	if agg.Overdue {
		t.Error("overdue should be false — only the done phase was overdue")
	}
	if agg.Remaining != 800 {
		t.Errorf("remaining = %v want 800", agg.Remaining)
	}
}

func TestBuildSaveSchedule(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// Phase A: €100/mo for 3 months; Phase B: €50/mo for 6 months.
	sched := BuildSaveSchedule([]float64{100, 50}, []int{3, 6}, now)

	if len(sched) != 6 { // horizon = max(3, 6)
		t.Fatalf("len = %d want 6", len(sched))
	}
	if sched[0].Label != "Jun 2026" {
		t.Errorf("first label = %q want Jun 2026", sched[0].Label)
	}
	// Months 1-3: both contribute (150); months 4-6: only B (50).
	for i, want := range []float64{150, 150, 150, 50, 50, 50} {
		if sched[i].Total != want {
			t.Errorf("month %d total = %v want %v", i+1, sched[i].Total, want)
		}
	}
	// After A finishes, its column is zero.
	if sched[3].ByGroup[0] != 0 || sched[3].ByGroup[1] != 50 {
		t.Errorf("month 4 byGroup = %v want [0 50]", sched[3].ByGroup)
	}
	if sched[5].Label != "Nov 2026" {
		t.Errorf("last label = %q want Nov 2026", sched[5].Label)
	}

	// No active savers → empty schedule.
	if s := BuildSaveSchedule(nil, nil, now); len(s) != 0 {
		t.Errorf("empty schedule len = %d want 0", len(s))
	}
}

func TestPhasesAndItemGrouping(t *testing.T) {
	s := testStore(t)
	pid, err := s.CreateProjectInput(ProjectInput{Name: "Kitchen", TargetDate: "2026-12-01", Status: "saving", FinanceMethod: "save"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePhase(pid, "Cabinets", "2026-09-01"); err != nil {
		t.Fatal(err)
	}
	phases, _ := s.ListPhases(pid)
	if len(phases) != 1 {
		t.Fatalf("phases = %d want 1", len(phases))
	}
	phID := phases[0].ID

	if err := s.CreateProjectItem(pid, &phID, "Cabinet doors", 800, true); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProjectItem(pid, nil, "Misc", 100, true); err != nil {
		t.Fatal(err)
	}

	if phases, _ = s.ListPhases(pid); phases[0].ItemsTotal != 800 {
		t.Errorf("phase items total = %v want 800", phases[0].ItemsTotal)
	}

	items, _ := s.ListProjectItems(pid)
	var phased, unphased int
	for _, it := range items {
		switch {
		case it.PhaseID.Valid && it.PhaseID.Int64 == phID:
			phased++
		case !it.PhaseID.Valid:
			unphased++
		}
	}
	if phased != 1 || unphased != 1 {
		t.Errorf("grouping: phased=%d unphased=%d (want 1/1)", phased, unphased)
	}

	// Move the unphased "Misc" item into the phase, then back to General.
	var miscID int64
	for _, it := range items {
		if it.Name == "Misc" {
			miscID = it.ID
		}
	}
	if err := s.SetItemPhase(miscID, &phID); err != nil {
		t.Fatal(err)
	}
	if it, _ := s.GetProjectItem(miscID); !it.PhaseID.Valid || it.PhaseID.Int64 != phID {
		t.Errorf("move into phase failed: %+v", it.PhaseID)
	}
	if err := s.SetItemPhase(miscID, nil); err != nil {
		t.Fatal(err)
	}
	if it, _ := s.GetProjectItem(miscID); it.PhaseID.Valid {
		t.Errorf("move back to General failed: still phased")
	}

	// Deleting a phase drops its items back to General (phase_id → NULL).
	if err := s.DeletePhase(phID); err != nil {
		t.Fatal(err)
	}
	items, _ = s.ListProjectItems(pid)
	for _, it := range items {
		if it.PhaseID.Valid {
			t.Errorf("item %q still phased after phase delete", it.Name)
		}
	}
	if ph, _ := s.ListPhases(pid); len(ph) != 0 {
		t.Errorf("phases remain after delete: %d", len(ph))
	}
}

func TestInviteLifecycle(t *testing.T) {
	s := testStore(t)
	uid, err := s.CreateUser("admin", "h", "admin")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.CreateInvite("hash-good", "demo_member", uid, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	iv, err := s.GetValidInvite("hash-good")
	if err != nil || iv.Label != "demo_member" {
		t.Fatalf("expected valid invite: %v %+v", err, iv)
	}
	if pend, _ := s.ListPendingInvites(); len(pend) != 1 {
		t.Errorf("pending=%d want 1", len(pend))
	}

	// Single-use: once consumed it no longer validates.
	if err := s.MarkInviteUsed(iv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetValidInvite("hash-good"); err == nil {
		t.Error("used invite should not validate")
	}

	// Expired invites are rejected and not listed as pending.
	if err := s.CreateInvite("hash-exp", "x", uid, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetValidInvite("hash-exp"); err != ErrInviteExpired {
		t.Errorf("want ErrInviteExpired, got %v", err)
	}
	if pend, _ := s.ListPendingInvites(); len(pend) != 0 {
		t.Errorf("pending after use+expire=%d want 0", len(pend))
	}
}

func TestIncFailedLoginReturnsCount(t *testing.T) {
	s := testStore(t)
	uid, err := s.CreateUser("u", "h", "member")
	if err != nil {
		t.Fatal(err)
	}
	for want := 1; want <= 3; want++ {
		got, err := s.IncFailedLogin(uid)
		if err != nil {
			t.Fatalf("IncFailedLogin: %v", err)
		}
		if got != want {
			t.Errorf("IncFailedLogin #%d returned %d, want %d", want, got, want)
		}
	}
	if err := s.ResetFailedLogin(uid); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.IncFailedLogin(uid); got != 1 {
		t.Errorf("after reset, IncFailedLogin returned %d, want 1", got)
	}
}

func TestFinanceWithLoanStartDate(t *testing.T) {
	base := ProjectPlan{Target: 25000}
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	loan := ComputeLoan(25000, 5000, 6, 60)

	// Future start: nothing hits the budget yet; LoanStart carries the date.
	fut := FinanceWithLoan(base, 5000, 6, 60, "2026-09-01", now)
	if fut.MonthlyToSave != 0 {
		t.Errorf("pre-start MonthlyToSave = %v want 0", fut.MonthlyToSave)
	}
	if fut.LoanStart != "2026-09-01" {
		t.Errorf("LoanStart = %q want 2026-09-01", fut.LoanStart)
	}
	if fut.MonthsLeft != 60 {
		t.Errorf("pre-start MonthsLeft = %v want 60 (full term)", fut.MonthsLeft)
	}

	// Start this month: full payment, full term (same as no date).
	cur := FinanceWithLoan(base, 5000, 6, 60, "2026-06-01", now)
	if cur.MonthlyToSave != loan.Monthly || cur.MonthsLeft != 60 || cur.LoanStart != "" {
		t.Errorf("same-month start = %+v want monthly %v / 60 mo", cur, loan.Monthly)
	}

	// Mid-term (started 10 months ago): same payment, 50 months remaining.
	mid := FinanceWithLoan(base, 5000, 6, 60, "2025-08-01", now)
	if mid.MonthlyToSave != loan.Monthly || mid.MonthsLeft != 50 {
		t.Errorf("mid-term = monthly %v / %v mo want %v / 50", mid.MonthlyToSave, mid.MonthsLeft, loan.Monthly)
	}

	// Fully repaid (started 60+ months ago): no further commitment.
	done := FinanceWithLoan(base, 5000, 6, 60, "2021-06-01", now)
	if done.MonthlyToSave != 0 || done.MonthsLeft != 0 {
		t.Errorf("repaid = %+v want 0/0", done)
	}

	// Unparseable date: behaves like no date.
	bad := FinanceWithLoan(base, 5000, 6, 60, "not-a-date", now)
	if bad.MonthlyToSave != loan.Monthly || bad.MonthsLeft != 60 || bad.LoanStart != "" {
		t.Errorf("bad date should fall back to immediate: %+v", bad)
	}
}

func TestPayLoanDeposit(t *testing.T) {
	s := testStore(t)
	if err := s.CreateAccount("Example Bank", "savings", "EUR", 5000, false); err != nil {
		t.Fatal(err)
	}
	accts, _ := s.ListAccounts()
	acctID := accts[0].ID
	pid, err := s.CreateProjectInput(ProjectInput{Name: "Solar", Status: "saving",
		FinanceMethod: "loan", LoanDeposit: 1500, LoanAPR: 5, LoanTermMonths: 48, LoanStartDate: "2026-09-01"})
	if err != nil {
		t.Fatal(err)
	}

	p, _ := s.GetProject(pid)
	if p.LoanStartDate != "2026-09-01" {
		t.Errorf("LoanStartDate = %q want 2026-09-01", p.LoanStartDate)
	}
	if p.DepositPaid != 0 || p.DepositPaidDate != "" {
		t.Fatalf("fresh project should have no deposit paid: %+v", p)
	}

	// Pay in increments: €500 now, €1000 later — the derived total accumulates
	// and the date tracks the most recent payment.
	if err := s.PayLoanDeposit(pid, acctID, "2026-08-20", 500, nil); err != nil {
		t.Fatal(err)
	}
	p, _ = s.GetProject(pid)
	if p.DepositPaid != 500 || p.DepositPaidDate != "2026-08-20" {
		t.Errorf("after first increment: paid=%v date=%q want 500 / 2026-08-20", p.DepositPaid, p.DepositPaidDate)
	}
	if err := s.PayLoanDeposit(pid, acctID, "2026-08-25", 1000, nil); err != nil {
		t.Fatal(err)
	}
	p, _ = s.GetProject(pid)
	if p.DepositPaid != 1500 || p.DepositPaidDate != "2026-08-25" {
		t.Errorf("after second increment: paid=%v date=%q want 1500 / 2026-08-25", p.DepositPaid, p.DepositPaidDate)
	}
	accts, _ = s.ListAccounts()
	if accts[0].Balance != 3500 {
		t.Errorf("balance = %v want 3500", accts[0].Balance)
	}
	if pays, _ := s.ListDepositPayments(pid); len(pays) != 2 || -pays[0].Amount != 1000 {
		t.Errorf("payment history = %+v want 2 entries, newest 1000", pays)
	}

	// Deleting ONE increment self-heals the derived total (ledger is the truth).
	pays, _ := s.ListDepositPayments(pid)
	first := pays[len(pays)-1] // oldest = the €500
	if err := s.DeleteTransaction(first.ID); err != nil {
		t.Fatal(err)
	}
	p, _ = s.GetProject(pid)
	if p.DepositPaid != 1000 {
		t.Errorf("after deleting the €500 increment: paid=%v want 1000", p.DepositPaid)
	}
	// …and the gap can be paid again.
	if err := s.PayLoanDeposit(pid, acctID, "2026-08-28", 500, nil); err != nil {
		t.Fatal(err)
	}
	if p, _ = s.GetProject(pid); p.DepositPaid != 1500 {
		t.Errorf("after topping back up: paid=%v want 1500", p.DepositPaid)
	}

	// EDITING an increment is allowed and the derived total follows the ledger.
	pays, _ = s.ListDepositPayments(pid)
	edit := pays[0]
	if err := s.UpdateTransaction(edit.ID, acctID, edit.Date, -250, nil, "Solar", "Loan deposit"); err != nil {
		t.Fatal(err)
	}
	if p, _ = s.GetProject(pid); p.DepositPaid != 1250 {
		t.Errorf("after editing a payment to €250: paid=%v want 1250", p.DepositPaid)
	}
}

func TestDepositPaidSumClamp(t *testing.T) {
	s := testStore(t)
	if err := s.CreateAccount("Example Bank", "savings", "EUR", 5000, false); err != nil {
		t.Fatal(err)
	}
	accts, _ := s.ListAccounts()
	pid, _ := s.CreateProjectInput(ProjectInput{Name: "Solar", Status: "saving",
		FinanceMethod: "loan", LoanDeposit: 1500, LoanTermMonths: 48})
	if err := s.PayLoanDeposit(pid, accts[0].ID, "2026-08-20", 1000, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.PayLoanDeposit(pid, accts[0].ID, "2026-08-22", 500, nil); err != nil {
		t.Fatal(err)
	}
	// A tagged refund (positive amount) legitimately reduces the derived total.
	pays, _ := s.ListDepositPayments(pid)
	newest := pays[0]
	if err := s.UpdateTransaction(newest.ID, accts[0].ID, newest.Date, 200, nil, "Solar", "refund"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GetProject(pid); p.DepositPaid != 800 { // 1000 − 200
		t.Errorf("after a €200 refund row: DepositPaid = %v want 800", p.DepositPaid)
	}
	// Even if every row flips positive, the derived sum clamps at 0.
	if _, err := s.db.Exec(`UPDATE transactions SET amount=1500 WHERE deposit_project_id=?`, pid); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GetProject(pid); p.DepositPaid != 0 {
		t.Errorf("all-positive deposit rows: DepositPaid = %v want 0 (clamped)", p.DepositPaid)
	}
}

func TestMigrateBackfillsLegacyDepositLink(t *testing.T) {
	s := testStore(t)
	if err := s.CreateAccount("Example Bank", "savings", "EUR", 5000, false); err != nil {
		t.Fatal(err)
	}
	accts, _ := s.ListAccounts()
	pid, _ := s.CreateProjectInput(ProjectInput{Name: "Solar", Status: "saving",
		FinanceMethod: "loan", LoanDeposit: 1500, LoanTermMonths: 48})
	if err := s.PayLoanDeposit(pid, accts[0].ID, "2026-08-20", 1500, nil); err != nil {
		t.Fatal(err)
	}
	// Simulate a v1 deposit: the one-slot link is set, the per-row tag is not.
	pays, _ := s.ListDepositPayments(pid)
	if _, err := s.db.Exec(`UPDATE projects SET deposit_transaction_id=? WHERE id=?`, pays[0].ID, pid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE transactions SET deposit_project_id=NULL WHERE id=?`, pays[0].ID); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GetProject(pid); p.DepositPaid != 0 {
		t.Fatalf("precondition: derived paid should be 0 with the tag stripped, got %v", p.DepositPaid)
	}
	// Re-running Migrate reconnects the v1 link as a tagged payment.
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GetProject(pid); p.DepositPaid != 1500 {
		t.Errorf("after backfill: DepositPaid = %v want 1500", p.DepositPaid)
	}
}

func TestFinanceWithLoanUpcomingMonthly(t *testing.T) {
	base := ProjectPlan{Target: 25000}
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	loan := ComputeLoan(25000, 5000, 6, 60)
	fut := FinanceWithLoan(base, 5000, 6, 60, "2026-09-01", now)
	if fut.UpcomingMonthly != loan.Monthly {
		t.Errorf("pre-start UpcomingMonthly = %v want %v", fut.UpcomingMonthly, loan.Monthly)
	}
	// Started loans don't set it (the live MonthlyToSave carries the figure).
	cur := FinanceWithLoan(base, 5000, 6, 60, "2026-06-01", now)
	if cur.UpcomingMonthly != 0 {
		t.Errorf("started loan UpcomingMonthly = %v want 0", cur.UpcomingMonthly)
	}
}

func TestDepositPaidDateIgnoresRefunds(t *testing.T) {
	s := testStore(t)
	if err := s.CreateAccount("Example Bank", "savings", "EUR", 5000, false); err != nil {
		t.Fatal(err)
	}
	accts, _ := s.ListAccounts()
	pid, _ := s.CreateProjectInput(ProjectInput{Name: "Solar", Status: "saving",
		FinanceMethod: "loan", LoanDeposit: 1500, LoanTermMonths: 48})
	if err := s.PayLoanDeposit(pid, accts[0].ID, "2026-08-20", 1000, nil); err != nil {
		t.Fatal(err)
	}
	// A later refund row must not become the "last payment" date.
	if err := s.PayLoanDeposit(pid, accts[0].ID, "2026-08-25", 200, nil); err != nil {
		t.Fatal(err)
	}
	pays, _ := s.ListDepositPayments(pid)
	if err := s.UpdateTransaction(pays[0].ID, accts[0].ID, "2026-08-30", 200, nil, "Solar", "refund"); err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetProject(pid)
	if p.DepositPaid != 800 { // 1000 − 200 refund
		t.Errorf("DepositPaid = %v want 800", p.DepositPaid)
	}
	if p.DepositPaidDate != "2026-08-20" {
		t.Errorf("DepositPaidDate = %q want 2026-08-20 (the real payment, not the refund)", p.DepositPaidDate)
	}
}

// Insights/savings-rate aggregates must be in EUR, like NAV and CategorySpend:
// a −£100 spend at 1.15 is €115, not 100.
func TestMonthlyTotalsAndSpendByCategoryConvertToEUR(t *testing.T) {
	s := testStore(t)
	_ = s.EnsureDefaultCategories()
	_ = s.CreateAccount("EUR acct", "checking", "EUR", 0, false)
	_ = s.CreateAccount("GBP acct", "checking", "GBP", 0, false)
	if err := s.UpsertFXRate("GBP", 1.15); err != nil {
		t.Fatal(err)
	}
	var eurID, gbpID int64
	accts, _ := s.ListAccounts()
	for _, a := range accts {
		if a.Currency == "GBP" {
			gbpID = a.ID
		} else {
			eurID = a.ID
		}
	}
	var groceries *int64
	cats, _ := s.ListCategories()
	for _, c := range cats {
		if c.Name == "Groceries" {
			id := c.ID
			groceries = &id
		}
	}
	_ = s.CreateTransaction(eurID, "2026-05-01", 2000, nil, "Salary", "")
	_ = s.CreateTransaction(gbpID, "2026-05-02", 1000, nil, "UK pay", "")
	_ = s.CreateTransaction(eurID, "2026-05-10", -50, groceries, "Lidl", "")
	_ = s.CreateTransaction(gbpID, "2026-05-11", -100, groceries, "Tesco", "")
	_ = s.CreateTransaction(gbpID, "2026-05-12", -20, nil, "Pub", "")

	m, err := s.MonthlyTotals()
	if err != nil {
		t.Fatal(err)
	}
	near := func(a, b float64) bool { return a-b < 0.001 && b-a < 0.001 }
	if may := m["2026-05"]; !near(may.Income, 3150) || !near(may.Expense, 188) || may.Month != "2026-05" {
		t.Errorf("May totals = %+v want income 3150 / expense 188 (50 + 115 + 23)", may)
	}

	ct, err := s.SpendByCategory("2026-05-01", "2026-05-31")
	if err != nil {
		t.Fatal(err)
	}
	if len(ct) != 2 || ct[0].Category != "Groceries" || !near(ct[0].Amount, 165) ||
		ct[1].Category != "Uncategorized" || !near(ct[1].Amount, 23) {
		t.Errorf("SpendByCategory = %+v want Groceries 165 (merged across currencies), Uncategorized 23", ct)
	}
}
