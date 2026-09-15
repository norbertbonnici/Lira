package controller

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"lira/internal/model"
)

var costCategories = []string{
	"Housing", "Utilities", "Telecom", "Subscriptions",
	"Insurance", "Transport", "Groceries", "Health", "Other",
}

var costFrequencies = []string{"weekly", "monthly", "quarterly", "yearly"}

func costCategory(s string) string {
	for _, c := range costCategories {
		if c == s {
			return c
		}
	}
	return "Other"
}

func costFrequency(s string) string {
	switch s {
	case "weekly", "monthly", "quarterly", "yearly":
		return s
	}
	return "monthly"
}

func costURL(id int64) string { return "/costs/" + strconv.FormatInt(id, 10) }

// costRow is an operating cost plus its monthly EUR-normalized amount, next
// payment date, and (when linked to a transaction category) this month's actual
// spend vs the budget.
type costRow struct {
	model.OperatingCost
	MonthlyEUR float64
	NextDue    string // YYYY-MM-DD, "" when no due day set
	// budget tracking (only when CategoryID is set)
	Tracked bool
	Actual  float64 // this month's spend in the linked category (EUR)
	Pct     int     // actual / budget %
	State   string  // ok | low | over
}

// budgetAlert is a tracked budget that's at/over its limit, for the costs-page
// banner and the dashboard.
type budgetAlert struct {
	Name           string
	Pct            int
	State          string
	Actual, Budget float64
}

// upcomingPayment is one cost's next payment for the "due soon" list.
type upcomingPayment struct {
	Name      string
	Category  string
	NextDue   string
	AmountEUR float64
}

// costGroup buckets costs under a category with the active monthly EUR subtotal.
type costGroup struct {
	Category string
	Costs    []costRow
	Monthly  float64
}

// budgetSummary returns the monthly budget figures (all EUR): income, active
// operating costs, active-project savings, and what's left. Reused by the
// dashboard panel.
func (c *Controller) budgetSummary() (income, operating, projectSavings, available float64) {
	costs, _ := c.store.ListOperatingCosts()
	rates, _ := c.store.GetFXRates()
	rate := func(ccy string) float64 {
		if v, ok := rates[strings.ToUpper(ccy)]; ok && v > 0 {
			return v
		}
		return 1
	}
	for _, oc := range costs {
		if oc.Active {
			operating += model.MonthlyAmount(oc.Amount, oc.Frequency) * rate(oc.Currency)
		}
	}
	if hh, _ := c.store.GetHousehold(); hh != nil {
		income = hh.MonthlyIncome
	}
	active, _ := c.store.ListActiveProjects()
	for _, card := range c.projectCards(active, time.Now()) {
		projectSavings += card.Plan.MonthlyToSave
	}
	return income, operating, projectSavings, income - operating - projectSavings
}

// monthRange returns the first and last day (YYYY-MM-DD) of now's month for
// inclusive date filters.
func monthRange(now time.Time) (from, to string) {
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1)
	return first.Format("2006-01-02"), last.Format("2006-01-02")
}

// tracksBudget reports whether a cost's category spend can be sensibly compared
// against its monthly-normalized amount. Only frequencies that accrue within a
// month (weekly, monthly) compare fairly; a quarterly/yearly payment lands as a
// lump in a single month and would always read as far over budget.
func tracksBudget(oc model.OperatingCost) bool {
	return oc.CategoryID.Valid && (oc.Frequency == "weekly" || oc.Frequency == "monthly")
}

// budgetAlerts returns this month's tracked budgets that are running low (≥90%)
// or over, plus the counts — for the costs-page banner and the dashboard.
func (c *Controller) budgetAlerts(now time.Time) (over, low int, alerts []budgetAlert) {
	costs, _ := c.store.ListOperatingCosts()
	rates, _ := c.store.GetFXRates()
	rate := func(ccy string) float64 {
		if v, ok := rates[strings.ToUpper(ccy)]; ok && v > 0 {
			return v
		}
		return 1
	}
	n, _ := c.store.CountSpendingAccounts()
	from, to := monthRange(now)
	actuals, _ := c.store.CategorySpend(from, to, n > 0)
	for _, oc := range costs {
		if !oc.Active || !tracksBudget(oc) {
			continue
		}
		budget := model.MonthlyAmount(oc.Amount, oc.Frequency) * rate(oc.Currency)
		actual := actuals[oc.CategoryID.Int64]
		pct, state := model.BudgetStatus(actual, budget)
		switch state {
		case "over":
			over++
		case "low":
			low++
		default:
			continue
		}
		alerts = append(alerts, budgetAlert{Name: oc.Name, Pct: pct, State: state, Actual: actual, Budget: budget})
	}
	return over, low, alerts
}

