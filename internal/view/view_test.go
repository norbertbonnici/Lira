package view

import (
	"database/sql"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lira/internal/model"
)

// projCard mirrors the controller's projectCard shape (Project + derived Plan)
// that the project templates range over.
type projCard struct {
	model.Project
	Plan model.ProjectPlan
}

// holdRow mirrors the controller's holdingRow (holding + EUR gain).
type holdRow struct {
	model.Holding
	GainEUR float64
	GainPct float64
	HasGain bool
}

// currencyV mirrors the controller's currencyTotal (per-currency value subtotal).
type currencyV struct {
	Currency string
	Value    float64
	ValueEUR float64
}

// fcYear mirrors the NavForecast handler's year-by-year row.
type fcYear struct {
	Year            int
	Label           string
	Planned, Actual float64
	PlannedHit      bool
	ActualHit       bool
}

// cashRowV mirrors the BurnDown handler's cash-ledger row (save inflow or spend).
type cashRowV struct {
	Kind    string
	Label   string
	When    string
	Amount  float64
	Balance float64
	Months  int
}

// costR / costGrp mirror the controller's costRow / costGroup for costs.html.
type costR struct {
	model.OperatingCost
	MonthlyEUR float64
	NextDue    string
	Tracked    bool
	Actual     float64
	Pct        int
	State      string
}

// budgetAlertV mirrors the controller's budgetAlert.
type budgetAlertV struct {
	Name           string
	Pct            int
	State          string
	Actual, Budget float64
}
type costGrp struct {
	Category string
	Costs    []costR
	Monthly  float64
}
type upcomingP struct {
	Name      string
	Category  string
	NextDue   string
	AmountEUR float64
}

// CSV-import mirror types for import_map / import_result.
type colV struct {
	Index int
	Label string
}
type layoutV struct{ Value, Label string }
type reasonV struct {
	Reason string
	Count  int
}

// catManageRow mirrors the controller's categories.html row (Category + Count).
type catManageRow struct {
	model.Category
	Count int
}

// monthRowV / catRowV mirror the controller's insights rows.
type monthRowV struct {
	Month                      string
	Income, Expense, Net, Rate float64
}
type catRowV struct {
	Category string
	Amount   float64
	Pct      int
}

// payRow mirrors the controller's itemPayment shape used by project_purchase.html.
type payRow struct {
	Date    string
	Account string
	Amount  float64
}

// projGroup mirrors the controller's phaseGroup shape used by project_detail.html.
type projGroup struct {
	Phase      *model.Phase
	Name       string
	TargetDate string
	IsGeneral  bool
	Items      []model.ProjectItem
	Plan       model.ProjectPlan
	Done       bool
}

