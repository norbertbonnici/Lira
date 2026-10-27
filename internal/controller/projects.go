package controller

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"lira/internal/model"
)

// projectCard pairs a project with its derived savings plan for display.
type projectCard struct {
	model.Project
	Plan model.ProjectPlan
}

func (c *Controller) projectCards(ps []model.Project, now time.Time) []projectCard {
	cards := make([]projectCard, 0, len(ps))
	for i := range ps {
		_, agg := c.projectGroups(&ps[i], now)
		cards = append(cards, projectCard{Project: ps[i], Plan: agg})
	}
	return cards
}

// phaseGroup is a savings bucket on the project detail page: either a phase
// (a dated milestone grouping its items) or the implicit "General" group of
// unphased items.
type phaseGroup struct {
	Phase      *model.Phase // nil for General
	Name       string
	TargetDate string
	IsGeneral  bool
	Items      []model.ProjectItem
	Plan       model.ProjectPlan
	Done       bool // has items and all are purchased
}

func allPurchased(items []model.ProjectItem) bool {
	for _, it := range items {
		if !it.Purchased {
			return false
		}
	}
	return len(items) > 0
}

// projectGroups buckets a project's items by phase, computes a savings plan per
// group (reusing model.ComputePlan), and folds them into the concurrent
// project-level aggregate (model.AggregatePhases). Unphased items form the
// "General" group, dated by the project's own target date.
func (c *Controller) projectGroups(p *model.Project, now time.Time) ([]phaseGroup, model.ProjectPlan) {
	phases, _ := c.store.ListPhases(p.ID)
	items, _ := c.store.ListProjectItems(p.ID)

	byPhase := map[int64][]model.ProjectItem{}
	var general []model.ProjectItem
	for _, it := range items {
		if it.PhaseID.Valid {
			byPhase[it.PhaseID.Int64] = append(byPhase[it.PhaseID.Int64], it)
		} else {
			general = append(general, it)
		}
	}

	groups := make([]phaseGroup, 0, len(phases)+1)
	for i := range phases {
		ph := phases[i]
		its := byPhase[ph.ID]
		groups = append(groups, phaseGroup{
			Phase: &phases[i], Name: ph.Name, TargetDate: ph.TargetDate, Items: its,
			Plan: model.ComputePlan(ph.ItemsTotal, ph.Spent, p.FinancePct, p.BufferPct, ph.TargetDate, now),
			Done: allPurchased(its),
		})
	}
	// General group: shown when there are unphased items, or when the project has
	// no phases at all (so it renders as today's flat layout).
	if len(general) > 0 || len(phases) == 0 {
		var total, spent float64
		for _, it := range general {
			total += it.EstimatedCost
			spent += it.AmountPaid // counts deposits + balance, not just completed items
		}
		groups = append(groups, phaseGroup{
			Name: "General", TargetDate: p.TargetDate, IsGeneral: true, Items: general,
			Plan: model.ComputePlan(total, spent, p.FinancePct, p.BufferPct, p.TargetDate, now),
			Done: allPurchased(general),
		})
	}

	plans := make([]model.ProjectPlan, len(groups))
	done := make([]bool, len(groups))
	for i, g := range groups {
		plans[i], done[i] = g.Plan, g.Done
	}
	agg := model.AggregatePhases(plans, done)
	// A loan-financed project's monthly commitment is the amortized loan payment
	// (deposit drawn upfront), not target-date saving — so the budget summary,
	// cards, and dashboard all reflect the loan instead of a per-month save figure.
	if loanFinanced(p) {
		agg = model.FinanceWithLoan(agg, p.LoanDeposit, p.LoanAPR, p.LoanTermMonths, p.LoanStartDate, now)
	}
	// A paid deposit is real money into the project, so it counts toward the
	// displayed Spent and draws down Remaining/progress — item payments alone
	// would leave a paid deposit looking like it never happened. Applied at the
	// aggregate (project) level regardless of the current finance method, so a
	// later switch back to "save" keeps the money counted. KNOWN SKEW: per-group
	// plans stay item-derived, so for a method-switched save project the
	// burn-down's per-group lumps ignore the deposit credit (projection errs
	// pessimistic, never optimistic) — accepted rather than pushing a
	// project-level payment down into arbitrary groups.
	if p.DepositPaid > 0 {
		agg.Spent += p.DepositPaid
		agg.Remaining = agg.Target - agg.Spent
		if agg.Remaining < 0 {
			agg.Remaining = 0
		}
	}
	return groups, agg
}