// billsDue returns the count and EUR total of active operating-cost payments due
// within the next `days` days (for the dashboard "bills due" banner).
func (c *Controller) billsDue(days int) (count int, total float64) {
	costs, _ := c.store.ListOperatingCosts()
	rates, _ := c.store.GetFXRates()
	rate := func(ccy string) float64 {
		if v, ok := rates[strings.ToUpper(ccy)]; ok && v > 0 {
			return v
		}
		return 1
	}
	now := time.Now()
	cut := now.AddDate(0, 0, days).Format("2006-01-02")
	for _, oc := range costs {
		if !oc.Active {
			continue
		}
		if d, ok := model.NextDueDate(oc.DueDay, now); ok && d <= cut {
			count++
			total += oc.Amount * rate(oc.Currency)
		}
	}
	return count, total
}

func (c *Controller) Costs(w http.ResponseWriter, r *http.Request) {
	costs, _ := c.store.ListOperatingCosts()
	rates, _ := c.store.GetFXRates()
	rate := func(ccy string) float64 {
		if v, ok := rates[strings.ToUpper(ccy)]; ok && v > 0 {
			return v
		}
		return 1
	}

	now := time.Now()
	// This month's actuals per category, scoped to spending accounts when any are
	// flagged (else all accounts), for budget-vs-actual on each tracked cost.
	spendingCount, _ := c.store.CountSpendingAccounts()
	mFrom, mTo := monthRange(now)
	actuals, _ := c.store.CategorySpend(mFrom, mTo, spendingCount > 0)

	byCat := map[string][]costRow{}
	var monthlyTotal float64
	var upcoming []upcomingPayment
	var dueSoon float64
	var overCount, lowCount int
	var alerts []budgetAlert
	cutoff := now.AddDate(0, 0, 30).Format("2006-01-02")
	for _, oc := range costs {
		m := model.MonthlyAmount(oc.Amount, oc.Frequency) * rate(oc.Currency)
		nextDue, hasDue := model.NextDueDate(oc.DueDay, now)
		row := costRow{OperatingCost: oc, MonthlyEUR: m, NextDue: nextDue}
		if tracksBudget(oc) {
			row.Tracked = true
			row.Actual = actuals[oc.CategoryID.Int64]
			row.Pct, row.State = model.BudgetStatus(row.Actual, m)
			if oc.Active && (row.State == "over" || row.State == "low") {
				alerts = append(alerts, budgetAlert{Name: oc.Name, Pct: row.Pct, State: row.State, Actual: row.Actual, Budget: m})
				if row.State == "over" {
					overCount++
				} else {
					lowCount++
				}
			}
		}
		byCat[oc.Category] = append(byCat[oc.Category], row)
		if oc.Active {
			monthlyTotal += m
			if hasDue {
				amt := oc.Amount * rate(oc.Currency)
				upcoming = append(upcoming, upcomingPayment{oc.Name, oc.Category, nextDue, amt})
				if nextDue <= cutoff {
					dueSoon += amt
				}
			}
		}
	}
	sort.Slice(upcoming, func(i, j int) bool { return upcoming[i].NextDue < upcoming[j].NextDue })
	var groups []costGroup
	seen := map[string]bool{}
	addGroup := func(cat string) {
		rows := byCat[cat]
		if len(rows) == 0 {
			return
		}
		var sub float64
		for _, row := range rows {
			if row.Active {
				sub += row.MonthlyEUR
			}
		}
		groups = append(groups, costGroup{Category: cat, Costs: rows, Monthly: sub})
		seen[cat] = true
	}
	for _, cat := range costCategories {
		addGroup(cat)
	}
	for cat := range byCat { // any stray categories not in the canonical list
		if !seen[cat] {
			addGroup(cat)
		}
	}

	hh, _ := c.store.GetHousehold()
	var income float64
	if hh != nil {
		income = hh.MonthlyIncome
	}

	// Project savings = Σ active projects' aggregate monthly target.
	active, _ := c.store.ListActiveProjects()
	var projectSavings float64
	for _, card := range c.projectCards(active, now) {
		projectSavings += card.Plan.MonthlyToSave
	}

	// This month's actuals (from the ledger, transfers excluded) vs the plan.
	monthly, _ := c.store.MonthlyTotals()
	cur := monthly[now.Format("2006-01")]

	c.render(w, r, "costs.html", map[string]any{
		"Groups":         groups,
		"Household":      hh,
		"Income":         income,
		"MonthlyTotal":   monthlyTotal,
		"AnnualTotal":    monthlyTotal * 12,
		"ProjectSavings": projectSavings,
		"Available":      income - monthlyTotal - projectSavings,
		"Upcoming":       upcoming,
		"DueSoon":        dueSoon,
		// per-budget actuals + alerts (this month)
		"Alerts":        alerts,
		"OverCount":     overCount,
		"LowCount":      lowCount,
		"SpendingScope": spendingCount > 0,
		// plan vs actual (this month)
		"CurMonth":      now.Format("January 2006"),
		"ActualIncome":  cur.Income,
		"ActualSpend":   cur.Expense,
		"ActualNet":     cur.Income - cur.Expense,
		"PlanNet":       income - monthlyTotal,
		"SpendVariance": cur.Expense - monthlyTotal,
		"HasActuals":    cur.Income > 0 || cur.Expense > 0,
	})
}