func TestTemplatesParseAndExecute(t *testing.T) {
	v := New() // template.Must panics here on any parse error

	accts := []model.Account{{ID: 1, Name: "Example Bank", Type: "savings", Currency: "EUR", OpeningBalance: 500, Balance: 1234.56}}
	holds := []model.Holding{{ID: 1, Symbol: "VWCE", Name: "FTSE All-World", Kind: "etf", Quantity: 10, Price: 100, Currency: "EUR", Value: 1000, CostBasis: sql.NullFloat64{Float64: 900, Valid: true}}}
	txs := []model.Transaction{
		{ID: 1, Date: "2026-05-30", Amount: -12.50, Payee: "Lidl", AccountName: "Example Bank", CategoryName: "Groceries"},
		{ID: 2, Date: "2026-05-29", Amount: -250, Payee: "Transfer", AccountName: "Example Bank", TransferID: sql.NullInt64{Int64: 2, Valid: true}},
	}
	cats := []model.Category{{ID: 1, Name: "Groceries", Kind: "expense"}}
	usr := &model.User{Username: "demo_admin", Role: "admin"}
	hh := &model.Household{ID: 1, Name: "Example Household"}
	members := []model.User{{Username: "demo_admin", Role: "admin"}, {Username: "demo_member", Role: "member"}}
	invites := []model.Invite{{ID: 1, Label: "demo_member", ExpiresAt: "2026-06-06T00:00:00Z"}}
	proj := &model.Project{ID: 1, Name: "Kitchen remodel", Status: "saving", TargetDate: "2026-12-01", FinancePct: 40, BufferPct: 10, ItemsTotal: 2200, Spent: 1200, AccountID: sql.NullInt64{Int64: 1, Valid: true}}
	loanProj := &model.Project{ID: 1, Name: "Kitchen remodel", Status: "saving", FinanceMethod: "loan", LoanDeposit: 500, LoanAPR: 6, LoanTermMonths: 60, ItemsTotal: 2200, BufferPct: 10}
	pitems := []model.ProjectItem{
		{ID: 1, ProjectID: 1, Name: "Oven", EstimatedCost: 1200, AmountPaid: 1200, Essential: true, Purchased: true, ActualCost: sql.NullFloat64{Float64: 1200, Valid: true}},
		{ID: 2, ProjectID: 1, Name: "Chairs", EstimatedCost: 600, AmountPaid: 150, Essential: false}, // part-paid (deposit)
	}
	pplan := model.ProjectPlan{Target: 2420, Spent: 1200, Remaining: 1220, Financed: 968, ToSave: 1452, MonthlyToSave: 242, MonthsLeft: 6}
	// Third card: finance_method "loan" but no term (LoanTermMonths 0) — must render
	// as "save" everywhere (the templates gate on method AND term, like loanFinanced).
	loanNoTerm := &model.Project{ID: 2, Name: "Half-set loan", Status: "saving", FinanceMethod: "loan", TargetDate: "2026-12-01", FinancePct: 40, BufferPct: 10, ItemsTotal: 2200}
	// Fourth: a loan whose repayments start in a future month (LoanStart branch) and
	// whose deposit has been PAID (paid-state branches on the detail page).
	fixedNow := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	paidLoanProj := &model.Project{ID: 3, Name: "Solar", Status: "saving", FinanceMethod: "loan",
		LoanDeposit: 1500, LoanAPR: 5, LoanTermMonths: 48, LoanStartDate: "2026-09-01",
		ItemsTotal: 8000, DepositPaid: 1500, DepositPaidDate: "2026-08-20"}
	// Part-paid deposit: increments in flight (paid < planned → part-paid branches).
	partPaidLoanProj := &model.Project{ID: 4, Name: "Solar", Status: "saving", FinanceMethod: "loan",
		LoanDeposit: 1500, LoanAPR: 5, LoanTermMonths: 48,
		ItemsTotal: 8000, DepositPaid: 500, DepositPaidDate: "2026-08-01"}
	// Method switched back to "save" AFTER part-paying the deposit: the panel must
	// keep the paid history visible but disable further payments.
	switchedProj := &model.Project{ID: 5, Name: "Solar", Status: "saving", FinanceMethod: "save",
		LoanDeposit: 1500, TargetDate: "2026-12-01", FinancePct: 40, BufferPct: 10,
		ItemsTotal: 8000, DepositPaid: 500, DepositPaidDate: "2026-08-01"}
	startPlan := model.FinanceWithLoan(pplan, 1500, 5, 48, "2026-09-01", fixedNow)
	pcards := []projCard{{Project: *proj, Plan: pplan}, {Project: *loanProj, Plan: model.FinanceWithLoan(pplan, 500, 6, 60, "", time.Time{})}, {Project: *loanNoTerm, Plan: pplan}, {Project: *paidLoanProj, Plan: startPlan}, {Project: *partPaidLoanProj, Plan: model.FinanceWithLoan(pplan, 1500, 5, 48, "", time.Time{})}}
	phase := &model.Phase{ID: 5, ProjectID: 1, Name: "Cabinets", TargetDate: "2026-09-01"}
	pphases := []model.Phase{*phase}
	pgroups := []projGroup{
		{Phase: phase, Name: "Cabinets", TargetDate: "2026-09-01", Items: pitems[:1], Plan: pplan, Done: false},
		{Name: "General", TargetDate: "2026-12-01", IsGeneral: true, Items: pitems[1:], Plan: model.ProjectPlan{Target: 600}},
	}
	flatGroups := []projGroup{{Name: "General", IsGeneral: true, Items: pitems, Plan: pplan}}
	loanPlan := model.ComputeLoan(2420, 500, 6, 60)
	psched := []model.ScheduleRow{
		{Label: "Jul 2026", ByGroup: []float64{200, 100}, Total: 300},
		{Label: "Aug 2026", ByGroup: []float64{200, 0}, Total: 200},
	}
	schedGroups := []string{"Cabinets", "General"}
	chartData := ChartSeries{
		Labels: []string{"2026-05-01", "2026-05-15", "2026-05-30"},
		Cash:   []float64{1000, 1100, 1200},
		Inv:    []float64{500, 600, 800},
		Total:  []float64{1500, 1700, 2000},
	}
	snaps := []model.NavSnapshot{
		{ID: 3, TakenAt: "2026-05-30T00:00:00Z", NavEUR: 2000, CashEUR: 1200, InvEUR: 800},
		{ID: 1, TakenAt: "2026-05-01T00:00:00Z", NavEUR: 1500, CashEUR: 1000, InvEUR: 500},
	}
	costGroups := []costGrp{{Category: "Subscriptions", Monthly: 12, Costs: []costR{
		{OperatingCost: model.OperatingCost{ID: 1, Name: "Netflix", Category: "Subscriptions", Amount: 12, Currency: "EUR", Frequency: "monthly", DueDay: 15, Active: true}, MonthlyEUR: 12, NextDue: "2026-06-15"},
		{OperatingCost: model.OperatingCost{ID: 2, Name: "iCloud+", Category: "Subscriptions", Amount: 36, Currency: "USD", Frequency: "yearly", Active: false}, MonthlyEUR: 2.58},
	}}, {Category: "Food", Monthly: 400, Costs: []costR{
		{OperatingCost: model.OperatingCost{ID: 3, Name: "Groceries", Category: "Food", Amount: 400, Currency: "EUR", Frequency: "monthly", Active: true, CategoryID: sql.NullInt64{Int64: 1, Valid: true}}, MonthlyEUR: 400, Tracked: true, Actual: 505, Pct: 126, State: "over"},
	}}}
	budgetAlertsV := []budgetAlertV{{Name: "Groceries", Pct: 126, State: "over", Actual: 505, Budget: 400}}
	upcomingList := []upcomingP{{Name: "Netflix", Category: "Subscriptions", NextDue: "2026-06-15", AmountEUR: 12}}

	cases := map[string][]map[string]any{
		"auth_login.html":       {{"CSRF": "x"}, {"CSRF": "x", "Error": "bad", "Username": "n"}},
		"auth_register.html":    {{"CSRF": "x", "Open": true}, {"CSRF": "x", "Open": false}},
		"auth_2fa_setup.html":   {{"CSRF": "x", "Secret": "ABCD EFGH", "OtpURL": "otpauth://x", "Account": "demo_admin"}, {"CSRF": "x", "Done": true, "RecoveryCodes": []string{"abc-def", "ghi-jkl"}}},
		"auth_2fa_verify.html":  {{"CSRF": "x"}, {"CSRF": "x", "Error": "Invalid code."}},
		"dashboard.html":        {{"User": usr, "CSRF": "x", "NAV": 2234.56, "Cash": 1234.56, "Investments": 1000.0, "Accounts": accts, "Holdings": holds, "Transactions": txs, "Projects": pcards, "Chart": chartData, "HasHistory": true, "HasBudget": true, "BudgetIncome": 4000.0, "BudgetOperating": 1500.0, "BudgetSavings": 242.0, "BudgetAvailable": 2258.0, "BillsDueCount": 2, "BillsDueTotal": 220.0, "BudgetOver": 1, "BudgetLow": 2, "HasChange": true, "MonthlyChange": 1245.0, "MonthlyChangePct": 3.2, "CashPct": 55, "InvPct": 45, "Alloc": []DonutSeg{{Label: "Investments", Value: 1000, Color: "#14463f"}, {Label: "Cash", Value: 1234.56, Color: "#a9cfc4"}}, "HasSavings": true, "SavedThisMonth": 1204.0, "SavingsRate": 34, "HasPortfolio": true, "PortfolioGainPct": 7.4, "PortfolioValue": 1000.0, "HasNextBill": true, "NextBillName": "Electricity", "NextBillAmount": 34.99, "NextBillDays": 2, "NextBillDate": "2026-06-15", "AllocatedPct": 44, "HasSpark": true, "NetSpark": []float64{200, 350, 180, 420, 300, 1204}}, {"User": usr, "CSRF": "x", "NAV": 0.0, "Cash": 0.0, "Investments": 0.0}},
		"history.html":          {{"User": usr, "CSRF": "x", "NAV": 2000.0, "Cash": 1200.0, "Investments": 800.0, "HasHistory": true, "Chart": chartData, "Snapshots": snaps, "Count": 3, "Last": snaps[0], "Change": 500.0, "ChangePct": 33.3}, {"User": usr, "CSRF": "x", "NAV": 0.0, "Cash": 0.0, "Investments": 0.0, "HasHistory": false}},
		"accounts.html":         {{"User": usr, "CSRF": "x", "Accounts": []model.Account{accts[0], {ID: 2, Name: "Daily", Type: "checking", Currency: "EUR", Spending: true, Balance: 234.56}}, "Total": 1469.12, "NetWorthTotal": 1234.56}, {"User": usr, "CSRF": "x", "Total": 0.0, "NetWorthTotal": 0.0}},
		"account_form.html":     {{"User": usr, "CSRF": "x", "Account": &model.Account{Currency: "EUR", Type: "savings"}, "Action": "/accounts", "Heading": "New account"}, {"User": usr, "CSRF": "x", "Account": &accts[0], "Action": "/accounts/1", "Heading": "Edit account", "Error": "x"}},
		"transactions.html":     {{"User": usr, "CSRF": "x", "Transactions": txs}, {"User": usr, "CSRF": "x"}},
		"transaction_form.html": {{"User": usr, "CSRF": "x", "Accounts": accts, "Categories": cats, "Action": "/transactions", "Heading": "New transaction", "IsExpense": true, "DateVal": "2026-05-31", "AmountStr": "", "SelAccount": int64(0), "SelCategory": int64(0), "Payee": "", "Notes": ""}, {"User": usr, "CSRF": "x", "Accounts": accts, "Categories": cats, "Action": "/transactions/1", "Heading": "Edit transaction", "IsExpense": false, "DateVal": "2026-05-10", "AmountStr": "3000.00", "SelAccount": int64(1), "SelCategory": int64(1), "Payee": "Salary", "Notes": "", "Error": "x"}},
		"categories.html":       {{"User": usr, "CSRF": "x", "Rows": []catManageRow{{Category: model.Category{ID: 1, Name: "Groceries", Kind: "expense"}, Count: 7}, {Category: model.Category{ID: 2, Name: "Salary", Kind: "income"}, Count: 1}}}, {"User": usr, "CSRF": "x", "Rows": []catManageRow{}}},
		"category_form.html":    {{"User": usr, "CSRF": "x", "Category": &model.Category{ID: 1, Name: "Groceries", Kind: "expense"}}},
		"projection.html":       {{"User": usr, "CSRF": "x", "Rows": []model.ProjectionRow{{Year: 0, Contributed: 1000, Value: 1000}, {Year: 1, Contributed: 2200, Value: 2300, Growth: 100}}, "Chart": chartData, "HasChart": true, "Start": 1000.0, "Monthly": 100.0, "Return": 7.0, "Years": 25, "Target": 600000.0, "Inflation": 2.0, "FinalValue": 2300.0, "TotalContributed": 2200.0, "TotalGrowth": 100.0, "RealFinal": 2200.0, "HasInflation": true, "HasTarget": true, "FIReached": true, "FIYear": 20, "FICalendar": 2046}, {"User": usr, "CSRF": "x", "Rows": []model.ProjectionRow{}, "HasChart": false, "Start": 0.0, "Monthly": 0.0, "Return": 7.0, "Years": 25, "Target": 0.0, "Inflation": 0.0, "FinalValue": 0.0, "TotalContributed": 0.0, "TotalGrowth": 0.0, "RealFinal": 0.0, "HasInflation": false, "HasTarget": false, "FIReached": false, "FIYear": 0, "FICalendar": 2026}},
		"transfer_form.html":    {{"User": usr, "CSRF": "x", "Accounts": accts}, {"User": usr, "CSRF": "x", "Error": "bad"}, {"User": usr, "CSRF": "x"}},
		"import_upload.html":    {{"User": usr, "CSRF": "x", "Accounts": accts}, {"User": usr, "CSRF": "x", "Accounts": accts, "Error": "bad file"}, {"User": usr, "CSRF": "x"}},
		"import_map.html":       {{"User": usr, "CSRF": "x", "Columns": []colV{{0, "Date"}, {1, "Amount"}, {2, "Payee"}}, "Preview": [][]string{{"2026-01-31", "-12.50", "Lidl"}}, "CSV": "Date,Amount,Payee\n2026-01-31,-12.50,Lidl", "HasHeader": true, "AccountID": "1", "DateLayouts": []layoutV{{"2006-01-02", "YYYY-MM-DD"}}, "RowCount": 1}},
		"import_result.html":    {{"User": usr, "CSRF": "x", "Imported": 12, "Skipped": 2, "Reasons": []reasonV{{"Unparseable or missing date", 2}}}, {"User": usr, "CSRF": "x", "Imported": 1, "Skipped": 0}},
		"insights.html":         {{"User": usr, "CSRF": "x", "CurMonth": "May 2026", "CurIncome": 3000.0, "CurExpense": 1200.0, "CurNet": 1800.0, "CurRate": 60.0, "Categories": []catRowV{{Category: "Groceries", Amount: 150, Pct: 100}, {Category: "Dining", Amount: 40, Pct: 27}}, "Months": []monthRowV{{Month: "2026-05", Income: 3000, Expense: 1200, Net: 1800, Rate: 60}}}, {"User": usr, "CSRF": "x", "CurMonth": "May 2026", "CurIncome": 0.0, "CurExpense": 0.0, "CurNet": 0.0, "CurRate": 0.0, "Categories": []catRowV{}, "Months": []monthRowV{}}},
		"holdings.html":         {{"User": usr, "CSRF": "x", "Rows": []holdRow{{Holding: holds[0], GainEUR: 100, GainPct: 11.1, HasGain: true}}, "Currencies": []currencyV{{Currency: "USD", Value: 3743.00, ValueEUR: 3210.10}, {Currency: "EUR", Value: 1000, ValueEUR: 1000}}, "ShowFX": true, "Total": 4210.10, "TotalGain": 100.0, "TotalGainPct": 11.1, "HasGains": true, "Refreshable": true, "Updated": "2026-05-30"}, {"User": usr, "CSRF": "x"}},
		"holding_form.html":     {{"User": usr, "CSRF": "x", "Holding": &model.Holding{Kind: "stock", Currency: "EUR"}, "Action": "/holdings", "Heading": "New holding"}, {"User": usr, "CSRF": "x", "Holding": &holds[0], "Action": "/holdings/1", "Heading": "Edit holding"}},
		"reports.html":          {{"User": usr, "CSRF": "x"}},
		"pocket.html":           {{"User": usr, "CSRF": "x", "Balance": 70.0, "ToppedUp": 100.0, "Spent": 30.0, "Categories": []model.CategoryTotal{{Category: "Beers", Amount: 30}}, "TopCat": 30.0, "DateVal": "2026-06-01", "Entries": []model.PocketEntry{{ID: 1, Date: "2026-06-02", Amount: -30, Category: "Beers", Note: "with the boys"}, {ID: 2, Date: "2026-06-01", Amount: 100, Category: "Allowance"}}, "Tokens": []model.APIToken{{ID: 1, Name: "iPhone", CreatedAt: "2026-06-01T10:00:00Z"}}, "NewToken": "lira_xyz"}, {"User": usr, "CSRF": "x", "Balance": 0.0, "ToppedUp": 0.0, "Spent": 0.0, "TopCat": 0.0, "DateVal": "2026-06-01"}},
		"burndown.html":         {{"User": usr, "CSRF": "x", "StartCash": 100000.0, "MonthlyIn": 2000.0, "Months": 36, "Chart": BurnSeries{Labels: []string{"May 2026", "Jun 2026", "Jul 2026"}, Balance: []float64{100000, 102000, 24000}, Spend: []float64{0, 0, 80000}}, "HasProjectIn": true, "Ledger": []cashRowV{{Kind: "save", Label: "Saving", When: "Jul 2026", Amount: 4000, Balance: 104000, Months: 2}, {Kind: "save", Label: "Saving for projects", When: "Jul 2026", Amount: 1500, Balance: 105500, Months: 2}, {Kind: "spend", Label: "New car", When: "2026-07-01", Amount: 80000, Balance: 24000}}, "EndBalance": 92000.0, "Low": 24000.0, "LowDate": "Jul 2026", "GoesNegative": false, "Undated": 0, "Beyond": 0, "NoSpends": false, "NoSavings": false}, {"User": usr, "CSRF": "x", "StartCash": 1000.0, "MonthlyIn": 0.0, "Months": 36, "Chart": BurnSeries{Labels: []string{"May 2026", "Jun 2026"}, Balance: []float64{1000, -4000}, Spend: []float64{0, 5000}}, "Ledger": []cashRowV{}, "EndBalance": -4000.0, "Low": -4000.0, "LowDate": "Jun 2026", "GoesNegative": true, "Undated": 1, "Beyond": 1, "NoSpends": true, "NoSavings": true}, {"User": usr, "CSRF": "x", "StartCash": 5000.0, "MonthlyIn": 300.0, "Months": 24, "HasLoanOuts": true, "Chart": BurnSeries{Labels: []string{"Aug 2026", "Sep 2026", "Oct 2026"}, Balance: []float64{5000, 4900, 4800}, Spend: []float64{0, 0, 0}}, "Ledger": []cashRowV{{Kind: "save", Label: "Saving", When: "Oct 2026", Amount: 600, Balance: 5600, Months: 2}, {Kind: "spend", Label: "Loan payments × 2 mo", When: "to Oct 2026", Amount: 800, Balance: 4800}, {Kind: "spend", Label: "Solar · loan deposit", When: "2026-10-01", Amount: 1500, Balance: 3300}, {Kind: "save", Label: "Saving", When: "Aug 2028", Amount: 6600, Balance: 9900, Months: 22}, {Kind: "spend", Label: "Loan payments × 22 mo", When: "to Aug 2028", Amount: 8800, Balance: 1100}}, "EndBalance": 1100.0, "Low": 1100.0, "LowDate": "Aug 2028", "GoesNegative": false, "Undated": 0, "Beyond": 0, "NoSpends": false, "NoSavings": false}},
		"forecast.html":         {{"User": usr, "CSRF": "x", "StartNAV": 50000.0, "Planned": 1500.0, "Actual": 1200.0, "Target": 500000.0, "HasTarget": true, "Return": 5.0, "Months": 120, "Years": []fcYear{{Year: 0, Label: "May 2026", Planned: 50000, Actual: 50000}, {Year: 1, Label: "May 2027", Planned: 68000, Actual: 64400, PlannedHit: false}}, "Chart": ForecastSeries{Labels: []string{"May 2026", "Jun 2026", "Jul 2026"}, Planned: []float64{50000, 51500, 53000}, Actual: []float64{50000, 51200, 52400}, Target: 500000}, "PlannedETAMonths": 180, "PlannedETA": "May 2041", "ActualETAMonths": -1, "ActualETA": "", "NoSavings": false}, {"User": usr, "CSRF": "x", "StartNAV": 0.0, "Planned": 0.0, "Actual": 0.0, "Target": 0.0, "HasTarget": false, "Return": 0.0, "Months": 120, "Years": []fcYear{{Year: 0, Label: "May 2026"}}, "Chart": ForecastSeries{Labels: []string{"May 2026", "Jun 2026"}, Planned: []float64{0, 0}, Actual: []float64{0, 0}}, "PlannedETAMonths": -1, "PlannedETA": "", "ActualETAMonths": -1, "ActualETA": "", "NoSavings": true}},
		"settings.html":         {{"User": usr, "CSRF": "x", "Household": hh, "Members": members, "Invites": invites, "Available": 1800.0, "Tokens": []model.APIToken{{ID: 1, Name: "iPhone", CreatedAt: "2026-06-01T10:00:00Z", LastUsedAt: "2026-06-02T08:00:00Z"}}, "NewToken": "lira_abc123"}, {"User": usr, "CSRF": "x", "Household": hh, "Members": members, "Invites": invites, "InviteLink": "https://lira.example.com/invite/tok", "Flash": "copied", "Available": 0.0, "Tokens": []model.APIToken{}}},
		"invite.html":           {{"CSRF": "x", "Token": "tok", "Label": "demo_member"}, {"CSRF": "x", "Invalid": true}},
		"projects.html":         {{"User": usr, "CSRF": "x", "Cards": pcards, "NAV": 5000.0, "Cash": 3000.0, "Committed": 968.0, "Uncommitted": 4032.0, "UncommittedCash": 2032.0}, {"User": usr, "CSRF": "x", "NAV": 0.0, "Cash": 0.0, "Committed": 0.0, "Uncommitted": 0.0, "UncommittedCash": 0.0}},
		"project_form.html":     {{"User": usr, "CSRF": "x", "Project": &model.Project{Status: "saving"}, "Accounts": accts, "Action": "/projects", "Heading": "New project"}, {"User": usr, "CSRF": "x", "Project": proj, "Accounts": accts, "Action": "/projects/1", "Heading": "Edit project", "Error": "x"}, {"User": usr, "CSRF": "x", "Project": loanProj, "Accounts": accts, "Action": "/projects/1", "Heading": "Edit project"}},
		"project_detail.html":   {{"User": usr, "CSRF": "x", "Project": proj, "HasPhases": true, "Groups": pgroups, "Phases": pphases, "Plan": pplan, "Loan": loanPlan, "HasLoan": true, "NAV": 5000.0, "Cash": 3000.0, "AfterNAV": 4032.0, "AfterCash": 2032.0, "ShortCash": true, "Accounts": accts}, {"User": usr, "CSRF": "x", "Project": proj, "HasPhases": false, "Groups": flatGroups, "Plan": pplan, "Loan": model.LoanPlan{}, "HasLoan": false, "NAV": 5000.0, "Cash": 3000.0, "AfterNAV": 4032.0, "AfterCash": 2032.0, "ShortCash": false, "Accounts": accts}, {"User": usr, "CSRF": "x", "Project": loanProj, "HasPhases": false, "Groups": flatGroups, "Plan": model.FinanceWithLoan(pplan, 500, 6, 60, "", time.Time{}), "Loan": loanPlan, "HasLoan": true, "LoanFinanced": true, "NAV": 5000.0, "Cash": 3000.0, "AfterNAV": 4500.0, "AfterCash": 2500.0, "ShortCash": false, "Accounts": accts, "Categories": cats, "DepositPayments": []payRow{}, "DepositRemaining": 500.0}, {"User": usr, "CSRF": "x", "Project": paidLoanProj, "HasPhases": false, "Groups": flatGroups, "Plan": startPlan, "Loan": model.ComputeLoan(2420, 1500, 5, 48), "HasLoan": true, "LoanFinanced": true, "NAV": 5000.0, "Cash": 3000.0, "AfterNAV": 5000.0, "AfterCash": 3000.0, "ShortCash": false, "Accounts": accts, "Categories": cats, "DepositPayments": []payRow{{Date: "2026-08-01", Account: "Example Bank", Amount: 1000.0}, {Date: "2026-08-20", Account: "Example Bank", Amount: 500.0}}, "DepositRemaining": 0.0}, {"User": usr, "CSRF": "x", "Project": partPaidLoanProj, "HasPhases": false, "Groups": flatGroups, "Plan": startPlan, "Loan": model.ComputeLoan(2420, 1500, 5, 48), "HasLoan": true, "LoanFinanced": true, "NAV": 5000.0, "Cash": 3000.0, "AfterNAV": 4500.0, "AfterCash": 2500.0, "ShortCash": false, "Accounts": accts, "Categories": cats, "DepositPayments": []payRow{{Date: "2026-08-01", Account: "Example Bank", Amount: 500.0}}, "DepositRemaining": 1000.0}, {"User": usr, "CSRF": "x", "Project": switchedProj, "HasPhases": false, "Groups": flatGroups, "Plan": pplan, "Loan": model.LoanPlan{}, "HasLoan": false, "LoanFinanced": false, "NAV": 5000.0, "Cash": 3000.0, "AfterNAV": 4532.0, "AfterCash": 2532.0, "ShortCash": false, "Accounts": accts, "Categories": cats, "DepositPayments": []payRow{{Date: "2026-08-01", Account: "Example Bank", Amount: 500.0}}, "DepositRemaining": 1000.0}},
		"item_form.html":        {{"User": usr, "CSRF": "x", "Item": &pitems[0], "ProjectID": int64(1)}},
		"project_purchase.html": {{"User": usr, "CSRF": "x", "Item": &pitems[1], "Project": proj, "Accounts": accts, "Categories": cats, "Remaining": 450.0, "HasPaid": true, "Payments": []payRow{{Date: "2026-06-01", Account: "Example Bank", Amount: 150.0}}}, {"User": usr, "CSRF": "x", "Item": &pitems[1], "Project": proj, "Accounts": accts, "Categories": cats, "Remaining": 450.0, "Error": "bad"}},
		"phase_form.html":       {{"User": usr, "CSRF": "x", "Phase": phase, "ProjectID": int64(1)}},
		"project_plan.html":     {{"User": usr, "CSRF": "x", "Project": proj, "Groups": pgroups, "Plan": pplan, "Schedule": psched, "ScheduleGroups": schedGroups, "Peak": 300.0, "DueNow": 0.0, "FinalDate": "2026-12-01", "NAV": 5000.0, "Cash": 3000.0, "AfterNAV": 4032.0, "AfterCash": 2032.0}, {"User": usr, "CSRF": "x", "Project": proj, "Groups": pgroups, "Plan": pplan, "Schedule": []model.ScheduleRow{}, "ScheduleGroups": []string{}, "Peak": 0.0, "DueNow": 250.0, "FinalDate": "2026-12-01", "NAV": 5000.0, "Cash": 3000.0, "AfterNAV": 4032.0, "AfterCash": 2032.0}, {"User": usr, "CSRF": "x", "Project": loanProj, "Groups": pgroups, "Plan": model.FinanceWithLoan(pplan, 500, 6, 60, "", time.Time{}), "LoanFinanced": true, "Loan": loanPlan, "Schedule": []model.ScheduleRow{}, "ScheduleGroups": []string{}, "Peak": 0.0, "DueNow": 0.0, "FinalDate": "2026-12-01", "NAV": 5000.0, "Cash": 3000.0, "AfterNAV": 4500.0, "AfterCash": 2500.0}},
		"costs.html":            {{"User": usr, "CSRF": "x", "Groups": costGroups, "Income": 4000.0, "MonthlyTotal": 14.58, "AnnualTotal": 174.96, "ProjectSavings": 242.0, "Available": 3743.42, "Upcoming": upcomingList, "DueSoon": 12.0, "Alerts": budgetAlertsV, "OverCount": 1, "LowCount": 0, "SpendingScope": false, "HasActuals": true, "CurMonth": "May 2026", "ActualIncome": 3000.0, "ActualSpend": 640.0, "ActualNet": 2360.0, "PlanNet": 3985.42, "SpendVariance": 625.42}, {"User": usr, "CSRF": "x", "Income": 0.0, "MonthlyTotal": 0.0, "AnnualTotal": 0.0, "ProjectSavings": 0.0, "Available": 0.0}},
		"cost_form.html":        {{"User": usr, "CSRF": "x", "Cost": &model.OperatingCost{Currency: "EUR", Frequency: "monthly", Category: "Other", Active: true}, "Action": "/costs", "Heading": "New cost", "Categories": []string{"Utilities", "Subscriptions", "Other"}, "Frequencies": []string{"weekly", "monthly", "yearly"}, "TxCategories": cats}, {"User": usr, "CSRF": "x", "Cost": &model.OperatingCost{ID: 1, Name: "Netflix", Currency: "EUR", Frequency: "monthly", Category: "Subscriptions", Active: false, CategoryID: sql.NullInt64{Int64: 1, Valid: true}}, "Action": "/costs/1", "Heading": "Edit cost", "Categories": []string{"Subscriptions"}, "Frequencies": []string{"monthly"}, "TxCategories": cats, "Error": "x"}},
	}

	for name, datas := range cases {
		for i, d := range datas {
			rec := httptest.NewRecorder()
			if err := v.Render(rec, name, d); err != nil {
				t.Errorf("%s[%d]: %v", name, i, err)
			}
		}
	}
}