// stillToDraw is how much of a plan's Financed figure has not actually left the
// accounts yet. Payments are real transactions that already reduced NAV, so
// projections must not subtract them a second time:
//   - loan-financed: Financed is the deposit, so credit the deposit paid (item
//     payments are funded by the loan, not the deposit);
//   - save-financed: payments draw the financed share first (the money that
//     exists today — see model.ProjectPlan.ToSave), so credit everything paid:
//     plan.Spent, which for projectGroups' aggregate includes item payments AND
//     any deposit paid before a switch back from "loan" (so that money can't
//     count twice either).
func stillToDraw(p *model.Project, plan model.ProjectPlan) float64 {
	paid := p.DepositPaid
	if !loanFinanced(p) {
		paid = plan.Spent
	}
	f := plan.Financed - paid
	if f < 0 {
		f = 0
	}
	return f
}

// loanFinanced reports whether a project should be funded by a modelled loan.
func loanFinanced(p *model.Project) bool {
	return p.FinanceMethod == "loan" && p.LoanTermMonths > 0
}

// loanRepaying reports whether a loan-financed project still has repayments
// ahead of it at now (not started yet, or mid-term), using the same month
// granularity as model.FinanceWithLoan. A loan with no usable start date is
// treated as running, as FinanceWithLoan keeps charging it.
func loanRepaying(p *model.Project, now time.Time) bool {
	if !loanFinanced(p) {
		return false
	}
	t, err := time.Parse("2006-01-02", p.LoanStartDate)
	if err != nil {
		return true
	}
	diff := (t.Year()*12 + int(t.Month())) - (now.Year()*12 + int(now.Month()))
	return diff > -p.LoanTermMonths
}

// projectInput builds the editable project fields from a submitted form.
func projectInput(r *http.Request) model.ProjectInput {
	term := int(parseFloat(r.FormValue("loan_term_months")))
	if term < 0 {
		term = 0
	}
	return model.ProjectInput{
		Name:           strings.TrimSpace(r.FormValue("name")),
		TargetDate:     strings.TrimSpace(r.FormValue("target_date")),
		Status:         projectStatus(r.FormValue("status")),
		FinancePct:     parseFloat(r.FormValue("finance_pct")),
		BufferPct:      parseFloat(r.FormValue("buffer_pct")),
		AccountID:      nullableID(r.FormValue("account_id")),
		FinanceMethod:  r.FormValue("finance_method"),
		LoanDeposit:    parseFloat(r.FormValue("loan_deposit")),
		LoanAPR:        parseFloat(r.FormValue("loan_apr")),
		LoanTermMonths: term,
		LoanStartDate:  strings.TrimSpace(r.FormValue("loan_start_date")),
	}
}

func projectStatus(s string) string {
	switch s {
	case "planning", "saving", "completed", "archived":
		return s
	}
	return "saving"
}

func pathInt(r *http.Request, name string) int64 {
	id, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id
}

func projectURL(id int64) string { return "/projects/" + strconv.FormatInt(id, 10) }

// ---------- Projects ----------

func (c *Controller) Projects(w http.ResponseWriter, r *http.Request) {
	projects, _ := c.store.ListProjects()
	cash, _, nav, err := c.store.ComputeNAV()
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	cards := c.projectCards(projects, time.Now())

	var committed float64
	for i := range cards {
		if cards[i].Status == "planning" || cards[i].Status == "saving" {
			// A paid loan deposit already left the accounts — count only what's
			// still to be drawn, so Uncommitted doesn't subtract it twice.
			committed += stillToDraw(&cards[i].Project, cards[i].Plan)
		}
	}
	c.render(w, r, "projects.html", map[string]any{
		"Cards":           cards,
		"NAV":             nav,
		"Cash":            cash,
		"Committed":       committed,
		"Uncommitted":     nav - committed,
		"UncommittedCash": cash - committed,
	})
}

