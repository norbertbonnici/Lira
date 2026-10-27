package controller

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"lira/internal/model"
)

func TestValidateProject(t *testing.T) {
	cases := []struct {
		name    string
		in      model.ProjectInput
		wantErr bool
	}{
		{"valid save", model.ProjectInput{Name: "Kitchen", FinanceMethod: "save"}, false},
		{"valid loan with term", model.ProjectInput{Name: "Solar", FinanceMethod: "loan", LoanTermMonths: 60}, false},
		{"empty name", model.ProjectInput{Name: "", FinanceMethod: "save"}, true},
		{"loan without term", model.ProjectInput{Name: "Solar", FinanceMethod: "loan", LoanTermMonths: 0}, true},
		{"loan negative term", model.ProjectInput{Name: "Solar", FinanceMethod: "loan", LoanTermMonths: -3}, true},
		{"save ignores missing term", model.ProjectInput{Name: "Kitchen", FinanceMethod: "save", LoanTermMonths: 0}, false},
		{"valid loan start date", model.ProjectInput{Name: "Solar", FinanceMethod: "loan", LoanTermMonths: 60, LoanStartDate: "2026-09-01"}, false},
		{"bad loan start date", model.ProjectInput{Name: "Solar", FinanceMethod: "loan", LoanTermMonths: 60, LoanStartDate: "next march"}, true},
		{"impossible loan start date", model.ProjectInput{Name: "Solar", FinanceMethod: "loan", LoanTermMonths: 60, LoanStartDate: "2026-02-31"}, true},
		{"empty start date ok", model.ProjectInput{Name: "Solar", FinanceMethod: "loan", LoanTermMonths: 60, LoanStartDate: ""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validateProject(tc.in)
			if (got != "") != tc.wantErr {
				t.Errorf("validateProject(%+v) = %q, wantErr=%v", tc.in, got, tc.wantErr)
			}
		})
	}
}

func TestStillToDraw(t *testing.T) {
	plan := model.ProjectPlan{Financed: 1500}
	loan := &model.Project{FinanceMethod: "loan", LoanTermMonths: 48, DepositPaid: 1500}
	if got := stillToDraw(loan, plan); got != 0 {
		t.Errorf("paid loan deposit: stillToDraw = %v want 0", got)
	}
	partial := &model.Project{FinanceMethod: "loan", LoanTermMonths: 48, DepositPaid: 1000}
	if got := stillToDraw(partial, plan); got != 500 {
		t.Errorf("partial deposit: stillToDraw = %v want 500", got)
	}
	// Switching the method back to "save" must not double-count the paid cash
	// (projectGroups' aggregate Spent includes the paid deposit).
	switched := &model.Project{FinanceMethod: "save", DepositPaid: 1500}
	if got := stillToDraw(switched, model.ProjectPlan{Financed: 2000, Spent: 1500}); got != 500 {
		t.Errorf("method switched after paying: stillToDraw = %v want 500", got)
	}
	// Save project: item payments already left NAV, so they reduce the draw.
	// 100% financed €10k with €6k paid → €4k still to draw, not €10k.
	saving := &model.Project{FinanceMethod: "save"}
	if got := stillToDraw(saving, model.ProjectPlan{Financed: 10000, Spent: 6000}); got != 4000 {
		t.Errorf("save project with 6k paid: stillToDraw = %v want 4000", got)
	}
	// Loan project: item payments are the loan's money, only the deposit counts.
	loanItems := &model.Project{FinanceMethod: "loan", LoanTermMonths: 48, DepositPaid: 500}
	if got := stillToDraw(loanItems, model.ProjectPlan{Financed: 1500, Spent: 9000}); got != 1000 {
		t.Errorf("loan project with item payments: stillToDraw = %v want 1000", got)
	}
	// Overpaid clamps at zero rather than going negative.
	over := &model.Project{FinanceMethod: "loan", LoanTermMonths: 48, DepositPaid: 9999}
	if got := stillToDraw(over, plan); got != 0 {
		t.Errorf("overpaid: stillToDraw = %v want 0", got)
	}
	// No deposit paid: full Financed still to draw.
	fresh := &model.Project{FinanceMethod: "save"}
	if got := stillToDraw(fresh, plan); got != 1500 {
		t.Errorf("fresh: stillToDraw = %v want 1500", got)
	}
}

func TestLoanRepaying(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	loan := func(start string, term int) *model.Project {
		return &model.Project{FinanceMethod: "loan", LoanTermMonths: term, LoanStartDate: start}
	}
	cases := []struct {
		name string
		p    *model.Project
		want bool
	}{
		{"save project", &model.Project{FinanceMethod: "save"}, false},
		{"loan without term", loan("2026-01-01", 0), false},
		{"starts later", loan("2027-03-01", 12), true},
		{"starts this month", loan("2026-10-30", 12), true},
		{"mid-term", loan("2026-01-15", 12), true},
		{"last payment this month", loan("2025-11-01", 12), true},
		{"repaid", loan("2025-10-01", 12), false},
		{"long repaid", loan("2020-01-01", 24), false},
		{"undated runs", loan("", 12), true},
	}
	for _, tc := range cases {
		if got := loanRepaying(tc.p, now); got != tc.want {
			t.Errorf("%s: loanRepaying = %v want %v", tc.name, got, tc.want)
		}
	}
}

// Paying a loan project's last item must not complete it while repayments are
// still running (completing drops the loan payment from the budget); a save
// project still auto-completes.
func TestItemPurchaseKeepsRunningLoanActive(t *testing.T) {
	c, store := newTestController(t)
	_ = store.CreateAccount("Current", "checking", "EUR", 50000, false)
	accts, _ := store.ListAccounts()
	acct := accts[0].ID

	start := time.Now().AddDate(0, -1, 0).Format("2006-01-02")
	mk := func(in model.ProjectInput) (pid, itemID int64) {
		t.Helper()
		pid, err := store.CreateProjectInput(in)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CreateProjectItem(pid, nil, "Thing", 1000, true); err != nil {
			t.Fatal(err)
		}
		items, _ := store.ListProjectItems(pid)
		return pid, items[0].ID
	}
	loanPID, loanItem := mk(model.ProjectInput{Name: "Car", Status: "saving", FinanceMethod: "loan",
		LoanAPR: 5, LoanTermMonths: 60, LoanStartDate: start})
	savePID, saveItem := mk(model.ProjectInput{Name: "Sofa", Status: "saving", FinanceMethod: "save"})

	pay := func(pid, itemID int64) {
		t.Helper()
		r := postForm("/", url.Values{"account_id": {strconv.FormatInt(acct, 10)},
			"amount": {"1000"}, "date": {time.Now().Format("2006-01-02")}})
		r.SetPathValue("id", strconv.FormatInt(pid, 10))
		r.SetPathValue("itemID", strconv.FormatInt(itemID, 10))
		w := httptest.NewRecorder()
		c.ItemPurchase(w, r)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("pay item: status %d", w.Code)
		}
	}
	pay(loanPID, loanItem)
	pay(savePID, saveItem)

	if p, _ := store.GetProject(loanPID); p.Status == "completed" {
		t.Error("loan project with repayments still running was auto-completed")
	}
	if p, _ := store.GetProject(savePID); p.Status != "completed" {
		t.Errorf("fully paid save project status = %q want completed", p.Status)
	}
}
