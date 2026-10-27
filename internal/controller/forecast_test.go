package controller

import (
	"database/sql"
	"strconv"
	"testing"
	"time"

	"lira/internal/config"
	"lira/internal/model"
	"lira/internal/view"

	_ "modernc.org/sqlite"
)

// testController wires a controller to a fresh migrated store and the real
// templates, for handler-level tests.
func testController(t *testing.T) (*Controller, *model.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+dir+"/t.db?_pragma=foreign_keys(ON)")
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

func TestAddLoanStream(t *testing.T) {
	outs := map[int]float64{}
	// €400/mo loan starting month 3, 4 months left, 6-month horizon → months 3..6.
	addLoanStream(outs, 3, 4, 6, 400)
	want := map[int]float64{3: 400, 4: 400, 5: 400, 6: 400}
	for m := 1; m <= 6; m++ {
		if outs[m] != want[m] {
			t.Errorf("month %d out = %v want %v", m, outs[m], want[m])
		}
	}
	// A short term ends inside the horizon; a second loan stacks additively.
	outs2 := map[int]float64{}
	addLoanStream(outs2, 1, 2, 12, 100)
	addLoanStream(outs2, 2, 2, 12, 50)
	if outs2[1] != 100 || outs2[2] != 150 || outs2[3] != 50 || outs2[4] != 0 {
		t.Errorf("stacked streams = %v", outs2)
	}
	// startMonth < 1 clamps to 1 (defensive).
	outs3 := map[int]float64{}
	addLoanStream(outs3, 0, 2, 12, 100)
	if outs3[1] != 100 || outs3[2] != 100 || outs3[3] != 0 {
		t.Errorf("clamped start = %v", outs3)
	}
	// A term extending past the horizon is clipped to it.
	outs4 := map[int]float64{}
	addLoanStream(outs4, 4, 60, 6, 400)
	if outs4[4] != 400 || outs4[6] != 400 || outs4[7] != 0 || len(outs4) != 3 {
		t.Errorf("horizon clip = %v want months 4..6 only", outs4)
	}
}