func (c *Controller) ProjectNew(w http.ResponseWriter, r *http.Request) {
	accts, _ := c.store.ListAccounts()
	c.render(w, r, "project_form.html", map[string]any{
		"Project":  &model.Project{Status: "saving", FinanceMethod: "save"},
		"Accounts": accts,
		"Action":   "/projects",
		"Heading":  "New project",
	})
}

// projectFromInput rebuilds a Project for re-rendering the form on a validation
// error, preserving everything the user typed (including the loan inputs).
func projectFromInput(in model.ProjectInput) *model.Project {
	return &model.Project{
		Name: in.Name, TargetDate: in.TargetDate, Status: in.Status,
		FinancePct: in.FinancePct, BufferPct: in.BufferPct,
		FinanceMethod: in.FinanceMethod, LoanDeposit: in.LoanDeposit,
		LoanAPR: in.LoanAPR, LoanTermMonths: in.LoanTermMonths,
		LoanStartDate: in.LoanStartDate,
	}
}

// validateProject returns a user-facing error message for invalid input, or ""
// if the input is acceptable. Loan financing requires a repayment term — without
// one there's nothing to amortize, and the project would silently fall back to
// saving (see loanFinanced), so we reject it up front.
func validateProject(in model.ProjectInput) string {
	if in.Name == "" {
		return "Name is required."
	}
	if in.FinanceMethod == "loan" && in.LoanTermMonths <= 0 {
		return "A loan term (months) is required when financing with a loan."
	}
	if in.FinancePct < 0 || in.FinancePct > 100 {
		return "Finance from savings must be between 0 and 100%."
	}
	if in.BufferPct < 0 {
		return "Contingency buffer can't be negative."
	}
	if in.LoanDeposit < 0 || in.LoanAPR < 0 {
		return "Loan deposit and APR can't be negative."
	}
	if in.LoanStartDate != "" {
		if _, err := time.Parse("2006-01-02", in.LoanStartDate); err != nil {
			return "Loan start date must be a valid date (YYYY-MM-DD)."
		}
	}
	return ""
}

func (c *Controller) ProjectCreate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	in := projectInput(r)
	if msg := validateProject(in); msg != "" {
		accts, _ := c.store.ListAccounts()
		c.render(w, r, "project_form.html", map[string]any{
			"Project": projectFromInput(in), "Accounts": accts,
			"Action": "/projects", "Heading": "New project", "Error": msg,
		})
		return
	}
	id, err := c.store.CreateProjectInput(in)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, projectURL(id), http.StatusSeeOther)
}