func (c *Controller) costForm(action, heading string, cost *model.OperatingCost, errMsg string) map[string]any {
	txCats, _ := c.store.ListCategories()
	return map[string]any{
		"Cost": cost, "Action": action, "Heading": heading,
		"Categories": costCategories, "Frequencies": costFrequencies,
		"TxCategories": txCats, "Error": errMsg,
	}
}

func (c *Controller) CostNew(w http.ResponseWriter, r *http.Request) {
	c.render(w, r, "cost_form.html", c.costForm("/costs", "New cost",
		&model.OperatingCost{Currency: "EUR", Frequency: "monthly", Category: "Other", Active: true}, ""))
}

func costFromForm(r *http.Request) *model.OperatingCost {
	ccy := strings.ToUpper(strings.TrimSpace(r.FormValue("currency")))
	if ccy == "" {
		ccy = "EUR"
	}
	dueDay := int(parseFloat(r.FormValue("due_day")))
	if dueDay < 0 {
		dueDay = 0
	} else if dueDay > 31 {
		dueDay = 31
	}
	return &model.OperatingCost{
		Name:       strings.TrimSpace(r.FormValue("name")),
		Category:   costCategory(r.FormValue("category")),
		Amount:     parseFloat(r.FormValue("amount")),
		Currency:   ccy,
		Frequency:  costFrequency(r.FormValue("frequency")),
		DueDay:     dueDay,
		CategoryID: model.NullID(nullableID(r.FormValue("category_id"))),
		Active:     r.FormValue("active") != "",
	}
}

func (c *Controller) CostCreate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	oc := costFromForm(r)
	if oc.Name == "" {
		c.render(w, r, "cost_form.html", c.costForm("/costs", "New cost", oc, "Name is required."))
		return
	}
	if _, ok := formMoney(r.FormValue("amount")); !ok {
		c.render(w, r, "cost_form.html", c.costForm("/costs", "New cost", oc, "Enter a valid amount."))
		return
	}
	if err := c.store.CreateOperatingCost(oc.Name, oc.Category, oc.Amount, oc.Currency, oc.Frequency, oc.DueDay, nullableID(r.FormValue("category_id")), oc.Active); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/costs", http.StatusSeeOther)
}

func (c *Controller) CostEdit(w http.ResponseWriter, r *http.Request) {
	oc, err := c.store.GetOperatingCost(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c.render(w, r, "cost_form.html", c.costForm(costURL(oc.ID), "Edit cost", oc, ""))
}

func (c *Controller) CostUpdate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	id := pathID(r)
	oc := costFromForm(r)
	if oc.Name == "" {
		http.Redirect(w, r, costURL(id)+"/edit", http.StatusSeeOther)
		return
	}
	if _, ok := formMoney(r.FormValue("amount")); !ok {
		http.Redirect(w, r, costURL(id)+"/edit", http.StatusSeeOther)
		return
	}
	if err := c.store.UpdateOperatingCost(id, oc.Name, oc.Category, oc.Amount, oc.Currency, oc.Frequency, oc.DueDay, nullableID(r.FormValue("category_id")), oc.Active); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/costs", http.StatusSeeOther)
}

func (c *Controller) CostToggle(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if oc, err := c.store.GetOperatingCost(pathID(r)); err == nil {
		_ = c.store.SetOperatingCostActive(oc.ID, !oc.Active)
	}
	http.Redirect(w, r, "/costs", http.StatusSeeOther)
}

func (c *Controller) CostDelete(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	_ = c.store.DeleteOperatingCost(pathID(r))
	http.Redirect(w, r, "/costs", http.StatusSeeOther)
}

func (c *Controller) IncomeUpdate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	v, ok := formMoney(r.FormValue("monthly_income"))
	if !ok { // unparseable input: keep the existing value rather than zeroing it
		http.Redirect(w, r, "/costs", http.StatusSeeOther)
		return
	}
	_ = c.store.SetMonthlyIncome(v)
	http.Redirect(w, r, "/costs", http.StatusSeeOther)
}