func TestNavChart(t *testing.T) {
	s := ChartSeries{
		Labels: []string{"2026-05-01", "2026-05-30"},
		Cash:   []float64{1000, 1200},
		Inv:    []float64{500, 800},
		Total:  []float64{1500, 2000},
	}
	out := string(navChart(s))
	for _, want := range []string{"<svg", "<polygon", "<polyline", "2026-05-30"} {
		if !strings.Contains(out, want) {
			t.Errorf("navChart output missing %q", want)
		}
	}
	if strings.Contains(out, "<script") {
		t.Error("navChart must not emit <script> (CSP)")
	}
	// Fewer than 2 points → empty (caller shows an empty state).
	if navChart(ChartSeries{}) != "" {
		t.Error("empty series should render nothing")
	}
	if navChart(ChartSeries{Labels: []string{"x"}, Cash: []float64{1}, Inv: []float64{1}, Total: []float64{2}}) != "" {
		t.Error("single point should render nothing")
	}
}

func TestMoney(t *testing.T) {
	cases := []struct {
		v    float64
		ccy  string
		want string
	}{
		{1234.5, "EUR", "€1,234.50"},
		{1234.5, "", "€1,234.50"},
		{311.78, "USD", "311.78 USD"},
		{-1000, "usd", "-1,000.00 USD"},
	}
	for _, c := range cases {
		if got := money(c.v, c.ccy); got != c.want {
			t.Errorf("money(%v,%q) = %q want %q", c.v, c.ccy, got, c.want)
		}
	}
}

func TestShortEUR(t *testing.T) {
	cases := map[float64]string{950: "€950", 1234: "€1.2k", 2500000: "€2.5M"}
	for in, want := range cases {
		if got := shortEUR(in); got != want {
			t.Errorf("shortEUR(%v) = %q want %q", in, got, want)
		}
	}
}

func TestEUR(t *testing.T) {
	cases := map[float64]string{0: "\u20ac0.00", 1234.5: "\u20ac1,234.50", -1000000: "-\u20ac1,000,000.00", 12.345: "\u20ac12.35"}
	for in, want := range cases {
		if got := eur(in); got != want {
			t.Errorf("eur(%v)=%q want %q", in, got, want)
		}
	}
}