func (c *Controller) ProjectView(w http.ResponseWriter, r *http.Request) {
	p, err := c.store.GetProject(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	groups, plan := c.projectGroups(p, time.Now())
	accts, _ := c.store.ListAccounts()
	cats, _ := c.store.ListCategories()
	phases, _ := c.store.ListPhases(p.ID)
	cash, _, nav, err := c.store.ComputeNAV()
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	loan := model.ComputeLoan(plan.Target, p.LoanDeposit, p.LoanAPR, p.LoanTermMonths)
	draw := stillToDraw(p, plan) // financing not yet taken out of the accounts
	// Deposit payment history + what's left of the planned deposit (increments).
	rawDeps, _ := c.store.ListDepositPayments(p.ID)
	depPays := make([]itemPayment, 0, len(rawDeps))
	for _, t := range rawDeps {
		depPays = append(depPays, itemPayment{Date: t.Date, Account: t.AccountName, Amount: -t.Amount})
	}
	// Sub-half-cent residue counts as fully paid — mirrors DepositPay's 0.005
	// gate, so the templates can't show a payment form the handler would refuse.
	depRemaining := p.LoanDeposit - p.DepositPaid
	if depRemaining <= 0.005 {
		depRemaining = 0
	}
	c.render(w, r, "project_detail.html", map[string]any{
		"Project":          p,
		"Groups":           groups,
		"Phases":           phases, // for the per-item "move to phase" control
		"HasPhases":        len(phases) > 0,
		"Plan":             plan,
		"Loan":             loan,
		"HasLoan":          loan.TermMonths > 0 && loan.Principal > 0,
		"LoanFinanced":     loanFinanced(p),
		"NAV":              nav,
		"Cash":             cash,
		"AfterNAV":         nav - draw,
		"AfterCash":        cash - draw,
		"ShortCash":        draw > cash, // financing exceeds liquid savings
		"Accounts":         accts,
		"Categories":       cats, // for the pay-deposit form
		"DepositPayments":  depPays,
		"DepositRemaining": depRemaining,
	})
}

// LoanUpdate stores the loan-financing inputs (deposit, APR, term) for a project
// so the detail page can model the monthly payment and total interest.
func (c *Controller) LoanUpdate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	id := pathID(r)
	p, err := c.store.GetProject(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	term := int(parseFloat(r.FormValue("loan_term_months")))
	if term < 0 {
		term = 0
	}
	// A loan-financed project always has a term (validateProject enforces it on
	// the edit form); a blank or zero term here would silently turn it into a
	// target-date saving plan, so keep the stored one.
	if term == 0 && p.FinanceMethod == "loan" {
		term = p.LoanTermMonths
	}
	start := strings.TrimSpace(r.FormValue("loan_start_date"))
	if start != "" {
		if _, err := time.Parse("2006-01-02", start); err != nil {
			start = p.LoanStartDate // unparseable input must not wipe a stored date
		}
	}
	deposit := max(parseFloat(r.FormValue("loan_deposit")), 0)
	apr := max(parseFloat(r.FormValue("loan_apr")), 0)
	_ = c.store.UpdateProjectLoan(id, deposit, apr, term, start)
	http.Redirect(w, r, projectURL(id), http.StatusSeeOther)
}

// DepositPay records the loan deposit as a real expense transaction (linked to
// the project), so balances and net worth reflect the cash actually handed over.
func (c *Controller) DepositPay(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	id := pathID(r)
	p, err := c.store.GetProject(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	acctID, _ := strconv.ParseInt(r.FormValue("account_id"), 10, 64)
	amount, ok := formMoney(r.FormValue("amount"))
	amount = absf(amount)
	date := strings.TrimSpace(r.FormValue("date"))
	// Deposits are paid in increments: accept while the planned deposit isn't
	// covered yet. On bad input just bounce back (the form lives on the detail page).
	remaining := p.LoanDeposit - p.DepositPaid
	if !loanFinanced(p) || remaining <= 0.005 || acctID == 0 || !ok || amount == 0 || date == "" {
		http.Redirect(w, r, projectURL(id), http.StatusSeeOther)
		return
	}
	if err := c.store.PayLoanDeposit(id, acctID, date, amount, nullableID(r.FormValue("category_id"))); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, projectURL(id), http.StatusSeeOther)
}

// ProjectPlan is the read-only "saving plan" view: all phases, a month-by-month
// schedule, and summary statistics.
func (c *Controller) ProjectPlan(w http.ResponseWriter, r *http.Request) {
	p, err := c.store.GetProject(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	now := time.Now()
	groups, agg := c.projectGroups(p, now)
	cash, _, nav, err := c.store.ComputeNAV()
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	// Collect the active savers (still contributing a monthly amount) for the grid,
	// plus the overdue "due now" total and the latest target date.
	var names []string
	var monthly []float64
	var monthsLeft []int
	var dueNow float64
	finalDate := p.TargetDate
	for _, g := range groups {
		if g.Plan.Overdue && !g.Done {
			dueNow += g.Plan.ToSave
		}
		if !g.Done && g.Plan.MonthsLeft > 0 && g.Plan.MonthlyToSave > 0 {
			names = append(names, g.Name)
			monthly = append(monthly, g.Plan.MonthlyToSave)
			monthsLeft = append(monthsLeft, g.Plan.MonthsLeft)
		}
		if g.TargetDate > finalDate {
			finalDate = g.TargetDate
		}
	}
	schedule := model.BuildSaveSchedule(monthly, monthsLeft, now)
	var peak float64
	if len(schedule) > 0 {
		peak = schedule[0].Total // front-loaded: month 1 has every active group
	}

	draw := stillToDraw(p, agg) // a paid deposit already reduced NAV
	c.render(w, r, "project_plan.html", map[string]any{
		"Project":        p,
		"Groups":         groups,
		"Plan":           agg,
		"Schedule":       schedule,
		"ScheduleGroups": names,
		"Peak":           peak,
		"DueNow":         dueNow,
		"FinalDate":      finalDate,
		"LoanFinanced":   loanFinanced(p),
		"Loan":           model.ComputeLoan(agg.Target, p.LoanDeposit, p.LoanAPR, p.LoanTermMonths),
		"NAV":            nav,
		"Cash":           cash,
		"AfterNAV":       nav - draw,
		"AfterCash":      cash - draw,
	})
}

func (c *Controller) ProjectEdit(w http.ResponseWriter, r *http.Request) {
	p, err := c.store.GetProject(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	accts, _ := c.store.ListAccounts()
	c.render(w, r, "project_form.html", map[string]any{
		"Project":  p,
		"Accounts": accts,
		"Action":   projectURL(p.ID),
		"Heading":  "Edit project",
	})
}

func (c *Controller) ProjectUpdate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	id := pathID(r)
	in := projectInput(r)
	if msg := validateProject(in); msg != "" {
		accts, _ := c.store.ListAccounts()
		p := projectFromInput(in)
		p.ID = id
		c.render(w, r, "project_form.html", map[string]any{
			"Project": p, "Accounts": accts,
			"Action": projectURL(id), "Heading": "Edit project", "Error": msg,
		})
		return
	}
	if err := c.store.UpdateProject(id, in); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, projectURL(id), http.StatusSeeOther)
}

func (c *Controller) ProjectStatus(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	id := pathID(r)
	_ = c.store.SetProjectStatus(id, projectStatus(r.FormValue("status")))
	http.Redirect(w, r, projectURL(id), http.StatusSeeOther)
}

func (c *Controller) ProjectDelete(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	_ = c.store.DeleteProject(pathID(r))
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}

// ---------- Project items ----------

func (c *Controller) ItemCreate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	pid := pathID(r)
	// Like a blank name, a negative estimate is ignored (it would shrink the budget).
	if name := strings.TrimSpace(r.FormValue("name")); name != "" {
		if est := parseFloat(r.FormValue("estimated_cost")); est >= 0 {
			_ = c.store.CreateProjectItem(pid, nullableID(r.FormValue("phase_id")), name,
				est, r.FormValue("essential") != "0")
		}
	}
	http.Redirect(w, r, projectURL(pid), http.StatusSeeOther)
}

