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
		{"finance pct 100 ok", model.ProjectInput{Name: "K", FinanceMethod: "save", FinancePct: 100}, false},
		{"finance pct over 100", model.ProjectInput{Name: "K", FinanceMethod: "save", FinancePct: 150}, true},
		{"finance pct negative", model.ProjectInput{Name: "K", FinanceMethod: "save", FinancePct: -50}, true},
		{"negative buffer", model.ProjectInput{Name: "K", FinanceMethod: "save", BufferPct: -10}, true},
		{"negative deposit", model.ProjectInput{Name: "S", FinanceMethod: "loan", LoanTermMonths: 60, LoanDeposit: -1}, true},
		{"negative apr", model.ProjectInput{Name: "S", FinanceMethod: "loan", LoanTermMonths: 60, LoanAPR: -2}, true},
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
	c, store := testController(t)
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

// The loan calculator can't blank out a loan project's term (which would
// silently turn it into a target-date saving plan), and negative deposit/APR
// are floored at 0.
func TestLoanUpdateKeepsTermOfLoanProject(t *testing.T) {
	c, store := testController(t)
	pid, _ := store.CreateProjectInput(model.ProjectInput{Name: "Car", Status: "saving",
		FinanceMethod: "loan", LoanDeposit: 2000, LoanAPR: 5, LoanTermMonths: 60})
	r := postForm("/", url.Values{"loan_term_months": {""}, "loan_deposit": {"-500"}, "loan_apr": {"-3"}})
	r.SetPathValue("id", strconv.FormatInt(pid, 10))
	c.LoanUpdate(httptest.NewRecorder(), r)
	p, _ := store.GetProject(pid)
	if p.LoanTermMonths != 60 || !loanFinanced(p) {
		t.Errorf("term = %d, loanFinanced = %v; want the stored 60 kept", p.LoanTermMonths, loanFinanced(p))
	}
	if p.LoanDeposit != 0 || p.LoanAPR != 0 {
		t.Errorf("deposit %v apr %v want both floored at 0", p.LoanDeposit, p.LoanAPR)
	}
}

// A negative item estimate is ignored on create and update (it would shrink
// the budget).
func TestItemNegativeEstimateIgnored(t *testing.T) {
	c, store := testController(t)
	pid, _ := store.CreateProjectInput(model.ProjectInput{Name: "Kitchen", Status: "saving", FinanceMethod: "save"})
	post := func(h http.HandlerFunc, form url.Values, itemID int64) {
		r := postForm("/", form)
		r.SetPathValue("id", strconv.FormatInt(pid, 10))
		r.SetPathValue("itemID", strconv.FormatInt(itemID, 10))
		h(httptest.NewRecorder(), r)
	}
	post(c.ItemCreate, url.Values{"name": {"Oven"}, "estimated_cost": {"-1000"}}, 0)
	if items, _ := store.ListProjectItems(pid); len(items) != 0 {
		t.Fatalf("negative estimate created %+v", items)
	}
	post(c.ItemCreate, url.Values{"name": {"Oven"}, "estimated_cost": {"1000"}}, 0)
	items, _ := store.ListProjectItems(pid)
	post(c.ItemUpdate, url.Values{"name": {"Oven"}, "estimated_cost": {"-1000"}}, items[0].ID)
	if it, _ := store.GetProjectItem(items[0].ID); it.EstimatedCost != 1000 {
		t.Errorf("estimate after a negative update = %v want 1000 kept", it.EstimatedCost)
	}
}

// Item and phase handlers act only on an item/phase of the project in the URL:
// pointed at another project's, they change nothing.
func TestItemAndPhaseHandlersCheckProject(t *testing.T) {
	c, store := testController(t)
	_ = store.CreateAccount("Bank", "savings", "EUR", 5000, false)
	accts, _ := store.ListAccounts()
	a, _ := store.CreateProjectInput(model.ProjectInput{Name: "A", Status: "saving", FinanceMethod: "save"})
	b, _ := store.CreateProjectInput(model.ProjectInput{Name: "B", Status: "saving", FinanceMethod: "save"})
	_ = store.CreatePhase(b, "B phase", "2027-01-01")
	phases, _ := store.ListPhases(b)
	bPhase := phases[0].ID
	_ = store.CreateProjectItem(b, nil, "B item", 100, true)
	items, _ := store.ListProjectItems(b)
	bItem := items[0].ID

	// call runs h against project A's URL, aimed at B's item/phase.
	call := func(h http.HandlerFunc, method string, form url.Values) *httptest.ResponseRecorder {
		var r *http.Request
		if method == http.MethodGet {
			r = httptest.NewRequest(http.MethodGet, "/", nil)
		} else {
			r = postForm("/", form)
		}
		r = withUser(r, &model.Session{MFAPassed: true}, &model.User{ID: 1, Username: "a", Role: "admin"})
		r.SetPathValue("id", strconv.FormatInt(a, 10))
		r.SetPathValue("itemID", strconv.FormatInt(bItem, 10))
		r.SetPathValue("phaseID", strconv.FormatInt(bPhase, 10))
		w := httptest.NewRecorder()
		h(w, r)
		return w
	}

	for name, h := range map[string]http.HandlerFunc{"ItemEdit": c.ItemEdit, "PhaseEdit": c.PhaseEdit, "ItemPurchaseForm": c.ItemPurchaseForm} {
		if w := call(h, http.MethodGet, nil); w.Code != http.StatusNotFound {
			t.Errorf("%s on another project's object: status %d want 404", name, w.Code)
		}
	}
	call(c.ItemPurchase, http.MethodPost, url.Values{"account_id": {strconv.FormatInt(accts[0].ID, 10)},
		"amount": {"100"}, "date": {"2026-10-01"}})
	call(c.ItemUpdate, http.MethodPost, url.Values{"name": {"hijacked"}, "estimated_cost": {"1"}})
	call(c.PhaseUpdate, http.MethodPost, url.Values{"name": {"hijacked"}})
	call(c.ItemCreate, http.MethodPost, url.Values{"name": {"stray"}, "estimated_cost": {"50"},
		"phase_id": {strconv.FormatInt(bPhase, 10)}})

	if it, _ := store.GetProjectItem(bItem); it.Name != "B item" || it.EstimatedCost != 100 || it.AmountPaid != 0 {
		t.Errorf("B's item changed through A's URL: %+v", it)
	}
	if pa, _ := store.GetProject(a); pa.Status == "completed" {
		t.Error("paying B's item through A's URL completed project A")
	}
	if ph, _ := store.GetPhase(bPhase); ph.Name != "B phase" {
		t.Errorf("B's phase renamed through A's URL: %q", ph.Name)
	}
	if its, _ := store.ListProjectItems(a); len(its) != 0 {
		t.Errorf("item created in A under B's phase: %+v", its)
	}

	call(c.ItemDelete, http.MethodPost, url.Values{})
	call(c.PhaseDelete, http.MethodPost, url.Values{})
	if _, err := store.GetProjectItem(bItem); err != nil {
		t.Error("B's item deleted through A's URL")
	}
	if _, err := store.GetPhase(bPhase); err != nil {
		t.Error("B's phase deleted through A's URL")
	}
}