// A paid deposit must draw down the project's displayed Spent/Remaining — not
// just financing figures. (Item payments alone would leave the deposit invisible.)
func TestProjectGroupsCountsPaidDeposit(t *testing.T) {
	c, store := testController(t)
	if err := store.CreateAccount("BOV", "savings", "EUR", 10000, false); err != nil {
		t.Fatal(err)
	}
	accts, _ := store.ListAccounts()
	pid, err := store.CreateProjectInput(model.ProjectInput{Name: "Solar", Status: "saving",
		FinanceMethod: "loan", LoanDeposit: 1500, LoanAPR: 5, LoanTermMonths: 48})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateProjectItem(pid, nil, "Panels", 8000, true); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
	p, _ := store.GetProject(pid)
	_, agg := c.projectGroups(p, now)
	if agg.Spent != 0 || agg.Remaining != agg.Target {
		t.Fatalf("before deposit: spent=%v remaining=%v (target %v)", agg.Spent, agg.Remaining, agg.Target)
	}

	// Pay the deposit in two increments; Spent/Remaining must follow.
	if err := store.PayLoanDeposit(pid, accts[0].ID, "2026-07-01", 500, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.PayLoanDeposit(pid, accts[0].ID, "2026-07-10", 1000, nil); err != nil {
		t.Fatal(err)
	}
	p, _ = store.GetProject(pid)
	_, agg = c.projectGroups(p, now)
	if agg.Spent != 1500 {
		t.Errorf("after deposit: spent = %v want 1500", agg.Spent)
	}
	if agg.Remaining != agg.Target-1500 {
		t.Errorf("after deposit: remaining = %v want %v", agg.Remaining, agg.Target-1500)
	}

	// Switching back to "save" keeps the paid money counted.
	if err := store.UpdateProject(pid, model.ProjectInput{Name: "Solar", Status: "saving",
		FinanceMethod: "save", LoanDeposit: 1500, LoanTermMonths: 48}); err != nil {
		t.Fatal(err)
	}
	p, _ = store.GetProject(pid)
	_, agg = c.projectGroups(p, now)
	if agg.Spent != 1500 {
		t.Errorf("method switched: spent = %v want 1500 (deposit still counted)", agg.Spent)
	}
}

func TestBuildCashLedger(t *testing.T) {
	labels := make([]string, 13)
	for i := range labels {
		labels[i] = "M" + strconv.Itoa(i)
	}

	// No loans: identical shape to the classic ledger — saving folds + spends,
	// nothing trailing after the last spend.
	spends := []burnSpend{{Month: 3, Label: "Sofa", When: "d3", Amount: 5000}}
	rows := buildCashLedger(10000, 1000, nil, spends, labels, 12)
	if len(rows) != 2 {
		t.Fatalf("no-loan rows = %d want 2 (saving + spend): %+v", len(rows), rows)
	}
	if rows[0].Kind != "save" || rows[0].Amount != 3000 || rows[0].Balance != 13000 {
		t.Errorf("saving fold = %+v", rows[0])
	}
	if rows[1].Balance != 8000 {
		t.Errorf("after spend balance = %v want 8000", rows[1].Balance)
	}

	// With a loan stream: explicit red rows per stretch, gross saving stays gross,
	// and a trailing pair covers repayments after the last spend. The final row's
	// balance must equal BuildBurnDown's horizon-end balance (chart ↔ ledger).
	outs := map[int]float64{}
	addLoanStream(outs, 2, 60, 12, 400) // €400/mo from month 2 through horizon
	rows = buildCashLedger(10000, 1000, outs, spends, labels, 12)
	// months 1..3: saving 3000; loan payments months 2,3 = 800; spend 5000; then
	// trailing months 4..12: saving 9000, loan payments 9×400 = 3600.
	if len(rows) != 5 {
		t.Fatalf("loan rows = %d want 5: %+v", len(rows), rows)
	}
	if rows[1].Label != "Loan payments × 2 mo" || rows[1].Amount != 800 || rows[1].Balance != 12200 {
		t.Errorf("window loan row = %+v", rows[1])
	}
	if rows[3].Kind != "save" || rows[3].Amount != 9000 || rows[3].Months != 9 {
		t.Errorf("trailing saving = %+v", rows[3])
	}
	if rows[4].Label != "Loan payments × 9 mo" || rows[4].Amount != 3600 {
		t.Errorf("trailing loan row = %+v", rows[4])
	}
	pts := model.BuildBurnDown(10000, 1000, outs, map[int]float64{3: 5000}, 12)
	if got, want := rows[len(rows)-1].Balance, pts[12].Balance; got != want {
		t.Errorf("ledger end balance %v != chart end balance %v", got, want)
	}

	// A single payment month says "Loan payment", not "× 1 mo".
	one := map[int]float64{}
	addLoanStream(one, 1, 1, 12, 250)
	rows = buildCashLedger(1000, 0, one, nil, labels, 12)
	if len(rows) != 1 || rows[0].Label != "Loan payment" || rows[0].Amount != 250 || rows[0].Balance != 750 {
		t.Errorf("single payment = %+v", rows)
	}

	// No spends at all but a loan running: the timeline still shows the horizon
	// (previously it was empty while the chart declined).
	rows = buildCashLedger(5000, 300, outs, nil, labels, 12)
	if len(rows) != 2 {
		t.Fatalf("no-spend loan rows = %d want 2: %+v", len(rows), rows)
	}
	pts = model.BuildBurnDown(5000, 300, outs, nil, 12)
	if rows[1].Balance != pts[12].Balance {
		t.Errorf("no-spend end balance %v != chart %v", rows[1].Balance, pts[12].Balance)
	}
}