// ---------- Project phases ----------

func (c *Controller) PhaseCreate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	pid := pathID(r)
	if name := strings.TrimSpace(r.FormValue("name")); name != "" {
		_ = c.store.CreatePhase(pid, name, strings.TrimSpace(r.FormValue("target_date")))
	}
	http.Redirect(w, r, projectURL(pid), http.StatusSeeOther)
}

func (c *Controller) PhaseEdit(w http.ResponseWriter, r *http.Request) {
	ph, err := c.store.GetPhase(pathInt(r, "phaseID"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c.render(w, r, "phase_form.html", map[string]any{
		"Phase":     ph,
		"ProjectID": pathID(r),
	})
}

func (c *Controller) PhaseUpdate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	pid := pathID(r)
	if name := strings.TrimSpace(r.FormValue("name")); name != "" {
		_ = c.store.UpdatePhase(pathInt(r, "phaseID"), name, strings.TrimSpace(r.FormValue("target_date")))
	}
	http.Redirect(w, r, projectURL(pid), http.StatusSeeOther)
}

func (c *Controller) PhaseDelete(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	_ = c.store.DeletePhase(pathInt(r, "phaseID"))
	http.Redirect(w, r, projectURL(pathID(r)), http.StatusSeeOther)
}

func (c *Controller) ItemEdit(w http.ResponseWriter, r *http.Request) {
	it, err := c.store.GetProjectItem(pathInt(r, "itemID"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c.render(w, r, "item_form.html", map[string]any{
		"Item":      it,
		"ProjectID": pathID(r),
	})
}

func (c *Controller) ItemUpdate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	pid := pathID(r)
	// Like a blank name, a negative estimate is ignored (it would shrink the budget).
	if name := strings.TrimSpace(r.FormValue("name")); name != "" {
		if est := parseFloat(r.FormValue("estimated_cost")); est >= 0 {
			_ = c.store.UpdateProjectItem(pathInt(r, "itemID"), name, est, r.FormValue("essential") != "0")
		}
	}
	http.Redirect(w, r, projectURL(pid), http.StatusSeeOther)
}

func (c *Controller) ItemDelete(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	_ = c.store.DeleteProjectItem(pathInt(r, "itemID"))
	http.Redirect(w, r, projectURL(pathID(r)), http.StatusSeeOther)
}

// ItemMove reassigns an item to another phase (or back to General).
func (c *Controller) ItemMove(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	pid := pathID(r)
	// Item must belong to this project.
	it, err := c.store.GetProjectItem(pathInt(r, "itemID"))
	if err != nil || it.ProjectID != pid {
		http.Redirect(w, r, projectURL(pid), http.StatusSeeOther)
		return
	}
	target := nullableID(r.FormValue("phase_id"))
	// A named target phase must belong to this same project.
	if target != nil {
		if ph, err := c.store.GetPhase(*target); err != nil || ph.ProjectID != pid {
			http.Redirect(w, r, projectURL(pid), http.StatusSeeOther)
			return
		}
	}
	_ = c.store.SetItemPhase(it.ID, target)
	http.Redirect(w, r, projectURL(pid), http.StatusSeeOther)
}

// itemPayment is one prior payment, shown as a positive amount on the pay form
// (the underlying transaction stores it as a negative expense).
type itemPayment struct {
	Date    string
	Account string
	Amount  float64
}

// payFormData builds the pay-form context (item, remaining, payment history).
func (c *Controller) payFormData(pid, itemID int64, errMsg string) map[string]any {
	it, _ := c.store.GetProjectItem(itemID)
	p, _ := c.store.GetProject(pid)
	accts, _ := c.store.ListAccounts()
	cats, _ := c.store.ListCategories()
	raw, _ := c.store.ListItemPayments(itemID)
	pays := make([]itemPayment, 0, len(raw))
	for _, t := range raw {
		pays = append(pays, itemPayment{Date: t.Date, Account: t.AccountName, Amount: -t.Amount})
	}
	data := map[string]any{
		"Item": it, "Project": p, "Accounts": accts, "Categories": cats,
		"Payments": pays, "Error": errMsg,
	}
	if it != nil {
		data["Remaining"] = it.Remaining()
		data["HasPaid"] = it.AmountPaid > 0
	}
	return data
}

func (c *Controller) ItemPurchaseForm(w http.ResponseWriter, r *http.Request) {
	pid, itemID := pathID(r), pathInt(r, "itemID")
	it, err := c.store.GetProjectItem(itemID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if it.Purchased { // already fully paid — nothing left to pay
		http.Redirect(w, r, projectURL(pid), http.StatusSeeOther)
		return
	}
	c.render(w, r, "project_purchase.html", c.payFormData(pid, itemID, ""))
}

func (c *Controller) ItemPurchase(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	pid := pathID(r)
	itemID := pathInt(r, "itemID")
	if _, err := c.store.GetProjectItem(itemID); err != nil { // item gone since the form loaded
		http.NotFound(w, r)
		return
	}
	acctID, _ := strconv.ParseInt(r.FormValue("account_id"), 10, 64)
	amount, ok := formMoney(r.FormValue("amount"))
	amount = absf(amount)
	date := strings.TrimSpace(r.FormValue("date"))
	final := r.FormValue("final") != ""
	if acctID == 0 || !ok || amount == 0 || date == "" {
		c.render(w, r, "project_purchase.html",
			c.payFormData(pid, itemID, "Pick an account, a non-zero amount and a date."))
		return
	}
	if err := c.store.PayItem(itemID, acctID, date, amount, nullableID(r.FormValue("category_id")), final); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	// Auto-complete the project once every item is fully paid — unless it is
	// loan-financed with repayments still to come: a completed project leaves the
	// budget, which would silently drop the monthly loan payment from it.
	if n, err := c.store.CountUnpurchasedItems(pid); err == nil && n == 0 {
		if p, err := c.store.GetProject(pid); err == nil && !loanRepaying(p, time.Now()) {
			_ = c.store.SetProjectStatus(pid, "completed")
		}
	}
	http.Redirect(w, r, projectURL(pid), http.StatusSeeOther)
}
