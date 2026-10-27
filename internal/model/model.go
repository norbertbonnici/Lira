// Package model is the data layer: domain types and the SQLite-backed Store.
// It performs no HTTP, templating, or password/crypto work beyond generating
// opaque session identifiers.
package model

import (
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

//go:embed schema.sql
var schemaSQL string

// ---------- Types ----------

type User struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         string // "admin" | "member"
	TOTPSecret   string
	TOTPEnabled  bool
	TOTPLastStep int64 // last consumed TOTP step; rejects code replay
	FailedLogins int
	LockedUntil  string // RFC3339 or ""
	CreatedAt    string
}

type Session struct {
	Token     string
	UserID    int64
	MFAPassed bool
	ExpiresAt time.Time
}

type Account struct {
	ID             int64
	Name           string
	Type           string
	Currency       string
	OpeningBalance float64
	Spending       bool    // funds operating costs; excluded from NAV + projects
	Balance        float64 // computed: opening + sum(transactions)
}

type Category struct {
	ID   int64
	Name string
	Kind string
}

type Transaction struct {
	ID         int64
	AccountID  int64
	Date       string
	Amount     float64
	CategoryID sql.NullInt64
	Payee      string
	Notes      string
	ProjectID  sql.NullInt64
	TransferID sql.NullInt64 // set on both legs of a transfer
	// view-only joined fields
	AccountName  string
	CategoryName string
}

type Holding struct {
	ID        int64
	Symbol    string
	Name      string
	Kind      string
	Quantity  float64
	Price     float64
	Currency  string
	CostBasis sql.NullFloat64
	UpdatedAt string
	Value     float64 // computed: quantity * price
}

type RecoveryCode struct {
	ID   int64
	Hash string
}

type Household struct {
	ID             int64
	Name           string
	MonthlyIncome  float64 // net household income (EUR) for the budget view
	MonthlySavings float64 // planned amount saved per month (EUR), for the NAV forecast
}

// OperatingCost is a recurring household expense (a bill, subscription, etc.).
type OperatingCost struct {
	ID         int64
	Name       string
	Category   string
	Amount     float64
	Currency   string
	Frequency  string        // weekly | monthly | quarterly | yearly
	DueDay     int           // day of month (1-31); 0 = unset
	CategoryID sql.NullInt64 // transaction category whose actuals this budget tracks
	Active     bool
	CreatedAt  string
}

// NextDueDate returns the next calendar date (YYYY-MM-DD) on or after `now` that
// falls on dueDay-of-month, clamped to the month's length (e.g. 31 → Feb 28/29).
// ok is false when dueDay is unset/invalid. Pure.
func NextDueDate(dueDay int, now time.Time) (string, bool) {
	if dueDay < 1 || dueDay > 31 {
		return "", false
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	onDay := func(year int, month time.Month) time.Time {
		last := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day() // last day of month
		d := dueDay
		if d > last {
			d = last
		}
		return time.Date(year, month, d, 0, 0, 0, 0, time.UTC)
	}
	d := onDay(today.Year(), today.Month())
	if d.Before(today) {
		ny, nm := today.Year(), today.Month()+1
		if nm > 12 {
			ny, nm = ny+1, 1
		}
		d = onDay(ny, nm)
	}
	return d.Format("2006-01-02"), true
}

// HoldingGain returns the unrealized gain (value − cost) and its percentage of
// cost. pct is 0 when cost is non-positive. Pure. cost_basis is the total amount
// paid for the position (in the holding's currency).
func HoldingGain(value, cost float64) (gain, pct float64) {
	gain = value - cost
	if cost > 0 {
		pct = gain / cost * 100
	}
	return gain, pct
}

// MonthlyAmount normalizes a recurring amount to a per-month figure. Pure.
func MonthlyAmount(amount float64, frequency string) float64 {
	switch frequency {
	case "weekly":
		return amount * 52 / 12
	case "quarterly":
		return amount / 3
	case "yearly":
		return amount / 12
	default: // monthly
		return amount
	}
}

type Invite struct {
	ID        int64
	TokenHash string
	Label     string
	CreatedBy sql.NullInt64
	CreatedAt string
	ExpiresAt string
	UsedAt    string
}

type Project struct {
	ID         int64
	Name       string
	TargetDate string // YYYY-MM-DD or ""
	Status     string // planning | saving | completed | archived
	FinancePct float64
	BufferPct  float64
	AccountID  sql.NullInt64 // default account for purchases
	// FinanceMethod is how the project is funded: "save" (spread over the months
	// to the target date) or "loan" (monthly commitment = amortized loan payment).
	FinanceMethod string
	// loan financing (optional; LoanTermMonths == 0 means no loan modelled)
	LoanDeposit    float64
	LoanAPR        float64
	LoanTermMonths int
	LoanStartDate  string // YYYY-MM-DD repayments begin; "" = immediately
	// computed
	ItemsTotal float64 // Σ estimated_cost
	Spent      float64 // Σ payments (deposits + balance) across items, derived from the ledger
	// deposit payments, derived from the ledger: DepositPaid = Σ of the tagged
	// payment transactions (clamped ≥ 0), DepositPaidDate = the latest payment's
	// date (0/"" = nothing paid yet). Deposits may be paid in several increments.
	DepositPaid     float64
	DepositPaidDate string
}

type ProjectItem struct {
	ID            int64
	ProjectID     int64
	PhaseID       sql.NullInt64 // NULL = unphased ("General")
	Name          string
	EstimatedCost float64
	ActualCost    sql.NullFloat64
	AmountPaid    float64 // Σ payments so far (deposits + balance), derived from the ledger
	Essential     bool
	Purchased     bool // fully paid / done
	TransactionID sql.NullInt64
	CreatedAt     string
}

// Remaining is the still-unpaid balance against the estimate (never negative).
func (it ProjectItem) Remaining() float64 {
	r := it.EstimatedCost - it.AmountPaid
	if r < 0 {
		return 0
	}
	return r
}

// Phase is a dated milestone that groups a project's line-items.
type Phase struct {
	ID         int64
	ProjectID  int64
	Name       string
	TargetDate string // YYYY-MM-DD or ""
	CreatedAt  string
	// computed
	ItemsTotal float64
	Spent      float64
}

// ProjectPlan is the derived savings guidance for a project.
type ProjectPlan struct {
	Target    float64 // ItemsTotal × (1 + buffer/100)
	Spent     float64
	Remaining float64 // Target − Spent
	Financed  float64 // Target × finance%/100 — drawn from existing savings
	// ToSave is what still has to be saved: the to-save share (Target − Financed)
	// less any payments beyond the financed share. Payments draw the financed
	// share first — that money exists today — so it's min(Target − Financed,
	// max(Remaining, 0)), and MonthlyToSave spreads it over the months left.
	ToSave        float64
	MonthlyToSave float64
	MonthsLeft    int
	Overdue       bool
	// LoanStart is set (YYYY-MM-DD) when a loan-financed project's repayments
	// begin in a future month — MonthlyToSave is 0 until then, and
	// UpcomingMonthly carries the payment that will start (display only,
	// never summed into the budget).
	LoanStart       string
	UpcomingMonthly float64
}

// ProjectTarget returns a project's budget: item estimates padded by the buffer %.
func ProjectTarget(itemsTotal, bufferPct float64) float64 {
	return itemsTotal * (1 + bufferPct/100)
}

// LoanPlan is the derived cost of financing a project with a loan. It is pure
// (no DB) so it can be unit-tested directly.
type LoanPlan struct {
	Deposit       float64 // upfront cash paid (capped at budget)
	Principal     float64 // amount borrowed = budget − deposit
	APR           float64 // annual interest rate (%)
	TermMonths    int
	Monthly       float64 // amortized monthly repayment
	TotalInterest float64 // interest paid over the life of the loan ("interest lost")
	TotalRepaid   float64 // Principal + TotalInterest
	TotalCost     float64 // Deposit + TotalRepaid — full outlay vs the budget
}

// ComputeLoan models a fixed-rate amortizing loan covering a project's budget
// after an upfront deposit. With apr == 0 the payment is a straight split.
// Returns a zero plan when there is nothing to borrow (deposit ≥ budget or
// term ≤ 0). Pure; safe to unit-test.
func ComputeLoan(budget, deposit, aprPct float64, termMonths int) LoanPlan {
	if deposit < 0 {
		deposit = 0
	}
	if deposit > budget {
		deposit = budget
	}
	principal := budget - deposit
	p := LoanPlan{Deposit: deposit, Principal: principal, APR: aprPct, TermMonths: termMonths}
	if principal <= 0 || termMonths <= 0 {
		p.TotalCost = deposit
		return p
	}
	n := float64(termMonths)
	i := aprPct / 100 / 12
	if i <= 0 {
		p.Monthly = principal / n
	} else {
		p.Monthly = principal * i / (1 - math.Pow(1+i, -n))
	}
	p.TotalRepaid = p.Monthly * n
	p.TotalInterest = p.TotalRepaid - principal
	p.TotalCost = deposit + p.TotalRepaid
	return p
}

// FinanceWithLoan rewrites a project's aggregate plan so its monthly commitment
// is the amortized loan payment rather than target-date saving: the deposit is
// the upfront draw from savings, and the budget less the deposit is borrowed over
// the term. Used when a project's FinanceMethod is "loan". Pure.
//
// startDate (YYYY-MM-DD, optional) is when repayments begin, at month
// granularity: before that month the plan's MonthlyToSave is 0 (nothing deducted
// from the budget yet) and LoanStart carries the date for display; mid-term,
// MonthsLeft counts down from the start; after the final payment both go to 0.
// An empty/unparseable startDate means the loan runs from now (prior behavior).
func FinanceWithLoan(plan ProjectPlan, deposit, aprPct float64, termMonths int, startDate string, now time.Time) ProjectPlan {
	loan := ComputeLoan(plan.Target, deposit, aprPct, termMonths)
	plan.Financed = loan.Deposit
	plan.ToSave = loan.Principal
	plan.MonthlyToSave = loan.Monthly
	plan.MonthsLeft = loan.TermMonths
	plan.Overdue = false
	if t, err := time.Parse("2006-01-02", startDate); err == nil && loan.TermMonths > 0 {
		// Months from the current month to the start month (negative = started).
		diff := (t.Year()*12 + int(t.Month())) - (now.Year()*12 + int(now.Month()))
		switch {
		case diff > 0: // repayments haven't started: nothing hits the budget yet
			plan.LoanStart = startDate
			plan.MonthlyToSave = 0
			plan.UpcomingMonthly = loan.Monthly // for "€X/mo from <date>" labels
		case diff <= -termMonths: // loan fully repaid
			plan.MonthlyToSave = 0
			plan.MonthsLeft = 0
		default: // mid-term: same payment, fewer months remaining
			plan.MonthsLeft = termMonths + diff
		}
	}
	return plan
}

// ComputePlan derives the savings guidance for a project. It is pure (no DB) so
// it can be unit-tested directly. monthsLeft rounds partial months up.
func ComputePlan(itemsTotal, spent, financePct, bufferPct float64, targetDate string, now time.Time) ProjectPlan {
	target := ProjectTarget(itemsTotal, bufferPct)
	// Clamp a stored out-of-range percentage (the form now rejects one): over 100
	// would finance more than the budget, below 0 would save more than it.
	financePct = min(max(financePct, 0), 100)
	financed := target * financePct / 100
	toSave := target - financed
	if rem := target - spent; rem < toSave {
		toSave = rem // already-paid money has covered part of the to-save share
	}
	if toSave < 0 {
		toSave = 0
	}
	plan := ProjectPlan{
		Target:    target,
		Spent:     spent,
		Remaining: target - spent,
		Financed:  financed,
		ToSave:    toSave,
	}

	t, err := time.Parse("2006-01-02", targetDate)
	if targetDate == "" || err != nil {
		// No usable date: show the lump sum without a monthly cadence.
		plan.MonthlyToSave = toSave
		return plan
	}
	day := func(x time.Time) time.Time {
		return time.Date(x.Year(), x.Month(), x.Day(), 0, 0, 0, 0, time.UTC)
	}
	t, now = day(t), day(now)
	if !t.After(now) {
		plan.Overdue = true
		plan.MonthlyToSave = toSave // all due now
		return plan
	}
	// Whole calendar months until the target, floored (so the monthly figure is
	// never underestimated). A clean month boundary like Jun 1 → Dec 1 is 6.
	months := (t.Year()-now.Year())*12 + int(t.Month()) - int(now.Month())
	if t.Day() < now.Day() {
		months--
	}
	if months < 1 {
		months = 1
	}
	plan.MonthsLeft = months
	plan.MonthlyToSave = toSave / float64(months)
	return plan
}

// ProjectionRow is one year of an investment growth projection.
type ProjectionRow struct {
	Year        int     // 0 = today
	Contributed float64 // cumulative money paid in (incl. the starting amount)
	Value       float64 // projected portfolio value
	Growth      float64 // Value − Contributed (returns earned)
}

// BuildProjection models monthly-compounded growth: each month the balance earns
// annualReturnPct/12 and a fixed `monthly` contribution is added. Returns one row
// per year (year 0 = the starting position). Pure.
func BuildProjection(start, monthly, annualReturnPct float64, years int) []ProjectionRow {
	if years < 0 {
		years = 0
	}
	r := annualReturnPct / 100 / 12
	value, contributed := start, start
	rows := []ProjectionRow{{Year: 0, Contributed: contributed, Value: value}}
	for y := 1; y <= years; y++ {
		for m := 0; m < 12; m++ {
			value = value*(1+r) + monthly
			contributed += monthly
		}
		rows = append(rows, ProjectionRow{Year: y, Contributed: contributed, Value: value, Growth: value - contributed})
	}
	return rows
}

// NavForecastPoint is one month of the net-worth forecast: the projected NAV at
// the planned monthly saving rate and at the actual (recent) saving rate.
type NavForecastPoint struct {
	Month   int // months from now (0 = today)
	Planned float64
	Actual  float64
}

// BuildNavForecast projects net worth forward `months` months from `start`,
// adding `planned`/`actual` to NAV each month and compounding the whole balance
// at annualReturnPct (applied monthly). It returns months+1 points (0..months).
// Pure (no DB), so it is unit-tested directly.
func BuildNavForecast(start, planned, actual, annualReturnPct float64, months int) []NavForecastPoint {
	if months < 0 {
		months = 0
	}
	r := annualReturnPct / 100 / 12
	pv, av := start, start
	pts := make([]NavForecastPoint, 0, months+1)
	pts = append(pts, NavForecastPoint{Month: 0, Planned: start, Actual: start})
	for m := 1; m <= months; m++ {
		pv = pv*(1+r) + planned
		av = av*(1+r) + actual
		pts = append(pts, NavForecastPoint{Month: m, Planned: pv, Actual: av})
	}
	return pts
}

// BurnPoint is one month of the cash burn-down: the running balance and any lump
// outflow scheduled that month.
type BurnPoint struct {
	Month   int
	Balance float64
	Spend   float64 // scheduled lump outflow this month (0 if none)
}

// BuildBurnDown projects a cash balance forward `months` months: starting at
// `start`, each month adds `monthlyIn` plus `ins[m]` (per-month inflows, e.g.
// money set aside for a project ahead of its purchase), subtracts `outs[m]`
// (recurring per-month outflows, e.g. upcoming loan repayments — drawn from the
// balance but NOT marked as spend dots), and subtracts any scheduled lump in
// `events` (marked as that month's Spend). All maps are keyed by 1-based month
// number. Month 0 is today (just the starting balance). The balance may go
// negative — that is a useful "you'd run out" signal, not an error. Pure.
func BuildBurnDown(start, monthlyIn float64, ins, outs, events map[int]float64, months int) []BurnPoint {
	if months < 0 {
		months = 0
	}
	pts := make([]BurnPoint, 0, months+1)
	bal := start
	pts = append(pts, BurnPoint{Month: 0, Balance: bal})
	for m := 1; m <= months; m++ {
		bal += monthlyIn + ins[m] - outs[m] - events[m]
		pts = append(pts, BurnPoint{Month: m, Balance: bal, Spend: events[m]})
	}
	return pts
}

// MonthsToTarget returns how many months `start` needs to reach `target` while
// growing by `monthly` (compounded monthly at annualReturnPct). It returns 0 if
// already at/above target, and -1 if it is not reached within `maxMonths` (or
// can never be — non-positive monthly and no growth). Pure.
func MonthsToTarget(start, monthly, annualReturnPct, target float64, maxMonths int) int {
	if start >= target {
		return 0
	}
	r := annualReturnPct / 100 / 12
	v := start
	for m := 1; m <= maxMonths; m++ {
		v = v*(1+r) + monthly
		if v >= target {
			return m
		}
	}
	return -1
}

// AggregatePhases folds per-phase plans into a single project-level plan using
// the concurrent model: budgets and financing sum, and the monthly figure is the
// SUM of each phase's monthly — except phases flagged done (fully purchased),
// which still count toward budget/spent but demand no further saving. Pure.
func AggregatePhases(plans []ProjectPlan, done []bool) ProjectPlan {
	var agg ProjectPlan
	for i, p := range plans {
		agg.Target += p.Target
		agg.Spent += p.Spent
		agg.Financed += p.Financed
		agg.ToSave += p.ToSave
		if p.MonthsLeft > agg.MonthsLeft {
			agg.MonthsLeft = p.MonthsLeft
		}
		isDone := i < len(done) && done[i]
		if !isDone {
			agg.MonthlyToSave += p.MonthlyToSave
			if p.Overdue {
				agg.Overdue = true
			}
		}
	}
	agg.Remaining = agg.Target - agg.Spent
	return agg
}

// ScheduleRow is one month of the saving schedule: each active group's
// contribution that month plus the combined total.
type ScheduleRow struct {
	Label   string    // e.g. "Jul 2026"; month 1 is the current month
	ByGroup []float64 // aligned to the caller's group-name slice
	Total   float64
}

// BuildSaveSchedule lays out the month-by-month saving plan. Under the concurrent
// model, group g contributes monthly[g] for months 1..monthsLeft[g], so the
// combined total steps down as nearer groups finish. The horizon is the longest
// monthsLeft. Pure (now is passed in). monthly and monthsLeft must be aligned.
func BuildSaveSchedule(monthly []float64, monthsLeft []int, now time.Time) []ScheduleRow {
	horizon := 0
	for _, m := range monthsLeft {
		if m > horizon {
			horizon = m
		}
	}
	// Anchor labels to the 1st of the current month so adding months can't
	// overflow (e.g. May 31 + 1mo → Jul 1, skipping June).
	base := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	rows := make([]ScheduleRow, 0, horizon)
	for m := 1; m <= horizon; m++ {
		row := ScheduleRow{
			Label:   base.AddDate(0, m-1, 0).Format("Jan 2006"),
			ByGroup: make([]float64, len(monthly)),
		}
		for g := range monthly {
			if m <= monthsLeft[g] {
				row.ByGroup[g] = monthly[g]
				row.Total += monthly[g]
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// ---------- Store ----------

type Store struct {
	db *sql.DB
}

func New(db *sql.DB) *Store { return &Store{db: db} }

func nowRFC() string { return time.Now().UTC().Format(time.RFC3339) }

func randToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Migrate applies the embedded schema idempotently.
func (s *Store) Migrate() error {
	for _, stmt := range strings.Split(stripSQLComments(schemaSQL), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("schema stmt %q: %w", stmt, err)
		}
	}
	// Additive column migrations for databases created before the column existed
	// (CREATE TABLE IF NOT EXISTS leaves an existing table untouched).
	if err := s.ensureColumn("users", "totp_last_step", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("users", "role", "TEXT NOT NULL DEFAULT 'member'"); err != nil {
		return err
	}
	if err := s.ensureColumn("projects", "status", "TEXT NOT NULL DEFAULT 'saving'"); err != nil {
		return err
	}
	if err := s.ensureColumn("projects", "finance_pct", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("projects", "buffer_pct", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("project_items", "phase_id", "INTEGER REFERENCES project_phases(id) ON DELETE SET NULL"); err != nil {
		return err
	}
	// Index phase_id now that the column is guaranteed to exist (it can't live in
	// schema.sql, which runs before the ensureColumn above on legacy databases).
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_pitem_phase ON project_items(phase_id)`); err != nil {
		return err
	}
	if err := s.ensureColumn("nav_snapshots", "cash_eur", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("nav_snapshots", "inv_eur", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("households", "monthly_income", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("households", "monthly_savings", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("operating_costs", "due_day", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("transactions", "transfer_id", "INTEGER"); err != nil {
		return err
	}
	if err := s.ensureColumn("projects", "finance_method", "TEXT NOT NULL DEFAULT 'save'"); err != nil {
		return err
	}
	if err := s.ensureColumn("projects", "loan_deposit", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("projects", "loan_apr", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("projects", "loan_term_months", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("accounts", "spending", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("operating_costs", "category_id", "INTEGER REFERENCES categories(id) ON DELETE SET NULL"); err != nil {
		return err
	}
	// Partial payments: each payment is a transaction tagged with project_item_id;
	// an item's "paid" total and a project's "spent" are derived from these rows
	// (the ledger stays the single source of truth — no denormalized copy to desync).
	if err := s.ensureColumn("transactions", "project_item_id", "INTEGER REFERENCES project_items(id) ON DELETE SET NULL"); err != nil {
		return err
	}
	// Backfill: legacy item purchases predate project_item_id but recorded a single
	// transaction via project_items.transaction_id — reconnect it so derived spend
	// includes them. Idempotent: only fills rows still NULL.
	if _, err := s.db.Exec(
		`UPDATE transactions SET project_item_id=(SELECT pi.id FROM project_items pi WHERE pi.transaction_id=transactions.id)
		 WHERE project_item_id IS NULL
		   AND EXISTS (SELECT 1 FROM project_items pi WHERE pi.transaction_id=transactions.id)`); err != nil {
		return err
	}
	// Loan scheduling + a payable deposit: repayments can start on a future date
	// (budget deducts from then), and the deposit is paid as real transactions.
	if err := s.ensureColumn("projects", "loan_start_date", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// Vestigial v1 single-payment deposit link — kept only so the backfill below
	// can migrate rows created before deposits became incremental.
	if err := s.ensureColumn("projects", "deposit_transaction_id", "INTEGER REFERENCES transactions(id) ON DELETE SET NULL"); err != nil {
		return err
	}
	// Deposits are paid in any number of increments: each payment transaction is
	// tagged with deposit_project_id, and a project's DepositPaid is Σ over them
	// (ledger-derived — editing or deleting a payment self-corrects, exactly like
	// item payments; no single-slot link to guard).
	if err := s.ensureColumn("transactions", "deposit_project_id", "INTEGER REFERENCES projects(id) ON DELETE SET NULL"); err != nil {
		return err
	}
	// Backfill: v1 deposits were linked one-shot via projects.deposit_transaction_id;
	// tag that transaction so the derived Σ includes it. Idempotent (NULL guard).
	if _, err := s.db.Exec(
		`UPDATE transactions SET deposit_project_id=(SELECT p.id FROM projects p WHERE p.deposit_transaction_id=transactions.id)
		 WHERE deposit_project_id IS NULL
		   AND EXISTS (SELECT 1 FROM projects p WHERE p.deposit_transaction_id=transactions.id)`); err != nil {
		return err
	}
	// The derived paid/spent sums are correlated subqueries over transactions —
	// index both payment-tag columns. Created here (not schema.sql) because the
	// columns arrive via ensureColumn on legacy databases, like idx_pitem_phase.
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_tx_project_item ON transactions(project_item_id)`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_tx_deposit_project ON transactions(deposit_project_id)`); err != nil {
		return err
	}
	return nil
}

// ensureColumn adds a column to an existing table if it is not already present.
// table and column are internal constants, never user input.
func (s *Store) ensureColumn(table, column, decl string) error {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('` + table + `')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return rows.Err() // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, decl))
	return err
}

func stripSQLComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// Users

func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// ErrUserLimit is returned when creating an account would exceed the cap.
var ErrUserLimit = errors.New("user limit reached")

// CreateUserCapped inserts a user only while fewer than maxUsers exist, checked
// in the same statement so concurrent sign-ups can't both pass a stale count.
// Returns ErrUserLimit when full (a duplicate username is a plain error).
func (s *Store) CreateUserCapped(username, passwordHash, role string, maxUsers int) (int64, error) {
	return createUserCapped(s.db, username, passwordHash, role, maxUsers)
}

// execer is satisfied by *sql.DB and *sql.Tx.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func createUserCapped(db execer, username, passwordHash, role string, maxUsers int) (int64, error) {
	if role == "" {
		role = "member"
	}
	res, err := db.Exec(
		`INSERT INTO users(username, password_hash, role, created_at)
		 SELECT ?,?,?,? WHERE (SELECT COUNT(*) FROM users) < ?`,
		username, passwordHash, role, nowRFC(), maxUsers)
	if err != nil {
		return 0, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, err
	} else if n == 0 {
		return 0, ErrUserLimit
	}
	return res.LastInsertId()
}

func (s *Store) CreateUser(username, passwordHash, role string) (int64, error) {
	if role == "" {
		role = "member"
	}
	res, err := s.db.Exec(
		`INSERT INTO users(username, password_hash, role, created_at) VALUES(?,?,?,?)`,
		username, passwordHash, role, nowRFC())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListUsers returns all household members, oldest first, for the settings page.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func scanUser(sc interface {
	Scan(dest ...any) error
}) (*User, error) {
	var u User
	var enabled int
	if err := sc.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.TOTPSecret,
		&enabled, &u.FailedLogins, &u.LockedUntil, &u.CreatedAt, &u.TOTPLastStep); err != nil {
		return nil, err
	}
	u.TOTPEnabled = enabled == 1
	return &u, nil
}

const userCols = `id, username, password_hash, role, totp_secret, totp_enabled, failed_logins, locked_until, created_at, totp_last_step`

func (s *Store) GetUserByUsername(username string) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE username=?`, username))
}

func (s *Store) GetUserByID(id int64) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id))
}

func (s *Store) SetTOTPSecret(userID int64, secret string) error {
	_, err := s.db.Exec(`UPDATE users SET totp_secret=? WHERE id=?`, secret, userID)
	return err
}

func (s *Store) EnableTOTP(userID int64) error {
	_, err := s.db.Exec(`UPDATE users SET totp_enabled=1 WHERE id=?`, userID)
	return err
}

// SetTOTPLastStep records the most recent TOTP step a user consumed, so the same
// code can't be replayed within its validity window.
func (s *Store) SetTOTPLastStep(userID, step int64) error {
	_, err := s.db.Exec(`UPDATE users SET totp_last_step=? WHERE id=?`, step, userID)
	return err
}

// ConsumeTOTPStep records a TOTP code's time step as used, but only if it is
// newer than the last one used — in one statement, so the same code submitted
// twice at once passes at most once (and the step never moves backwards).
// Returns whether this call consumed it.
func (s *Store) ConsumeTOTPStep(userID, step int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE users SET totp_last_step=? WHERE id=? AND totp_last_step<?`, step, userID, step)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ClaimLoginAttempt atomically reserves one guess (password, TOTP or recovery
// code) against the user's lockout budget BEFORE the guess is checked, so
// parallel requests can't all slip past a stale "not locked" read. It returns
// false when the account is locked. The claim counts as a failure up front:
// the claim that reaches maxFailed also sets the lockout, and a correct guess
// undoes its claim with ResetFailedLogin or RefundLoginAttempt. Once a lockout
// has expired the count restarts.
func (s *Store) ClaimLoginAttempt(userID int64, maxFailed int, window time.Duration) (bool, error) {
	now := time.Now().UTC()
	var n int
	// SET expressions all see the pre-update row; the WHERE only admits an
	// unlocked or expired-lock row, so a set locked_until here means "expired".
	err := s.db.QueryRow(`
		UPDATE users SET
		  failed_logins = CASE WHEN locked_until<>'' THEN 1 ELSE failed_logins+1 END,
		  locked_until  = CASE WHEN (CASE WHEN locked_until<>'' THEN 1 ELSE failed_logins+1 END) >= ?
		                       THEN ? ELSE '' END
		WHERE id=? AND (locked_until='' OR locked_until<=?)
		RETURNING failed_logins`,
		maxFailed, now.Add(window).Format(time.RFC3339), userID, now.Format(time.RFC3339)).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// RefundLoginAttempt gives back one claimed attempt after a correct password
// for a user who still has to pass 2FA: the count drops by one (so earlier 2FA
// failures still count) and a lockout this claim set is lifted.
func (s *Store) RefundLoginAttempt(userID int64, maxFailed int) error {
	_, err := s.db.Exec(`
		UPDATE users SET
		  failed_logins = MAX(failed_logins-1, 0),
		  locked_until  = CASE WHEN failed_logins-1 < ? THEN '' ELSE locked_until END
		WHERE id=?`, maxFailed, userID)
	return err
}

func (s *Store) ResetFailedLogin(userID int64) error {
	_, err := s.db.Exec(`UPDATE users SET failed_logins=0, locked_until='' WHERE id=?`, userID)
	return err
}

// EnsureAdmin guarantees the install has exactly one admin: if no user has the
// admin role (e.g. a database created before roles existed), the earliest user
// is promoted. A no-op on an empty database.
func (s *Store) EnsureAdmin() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.db.Exec(`UPDATE users SET role='admin' WHERE id=(SELECT id FROM users ORDER BY id LIMIT 1)`)
	return err
}

// Households (single-row: this install is one household)

func (s *Store) EnsureHousehold() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM households`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO households(name, created_at) VALUES(?,?)`, "Household", nowRFC())
	return err
}

func (s *Store) GetHousehold() (*Household, error) {
	var h Household
	err := s.db.QueryRow(`SELECT id, name, monthly_income, monthly_savings FROM households ORDER BY id LIMIT 1`).
		Scan(&h.ID, &h.Name, &h.MonthlyIncome, &h.MonthlySavings)
	if err != nil {
		return nil, err
	}
	return &h, nil
}

func (s *Store) SetMonthlyIncome(v float64) error {
	_, err := s.db.Exec(`UPDATE households SET monthly_income=? WHERE id=(SELECT id FROM households ORDER BY id LIMIT 1)`, v)
	return err
}

func (s *Store) SetMonthlySavings(v float64) error {
	_, err := s.db.Exec(`UPDATE households SET monthly_savings=? WHERE id=(SELECT id FROM households ORDER BY id LIMIT 1)`, v)
	return err
}

func (s *Store) RenameHousehold(name string) error {
	_, err := s.db.Exec(`UPDATE households SET name=? WHERE id=(SELECT id FROM households ORDER BY id LIMIT 1)`, name)
	return err
}

// Operating costs

const operatingCostCols = `id, name, category, amount, currency, frequency, due_day, category_id, active, created_at`

func scanOperatingCost(sc interface{ Scan(...any) error }) (*OperatingCost, error) {
	var c OperatingCost
	var active int
	if err := sc.Scan(&c.ID, &c.Name, &c.Category, &c.Amount, &c.Currency, &c.Frequency, &c.DueDay, &c.CategoryID, &active, &c.CreatedAt); err != nil {
		return nil, err
	}
	c.Active = active == 1
	return &c, nil
}

func (s *Store) ListOperatingCosts() ([]OperatingCost, error) {
	rows, err := s.db.Query(`SELECT ` + operatingCostCols + ` FROM operating_costs ORDER BY active DESC, category, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OperatingCost
	for rows.Next() {
		c, err := scanOperatingCost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *Store) GetOperatingCost(id int64) (*OperatingCost, error) {
	return scanOperatingCost(s.db.QueryRow(`SELECT `+operatingCostCols+` FROM operating_costs WHERE id=?`, id))
}

func (s *Store) CreateOperatingCost(name, category string, amount float64, currency, frequency string, dueDay int, categoryID *int64, active bool) error {
	_, err := s.db.Exec(
		`INSERT INTO operating_costs(name, category, amount, currency, frequency, due_day, category_id, active, created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		name, category, amount, currency, frequency, dueDay, nullableInt(categoryID), boolInt(active), nowRFC())
	return err
}

func (s *Store) UpdateOperatingCost(id int64, name, category string, amount float64, currency, frequency string, dueDay int, categoryID *int64, active bool) error {
	_, err := s.db.Exec(
		`UPDATE operating_costs SET name=?, category=?, amount=?, currency=?, frequency=?, due_day=?, category_id=?, active=? WHERE id=?`,
		name, category, amount, currency, frequency, dueDay, nullableInt(categoryID), boolInt(active), id)
	return err
}

func (s *Store) SetOperatingCostActive(id int64, active bool) error {
	_, err := s.db.Exec(`UPDATE operating_costs SET active=? WHERE id=?`, boolInt(active), id)
	return err
}

func (s *Store) DeleteOperatingCost(id int64) error {
	_, err := s.db.Exec(`DELETE FROM operating_costs WHERE id=?`, id)
	return err
}

// Invites

var ErrInviteExpired = errors.New("invite expired")

func (s *Store) CreateInvite(tokenHash, label string, createdBy int64, expiresAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO invites(token_hash, label, created_by, created_at, expires_at) VALUES(?,?,?,?,?)`,
		tokenHash, label, createdBy, nowRFC(), expiresAt.UTC().Format(time.RFC3339))
	return err
}

// GetValidInvite returns the unused, unexpired invite matching the token hash,
// or an error (sql.ErrNoRows for unknown/used, ErrInviteExpired if lapsed).
func (s *Store) GetValidInvite(tokenHash string) (*Invite, error) {
	var iv Invite
	err := s.db.QueryRow(
		`SELECT id, token_hash, label, created_by, created_at, expires_at, used_at
		 FROM invites WHERE token_hash=? AND used_at=''`, tokenHash).
		Scan(&iv.ID, &iv.TokenHash, &iv.Label, &iv.CreatedBy, &iv.CreatedAt, &iv.ExpiresAt, &iv.UsedAt)
	if err != nil {
		return nil, err
	}
	exp, err := time.Parse(time.RFC3339, iv.ExpiresAt)
	if err != nil {
		return nil, err
	}
	if time.Now().After(exp) {
		return nil, ErrInviteExpired
	}
	return &iv, nil
}

// ErrInviteUsed is returned when an invite was already consumed or has expired.
var ErrInviteUsed = errors.New("invite already used or expired")

// AcceptInvite consumes an invite and creates its member account in one
// transaction: the invite is claimed only if still unused and unexpired, and
// the user is inserted only under the maxUsers cap. Either both happen or
// neither, so concurrent submissions of one link create at most one account
// (and a rejected username leaves the invite usable). Returns ErrInviteUsed,
// ErrUserLimit, or a plain error (e.g. a taken username).
func (s *Store) AcceptInvite(inviteID int64, username, passwordHash string, maxUsers int) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := nowRFC()
	res, err := tx.Exec(`UPDATE invites SET used_at=? WHERE id=? AND used_at='' AND expires_at>?`, now, inviteID, now)
	if err != nil {
		return 0, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, err
	} else if n == 0 {
		return 0, ErrInviteUsed
	}
	uid, err := createUserCapped(tx, username, passwordHash, "member", maxUsers)
	if err != nil {
		return 0, err
	}
	return uid, tx.Commit()
}

// ListPendingInvites returns unused, unexpired invites (newest first).
func (s *Store) ListPendingInvites() ([]Invite, error) {
	rows, err := s.db.Query(
		`SELECT id, token_hash, label, created_by, created_at, expires_at, used_at
		 FROM invites WHERE used_at='' AND expires_at > ? ORDER BY id DESC`, nowRFC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var iv Invite
		if err := rows.Scan(&iv.ID, &iv.TokenHash, &iv.Label, &iv.CreatedBy, &iv.CreatedAt, &iv.ExpiresAt, &iv.UsedAt); err != nil {
			return nil, err
		}
		out = append(out, iv)
	}
	return out, rows.Err()
}

func (s *Store) DeleteInvite(id int64) error {
	_, err := s.db.Exec(`DELETE FROM invites WHERE id=?`, id)
	return err
}

// Recovery codes

func (s *Store) StoreRecoveryCodes(userID int64, hashes []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM recovery_codes WHERE user_id=?`, userID); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.Exec(`INSERT INTO recovery_codes(user_id, code_hash) VALUES(?,?)`, userID, h); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListUnusedRecoveryCodes(userID int64) ([]RecoveryCode, error) {
	rows, err := s.db.Query(`SELECT id, code_hash FROM recovery_codes WHERE user_id=? AND used=0`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecoveryCode
	for rows.Next() {
		var rc RecoveryCode
		if err := rows.Scan(&rc.ID, &rc.Hash); err != nil {
			return nil, err
		}
		out = append(out, rc)
	}
	return out, rows.Err()
}

// UseRecoveryCode marks a recovery code used, only if it still is unused, and
// reports whether this call did — so one code can't pass twice concurrently.
func (s *Store) UseRecoveryCode(id int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE recovery_codes SET used=1 WHERE id=? AND used=0`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Sessions

func (s *Store) CreateSession(userID int64, mfaPassed bool, ttl time.Duration) (*Session, error) {
	token, err := randToken(24)
	if err != nil {
		return nil, err
	}
	exp := time.Now().UTC().Add(ttl)
	mfa := 0
	if mfaPassed {
		mfa = 1
	}
	if _, err := s.db.Exec(
		`INSERT INTO sessions(id, user_id, mfa_passed, created_at, expires_at) VALUES(?,?,?,?,?)`,
		token, userID, mfa, nowRFC(), exp.Format(time.RFC3339)); err != nil {
		return nil, err
	}
	return &Session{Token: token, UserID: userID, MFAPassed: mfaPassed, ExpiresAt: exp}, nil
}

var ErrSessionExpired = errors.New("session expired")

func (s *Store) GetSession(token string) (*Session, error) {
	var sess Session
	var mfa int
	var expStr string
	err := s.db.QueryRow(`SELECT id, user_id, mfa_passed, expires_at FROM sessions WHERE id=?`, token).
		Scan(&sess.Token, &sess.UserID, &mfa, &expStr)
	if err != nil {
		return nil, err
	}
	sess.MFAPassed = mfa == 1
	exp, err := time.Parse(time.RFC3339, expStr)
	if err != nil {
		return nil, err
	}
	sess.ExpiresAt = exp
	if time.Now().After(exp) {
		_ = s.DeleteSession(token)
		return nil, ErrSessionExpired
	}
	return &sess, nil
}

func (s *Store) SetSessionMFAPassed(token string) error {
	_, err := s.db.Exec(`UPDATE sessions SET mfa_passed=1 WHERE id=?`, token)
	return err
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id=?`, token)
	return err
}

func (s *Store) DeleteExpiredSessions() error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, nowRFC())
	return err
}

// Accounts

func (s *Store) ListAccounts() ([]Account, error) {
	rows, err := s.db.Query(`
		SELECT a.id, a.name, a.type, a.currency, a.opening_balance, a.spending,
		       a.opening_balance + COALESCE((SELECT SUM(t.amount) FROM transactions t WHERE t.account_id=a.id), 0)
		FROM accounts a
		WHERE a.archived=0
		ORDER BY a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		var spending int
		if err := rows.Scan(&a.ID, &a.Name, &a.Type, &a.Currency, &a.OpeningBalance, &spending, &a.Balance); err != nil {
			return nil, err
		}
		a.Spending = spending == 1
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetAccount(id int64) (*Account, error) {
	var a Account
	var spending int
	err := s.db.QueryRow(`SELECT id, name, type, currency, opening_balance, spending FROM accounts WHERE id=?`, id).
		Scan(&a.ID, &a.Name, &a.Type, &a.Currency, &a.OpeningBalance, &spending)
	if err != nil {
		return nil, err
	}
	a.Spending = spending == 1
	return &a, nil
}

func (s *Store) CreateAccount(name, typ, currency string, opening float64, spending bool) error {
	_, err := s.db.Exec(
		`INSERT INTO accounts(name, type, currency, opening_balance, spending, created_at) VALUES(?,?,?,?,?,?)`,
		name, typ, currency, opening, boolInt(spending), nowRFC())
	return err
}

func (s *Store) UpdateAccount(id int64, name, typ, currency string, opening float64, spending bool) error {
	_, err := s.db.Exec(
		`UPDATE accounts SET name=?, type=?, currency=?, opening_balance=?, spending=? WHERE id=?`,
		name, typ, currency, opening, boolInt(spending), id)
	return err
}

// CountSpendingAccounts reports how many active accounts are flagged as spending
// accounts (used to decide whether budget actuals scope to them).
func (s *Store) CountSpendingAccounts() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE spending=1 AND archived=0`).Scan(&n)
	return n, err
}

// Categories

func (s *Store) EnsureDefaultCategories() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM categories`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	defaults := []Category{
		{Name: "Salary", Kind: "income"},
		{Name: "Other income", Kind: "income"},
		{Name: "Groceries", Kind: "expense"},
		{Name: "Rent / Mortgage", Kind: "expense"},
		{Name: "Utilities", Kind: "expense"},
		{Name: "Dining", Kind: "expense"},
		{Name: "Transport", Kind: "expense"},
		{Name: "Savings transfer", Kind: "expense"},
		{Name: "Other", Kind: "expense"},
	}
	for _, c := range defaults {
		if _, err := s.db.Exec(`INSERT INTO categories(name, kind) VALUES(?,?)`, c.Name, c.Kind); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListCategories() ([]Category, error) {
	rows, err := s.db.Query(`SELECT id, name, kind FROM categories ORDER BY kind, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetCategory(id int64) (*Category, error) {
	var c Category
	if err := s.db.QueryRow(`SELECT id, name, kind FROM categories WHERE id=?`, id).Scan(&c.ID, &c.Name, &c.Kind); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) CreateCategory(name, kind string) error {
	_, err := s.db.Exec(`INSERT INTO categories(name, kind) VALUES(?,?)`, name, kind)
	return err
}

func (s *Store) UpdateCategory(id int64, name, kind string) error {
	_, err := s.db.Exec(`UPDATE categories SET name=?, kind=? WHERE id=?`, name, kind, id)
	return err
}

// DeleteCategory removes a category; transactions referencing it fall back to
// uncategorized (category_id → NULL via the ON DELETE SET NULL foreign key).
func (s *Store) DeleteCategory(id int64) error {
	_, err := s.db.Exec(`DELETE FROM categories WHERE id=?`, id)
	return err
}

// CategoryUsage returns the number of transactions referencing each category id.
func (s *Store) CategoryUsage() (map[int64]int, error) {
	rows, err := s.db.Query(`SELECT category_id, COUNT(*) FROM transactions WHERE category_id IS NOT NULL GROUP BY category_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// Transactions

func (s *Store) ListTransactions(limit int) ([]Transaction, error) {
	rows, err := s.db.Query(`
		SELECT t.id, t.account_id, t.date, t.amount, t.category_id, t.payee, t.notes, t.project_id, t.transfer_id,
		       a.name, COALESCE(c.name, '')
		FROM transactions t
		JOIN accounts a ON a.id = t.account_id
		LEFT JOIN categories c ON c.id = t.category_id
		ORDER BY t.date DESC, t.id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transaction
	for rows.Next() {
		var t Transaction
		if err := rows.Scan(&t.ID, &t.AccountID, &t.Date, &t.Amount, &t.CategoryID,
			&t.Payee, &t.Notes, &t.ProjectID, &t.TransferID, &t.AccountName, &t.CategoryName); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) CreateTransaction(accountID int64, date string, amount float64, categoryID *int64, payee, notes string) error {
	var cat any
	if categoryID != nil {
		cat = *categoryID
	}
	_, err := s.db.Exec(
		`INSERT INTO transactions(account_id, date, amount, category_id, payee, notes, created_at) VALUES(?,?,?,?,?,?,?)`,
		accountID, date, amount, cat, payee, notes, nowRFC())
	return err
}

func (s *Store) GetTransaction(id int64) (*Transaction, error) {
	var t Transaction
	err := s.db.QueryRow(
		`SELECT id, account_id, date, amount, category_id, payee, notes, project_id, transfer_id FROM transactions WHERE id=?`, id).
		Scan(&t.ID, &t.AccountID, &t.Date, &t.Amount, &t.CategoryID, &t.Payee, &t.Notes, &t.ProjectID, &t.TransferID)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) UpdateTransaction(id, accountID int64, date string, amount float64, categoryID *int64, payee, notes string) error {
	var cat any
	if categoryID != nil {
		cat = *categoryID
	}
	return s.changeTransaction(id,
		`UPDATE transactions SET account_id=?, date=?, amount=?, category_id=?, payee=?, notes=? WHERE id=?`,
		accountID, date, amount, cat, payee, notes, id)
}

// DeleteTransaction removes a transaction. A transfer leg takes its partner leg
// with it: deleting just one would leave the other moving a balance (and NAV) on
// its own, invisible to the reports that skip transfer legs.
func (s *Store) DeleteTransaction(id int64) error {
	return s.changeTransaction(id,
		`DELETE FROM transactions WHERE id=? OR transfer_id=(SELECT transfer_id FROM transactions WHERE id=?)`,
		id, id)
}

// changeTransaction runs an edit/delete of transaction id and, when that row is
// a project-item payment, re-derives the item's done state in the same
// transaction (see syncItemStatus). A missing id is a no-op.
func (s *Store) changeTransaction(id int64, query string, args ...any) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var itemID sql.NullInt64
	err = tx.QueryRow(`SELECT project_item_id FROM transactions WHERE id=?`, id).Scan(&itemID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(query, args...); err != nil {
		return err
	}
	if itemID.Valid {
		if err := syncItemStatus(tx, itemID.Int64); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// syncItemStatus re-derives a project item's done flag from its ledger payments
// after one of them was edited or deleted. The paid total is always derived, but
// purchased/actual_cost are stored when the item closes, so without this a
// deleted payment would leave the item "done" with money still owed (and its
// phase dropped from the monthly saving). Rules:
//   - done, but the payments now total less than the closing total → reopen
//     (a payment the closing counted is gone; re-close it with a new payment);
//   - done and still covered → stay done, actual_cost follows the new total;
//   - open, and an edit lifted the total to the estimate → close it.
//
// transaction_id is re-pointed at the latest remaining payment (or NULL).
// Legacy items closed without a recorded closing total are left as they are.
func syncItemStatus(tx *sql.Tx, itemID int64) error {
	var estimated, paid float64
	var purchased bool
	var actual sql.NullFloat64
	var latest sql.NullInt64
	if err := tx.QueryRow(
		`SELECT estimated_cost, purchased, actual_cost,
		        COALESCE((SELECT -SUM(t.amount) FROM transactions t WHERE t.project_item_id=project_items.id),0),
		        (SELECT MAX(t.id) FROM transactions t WHERE t.project_item_id=project_items.id)
		 FROM project_items WHERE id=?`, itemID).
		Scan(&estimated, &purchased, &actual, &paid, &latest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // item deleted; nothing to sync
		}
		return err
	}
	done := purchased
	switch {
	case purchased && !actual.Valid:
		return nil // legacy close with no recorded total: nothing to compare against
	case purchased && paid+0.005 < actual.Float64:
		done = false
	case !purchased && estimated > 0 && paid+0.005 >= estimated:
		done = true
	}
	var actualCost any
	if done {
		actualCost = paid
	}
	_, err := tx.Exec(`UPDATE project_items SET purchased=?, actual_cost=?, transaction_id=? WHERE id=?`,
		done, actualCost, latest, itemID)
	return err
}

// TxDedupKey is the identity used to detect duplicate imports: date + amount +
// payee (case-insensitive). Pure.
func TxDedupKey(date string, amount float64, payee string) string {
	return fmt.Sprintf("%s|%.2f|%s", date, amount, strings.ToLower(strings.TrimSpace(payee)))
}

// AccountTxKeys returns the set of dedup keys for an account's transactions.
func (s *Store) AccountTxKeys(accountID int64) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT date, amount, payee FROM transactions WHERE account_id=?`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var d, p string
		var a float64
		if err := rows.Scan(&d, &a, &p); err != nil {
			return nil, err
		}
		out[TxDedupKey(d, a, p)] = true
	}
	return out, rows.Err()
}

// Backup writes a consistent snapshot of the database to path (which must not
// already exist) using SQLite's VACUUM INTO.
func (s *Store) Backup(path string) error {
	_, err := s.db.Exec(`VACUUM INTO ?`, path)
	return err
}

// CreateTransfer records a money move between two accounts as two balanced legs
// (expense from `from`, income to `to`), atomically, sharing a transfer_id so
// they're recognizable (and excluded from spend/income analytics). NAV is
// unaffected. amount must be positive.
func (s *Store) CreateTransfer(fromID, toID int64, date string, amount float64, notes string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowRFC()
	res, err := tx.Exec(
		`INSERT INTO transactions(account_id, date, amount, payee, notes, created_at) VALUES(?,?,?,?,?,?)`,
		fromID, date, -amount, "Transfer", notes, now)
	if err != nil {
		return err
	}
	legA, err := res.LastInsertId()
	if err != nil {
		return err
	}
	res, err = tx.Exec(
		`INSERT INTO transactions(account_id, date, amount, payee, notes, created_at) VALUES(?,?,?,?,?,?)`,
		toID, date, amount, "Transfer", notes, now)
	if err != nil {
		return err
	}
	legB, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE transactions SET transfer_id=? WHERE id IN (?,?)`, legA, legA, legB); err != nil {
		return err
	}
	return tx.Commit()
}

// Spending analytics (transfers excluded — they aren't real income/expense)

type MonthTotal struct {
	Month   string // "YYYY-MM"
	Income  float64
	Expense float64
}

type CategoryTotal struct {
	Category string
	Amount   float64 // total expense (positive)
}

// MonthlyTotals returns income/expense per calendar month, keyed by "YYYY-MM",
// converted to EUR by each account's currency.
func (s *Store) MonthlyTotals() (map[string]MonthTotal, error) {
	rate, err := s.eurRate()
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`
		SELECT substr(t.date,1,7) AS m, a.currency,
		       SUM(CASE WHEN t.amount>0 THEN t.amount ELSE 0 END),
		       SUM(CASE WHEN t.amount<0 THEN -t.amount ELSE 0 END)
		FROM transactions t JOIN accounts a ON a.id=t.account_id
		WHERE t.transfer_id IS NULL
		GROUP BY m, a.currency`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]MonthTotal{}
	for rows.Next() {
		var m, ccy string
		var inc, exp float64
		if err := rows.Scan(&m, &ccy, &inc, &exp); err != nil {
			return nil, err
		}
		t := out[m]
		t.Month = m
		t.Income += inc * rate(ccy)
		t.Expense += exp * rate(ccy)
		out[m] = t
	}
	return out, rows.Err()
}

// eurRate returns a converter from a currency code to its EUR rate, using the
// stored fx_rates. A blank, unknown or non-positive rate counts as 1 (EUR) until
// a rate is fetched.
func (s *Store) eurRate() (func(ccy string) float64, error) {
	rates, err := s.GetFXRates()
	if err != nil {
		return nil, err
	}
	return func(ccy string) float64 {
		if r, ok := rates[strings.ToUpper(ccy)]; ok && r > 0 {
			return r
		}
		return 1
	}, nil
}

// CategorySpend returns this-period actual spend per transaction category, keyed
// by category id and converted to EUR. It counts expense transactions (negative,
// non-transfer) in [from,to]. When spendingOnly is true it restricts to accounts
// flagged as spending accounts — the dedicated operating-cost accounts. Used to
// measure budgets (operating costs) against actuals.
func (s *Store) CategorySpend(from, to string, spendingOnly bool) (map[int64]float64, error) {
	rate, err := s.eurRate()
	if err != nil {
		return nil, err
	}
	q := `SELECT t.category_id, a.currency, SUM(-t.amount)
	      FROM transactions t JOIN accounts a ON a.id=t.account_id
	      WHERE t.amount<0 AND t.transfer_id IS NULL AND t.category_id IS NOT NULL
	        AND t.date>=? AND t.date<=?`
	if spendingOnly {
		q += ` AND a.spending=1`
	}
	q += ` GROUP BY t.category_id, a.currency`
	rows, err := s.db.Query(q, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]float64{}
	for rows.Next() {
		var cid int64
		var ccy string
		var sum float64
		if err := rows.Scan(&cid, &ccy, &sum); err != nil {
			return nil, err
		}
		out[cid] += sum * rate(ccy)
	}
	return out, rows.Err()
}

// BudgetStatus compares an actual spend against a budget and returns the percent
// used (rounded; may exceed 100) and a state: "none" (no budget), "ok", "low"
// (≥90% but not over) or "over" (actual > budget). Pure.
func BudgetStatus(actual, budget float64) (pct int, state string) {
	if budget <= 0 {
		return 0, "none"
	}
	pct = int(actual/budget*100 + 0.5)
	switch {
	case actual > budget:
		return pct, "over"
	case actual >= budget*0.9:
		return pct, "low"
	default:
		return pct, "ok"
	}
}

// SpendByCategory returns total expense per category over [from,to] (inclusive
// YYYY-MM-DD), converted to EUR, largest first.
func (s *Store) SpendByCategory(from, to string) ([]CategoryTotal, error) {
	rate, err := s.eurRate()
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`
		SELECT COALESCE(NULLIF(c.name,''),'Uncategorized') AS cat, a.currency, SUM(-t.amount)
		FROM transactions t
		JOIN accounts a ON a.id=t.account_id
		LEFT JOIN categories c ON c.id=t.category_id
		WHERE t.amount<0 AND t.transfer_id IS NULL AND t.date>=? AND t.date<=?
		GROUP BY cat, a.currency`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sums := map[string]float64{}
	for rows.Next() {
		var cat, ccy string
		var sum float64
		if err := rows.Scan(&cat, &ccy, &sum); err != nil {
			return nil, err
		}
		sums[cat] += sum * rate(ccy)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]CategoryTotal, 0, len(sums))
	for cat, amt := range sums {
		out = append(out, CategoryTotal{Category: cat, Amount: amt})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Amount != out[j].Amount {
			return out[i].Amount > out[j].Amount
		}
		return out[i].Category < out[j].Category
	})
	return out, nil
}

// Holdings

func (s *Store) ListHoldings() ([]Holding, error) {
	rows, err := s.db.Query(
		`SELECT id, symbol, name, kind, quantity, price, currency, cost_basis, updated_at FROM holdings ORDER BY symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Holding
	for rows.Next() {
		var h Holding
		if err := rows.Scan(&h.ID, &h.Symbol, &h.Name, &h.Kind, &h.Quantity,
			&h.Price, &h.Currency, &h.CostBasis, &h.UpdatedAt); err != nil {
			return nil, err
		}
		h.Value = h.Quantity * h.Price
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) GetHolding(id int64) (*Holding, error) {
	var h Holding
	err := s.db.QueryRow(
		`SELECT id, symbol, name, kind, quantity, price, currency, cost_basis, updated_at FROM holdings WHERE id=?`, id).
		Scan(&h.ID, &h.Symbol, &h.Name, &h.Kind, &h.Quantity, &h.Price, &h.Currency, &h.CostBasis, &h.UpdatedAt)
	if err != nil {
		return nil, err
	}
	h.Value = h.Quantity * h.Price
	return &h, nil
}

func (s *Store) CreateHolding(symbol, name, kind string, qty, price float64, currency string, costBasis *float64) error {
	var cb any
	if costBasis != nil {
		cb = *costBasis
	}
	now := nowRFC()
	_, err := s.db.Exec(
		`INSERT INTO holdings(symbol, name, kind, quantity, price, currency, cost_basis, updated_at, created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		symbol, name, kind, qty, price, currency, cb, now, now)
	return err
}

func (s *Store) UpdateHolding(id int64, symbol, name, kind string, qty, price float64, currency string, costBasis *float64) error {
	var cb any
	if costBasis != nil {
		cb = *costBasis
	}
	_, err := s.db.Exec(
		`UPDATE holdings SET symbol=?, name=?, kind=?, quantity=?, price=?, currency=?, cost_basis=?, updated_at=? WHERE id=?`,
		symbol, name, kind, qty, price, currency, cb, nowRFC(), id)
	return err
}

// Projects

// projectSelect lists project columns plus computed item totals (estimate sum
// and purchased actual-cost sum), mirroring ListAccounts's balance subquery.
const projectSelect = `
	SELECT p.id, p.name, p.target_date, p.status, p.finance_pct, p.buffer_pct, p.account_id,
	       p.finance_method, p.loan_deposit, p.loan_apr, p.loan_term_months, p.loan_start_date,
	       COALESCE((SELECT SUM(estimated_cost) FROM project_items WHERE project_id=p.id),0),
	       COALESCE((SELECT -SUM(t.amount) FROM transactions t JOIN project_items pi ON pi.id=t.project_item_id WHERE pi.project_id=p.id),0),
	       COALESCE((SELECT MAX(-SUM(t.amount),0) FROM transactions t WHERE t.deposit_project_id=p.id),0),
	       COALESCE((SELECT MAX(t.date) FROM transactions t WHERE t.deposit_project_id=p.id AND t.amount<0),'')
	FROM projects p`

func scanProject(sc interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	if err := sc.Scan(&p.ID, &p.Name, &p.TargetDate, &p.Status, &p.FinancePct,
		&p.BufferPct, &p.AccountID, &p.FinanceMethod, &p.LoanDeposit, &p.LoanAPR, &p.LoanTermMonths,
		&p.LoanStartDate, &p.ItemsTotal, &p.Spent, &p.DepositPaid, &p.DepositPaidDate); err != nil {
		return nil, err
	}
	return &p, nil
}

// projectOrder sorts saving/planning first, then completed, then archived.
const projectOrder = ` ORDER BY CASE p.status WHEN 'saving' THEN 0 WHEN 'planning' THEN 1 WHEN 'completed' THEN 2 ELSE 3 END, p.target_date, p.id`

func (s *Store) listProjectsWhere(where string) ([]Project, error) {
	rows, err := s.db.Query(projectSelect + where + projectOrder)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *Store) ListProjects() ([]Project, error) { return s.listProjectsWhere("") }

// ListActiveProjects returns projects still being planned or saved for.
func (s *Store) ListActiveProjects() ([]Project, error) {
	return s.listProjectsWhere(` WHERE p.status IN ('planning','saving')`)
}

func (s *Store) GetProject(id int64) (*Project, error) {
	return scanProject(s.db.QueryRow(projectSelect+` WHERE p.id=?`, id))
}

// ProjectInput carries the editable fields of a project (save vs loan financing).
type ProjectInput struct {
	Name, TargetDate, Status string
	FinancePct, BufferPct    float64
	AccountID                *int64
	FinanceMethod            string // "save" | "loan"
	LoanDeposit, LoanAPR     float64
	LoanTermMonths           int
	LoanStartDate            string // YYYY-MM-DD or ""
}

func financeMethod(m string) string {
	if m == "loan" {
		return "loan"
	}
	return "save"
}

func (s *Store) CreateProjectInput(in ProjectInput) (int64, error) {
	var acct any
	if in.AccountID != nil {
		acct = *in.AccountID
	}
	res, err := s.db.Exec(
		`INSERT INTO projects(name, target_date, status, finance_pct, buffer_pct, account_id,
		    finance_method, loan_deposit, loan_apr, loan_term_months, loan_start_date, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		in.Name, in.TargetDate, in.Status, in.FinancePct, in.BufferPct, acct,
		financeMethod(in.FinanceMethod), in.LoanDeposit, in.LoanAPR, in.LoanTermMonths, in.LoanStartDate, nowRFC())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateProject(id int64, in ProjectInput) error {
	var acct any
	if in.AccountID != nil {
		acct = *in.AccountID
	}
	_, err := s.db.Exec(
		`UPDATE projects SET name=?, target_date=?, status=?, finance_pct=?, buffer_pct=?, account_id=?,
		    finance_method=?, loan_deposit=?, loan_apr=?, loan_term_months=?, loan_start_date=? WHERE id=?`,
		in.Name, in.TargetDate, in.Status, in.FinancePct, in.BufferPct, acct,
		financeMethod(in.FinanceMethod), in.LoanDeposit, in.LoanAPR, in.LoanTermMonths, in.LoanStartDate, id)
	return err
}

// UpdateProjectLoan stores the loan-financing inputs for a project. A term of 0
// disables loan modelling for that project.
func (s *Store) UpdateProjectLoan(id int64, deposit, aprPct float64, termMonths int, startDate string) error {
	_, err := s.db.Exec(
		`UPDATE projects SET loan_deposit=?, loan_apr=?, loan_term_months=?, loan_start_date=? WHERE id=?`,
		deposit, aprPct, termMonths, startDate, id)
	return err
}

// PayLoanDeposit records one loan-deposit payment as a real expense transaction
// tagged with deposit_project_id. Deposits accept any number of increments (a
// part now, more later); the project's DepositPaid is Σ over the tagged rows, so
// editing or deleting any payment self-corrects — exactly like item payments.
func (s *Store) PayLoanDeposit(projectID, accountID int64, date string, amount float64, categoryID *int64) error {
	var name string
	if err := s.db.QueryRow(`SELECT name FROM projects WHERE id=?`, projectID).Scan(&name); err != nil {
		return err
	}
	var cat any
	if categoryID != nil {
		cat = *categoryID
	}
	_, err := s.db.Exec(
		`INSERT INTO transactions(account_id, date, amount, category_id, payee, notes, project_id, deposit_project_id, created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		accountID, date, -amount, cat, name, "Loan deposit", projectID, projectID, nowRFC())
	return err
}

// ListDepositPayments returns a project's deposit payments (newest first), for
// the payment history in the detail page's deposit panel.
func (s *Store) ListDepositPayments(projectID int64) ([]Transaction, error) {
	rows, err := s.db.Query(`
		SELECT t.id, t.account_id, t.date, t.amount, a.name
		FROM transactions t
		JOIN accounts a ON a.id = t.account_id
		WHERE t.deposit_project_id = ?
		ORDER BY t.date DESC, t.id DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transaction
	for rows.Next() {
		var t Transaction
		if err := rows.Scan(&t.ID, &t.AccountID, &t.Date, &t.Amount, &t.AccountName); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) SetProjectStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE projects SET status=? WHERE id=?`, status, id)
	return err
}

func (s *Store) DeleteProject(id int64) error {
	_, err := s.db.Exec(`DELETE FROM projects WHERE id=?`, id)
	return err
}

// Project phases

// phaseSelect lists phase columns plus computed item totals, scoped to a phase.
const phaseSelect = `
	SELECT ph.id, ph.project_id, ph.name, ph.target_date, ph.created_at,
	       COALESCE((SELECT SUM(estimated_cost) FROM project_items WHERE phase_id=ph.id),0),
	       COALESCE((SELECT -SUM(t.amount) FROM transactions t JOIN project_items pi ON pi.id=t.project_item_id WHERE pi.phase_id=ph.id),0)
	FROM project_phases ph`

func scanPhase(sc interface{ Scan(...any) error }) (*Phase, error) {
	var ph Phase
	if err := sc.Scan(&ph.ID, &ph.ProjectID, &ph.Name, &ph.TargetDate, &ph.CreatedAt,
		&ph.ItemsTotal, &ph.Spent); err != nil {
		return nil, err
	}
	return &ph, nil
}

// ListPhases returns a project's phases in date order (undated last).
func (s *Store) ListPhases(projectID int64) ([]Phase, error) {
	rows, err := s.db.Query(phaseSelect+
		` WHERE ph.project_id=? ORDER BY ph.target_date='' , ph.target_date, ph.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Phase
	for rows.Next() {
		ph, err := scanPhase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ph)
	}
	return out, rows.Err()
}

func (s *Store) GetPhase(id int64) (*Phase, error) {
	return scanPhase(s.db.QueryRow(phaseSelect+` WHERE ph.id=?`, id))
}

func (s *Store) CreatePhase(projectID int64, name, targetDate string) error {
	_, err := s.db.Exec(
		`INSERT INTO project_phases(project_id, name, target_date, created_at) VALUES(?,?,?,?)`,
		projectID, name, targetDate, nowRFC())
	return err
}

func (s *Store) UpdatePhase(id int64, name, targetDate string) error {
	_, err := s.db.Exec(`UPDATE project_phases SET name=?, target_date=? WHERE id=?`, name, targetDate, id)
	return err
}

// DeletePhase removes a phase; its items fall back to General (phase_id → NULL
// via the ON DELETE SET NULL foreign key).
func (s *Store) DeletePhase(id int64) error {
	_, err := s.db.Exec(`DELETE FROM project_phases WHERE id=?`, id)
	return err
}

// Project items

// projectItemCols derives amount_paid from the ledger (Σ of the item's payment
// transactions) rather than a stored column, so it can never drift from the
// transactions that back it (e.g. when a payment is later deleted or edited).
const projectItemCols = `id, project_id, phase_id, name, estimated_cost, actual_cost,
	COALESCE((SELECT -SUM(t.amount) FROM transactions t WHERE t.project_item_id=project_items.id),0),
	essential, purchased, transaction_id, created_at`

func scanItem(sc interface{ Scan(...any) error }) (*ProjectItem, error) {
	var it ProjectItem
	var essential, purchased int
	if err := sc.Scan(&it.ID, &it.ProjectID, &it.PhaseID, &it.Name, &it.EstimatedCost, &it.ActualCost,
		&it.AmountPaid, &essential, &purchased, &it.TransactionID, &it.CreatedAt); err != nil {
		return nil, err
	}
	it.Essential = essential == 1
	it.Purchased = purchased == 1
	return &it, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullableInt returns the int64 value for SQL binding, or nil (→ SQL NULL) when
// the pointer is nil.
func nullableInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// NullID wraps an optional id pointer as a nullable column value (for struct
// fields), keeping database/sql out of the controller layer.
func NullID(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}

func (s *Store) ListProjectItems(projectID int64) ([]ProjectItem, error) {
	rows, err := s.db.Query(`SELECT `+projectItemCols+` FROM project_items WHERE project_id=? ORDER BY purchased, id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectItem
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

func (s *Store) GetProjectItem(id int64) (*ProjectItem, error) {
	return scanItem(s.db.QueryRow(`SELECT `+projectItemCols+` FROM project_items WHERE id=?`, id))
}

func (s *Store) CreateProjectItem(projectID int64, phaseID *int64, name string, estimated float64, essential bool) error {
	var phase any
	if phaseID != nil {
		phase = *phaseID
	}
	_, err := s.db.Exec(
		`INSERT INTO project_items(project_id, phase_id, name, estimated_cost, essential, created_at) VALUES(?,?,?,?,?,?)`,
		projectID, phase, name, estimated, boolInt(essential), nowRFC())
	return err
}

func (s *Store) UpdateProjectItem(id int64, name string, estimated float64, essential bool) error {
	_, err := s.db.Exec(
		`UPDATE project_items SET name=?, estimated_cost=?, essential=? WHERE id=?`,
		name, estimated, boolInt(essential), id)
	return err
}

func (s *Store) DeleteProjectItem(id int64) error {
	_, err := s.db.Exec(`DELETE FROM project_items WHERE id=?`, id)
	return err
}

// SetItemPhase reassigns an item to a phase, or to General (nil phaseID).
func (s *Store) SetItemPhase(id int64, phaseID *int64) error {
	var phase any
	if phaseID != nil {
		phase = *phaseID
	}
	_, err := s.db.Exec(`UPDATE project_items SET phase_id=? WHERE id=?`, phase, id)
	return err
}

// CountUnpurchasedItems reports how many items in a project remain unbought; used
// to auto-complete a project once everything is purchased.
func (s *Store) CountUnpurchasedItems(projectID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM project_items WHERE project_id=? AND purchased=0`, projectID).Scan(&n)
	return n, err
}

// PayItem records a payment toward an item (a deposit or the balance) as a real
// expense transaction tagged with the project and the item. The item's paid total
// is derived from these transactions, not stored; PayItem only flips it to
// purchased (done) when `final` is set or the cumulative payments reach the
// estimate, so a deposit leaves it open while the closing payment completes it.
func (s *Store) PayItem(itemID, accountID int64, date string, amount float64, categoryID *int64, final bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var projectID int64
	var name string
	var estimated, paid float64
	var wasDone bool
	// paid is the ledger-derived total so far (before the payment we're about to
	// add). It's read inside the transaction so two concurrent payments can't both
	// see the same old total and leave a fully paid item open.
	if err := tx.QueryRow(
		`SELECT project_id, name, estimated_cost, purchased,
		        COALESCE((SELECT -SUM(t.amount) FROM transactions t WHERE t.project_item_id=project_items.id),0)
		 FROM project_items WHERE id=?`, itemID).
		Scan(&projectID, &name, &estimated, &wasDone, &paid); err != nil {
		return err
	}
	var cat any
	if categoryID != nil {
		cat = *categoryID
	}
	res, err := tx.Exec(
		`INSERT INTO transactions(account_id, date, amount, category_id, payee, notes, project_id, project_item_id, created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		accountID, date, -amount, cat, name, "", projectID, itemID, nowRFC())
	if err != nil {
		return err
	}
	txID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	newPaid := paid + amount
	// Done when the user flags this the final payment, or (for an estimated item) the
	// running total covers the estimate — within half a cent so float drift doesn't
	// leave it a cent short. A zero-estimate item completes only on the explicit flag,
	// so it can still take a deposit first. An item already closed stays closed (a
	// late extra payment, or a double-submitted form, must not reopen it).
	done := final || wasDone || (estimated > 0 && newPaid+0.005 >= estimated)
	var actual any // actual_cost: the final total once done, else NULL (in progress)
	purchased := 0
	if done {
		purchased = 1
		actual = newPaid
	}
	if _, err := tx.Exec(
		`UPDATE project_items SET purchased=?, actual_cost=?, transaction_id=? WHERE id=?`,
		purchased, actual, txID, itemID); err != nil {
		return err
	}
	return tx.Commit()
}

// ListItemPayments returns the payments recorded against an item (newest first),
// for the per-item payment history on the pay form.
func (s *Store) ListItemPayments(itemID int64) ([]Transaction, error) {
	rows, err := s.db.Query(`
		SELECT t.id, t.account_id, t.date, t.amount, a.name
		FROM transactions t
		JOIN accounts a ON a.id = t.account_id
		WHERE t.project_item_id = ?
		ORDER BY t.date DESC, t.id DESC`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transaction
	for rows.Next() {
		var t Transaction
		if err := rows.Scan(&t.ID, &t.AccountID, &t.Date, &t.Amount, &t.AccountName); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// NAV

// ComputeNAV returns (cash, investments, total). v1 treats all amounts as the
// base currency (EUR); multi-currency conversion arrives with a later release.
func (s *Store) ComputeNAV() (cash, investments, total float64, err error) {
	rate, err := s.eurRate()
	if err != nil {
		return 0, 0, 0, err
	}
	accts, err := s.ListAccounts()
	if err != nil {
		return 0, 0, 0, err
	}
	for _, a := range accts {
		if a.Spending {
			continue // spending accounts fund operating costs; excluded from NAV
		}
		cash += a.Balance * rate(a.Currency)
	}
	holds, err := s.ListHoldings()
	if err != nil {
		return 0, 0, 0, err
	}
	for _, h := range holds {
		investments += h.Value * rate(h.Currency)
	}
	return cash, investments, cash + investments, nil
}

// FX rates + market-data helpers

func (s *Store) GetFXRates() (map[string]float64, error) {
	rows, err := s.db.Query(`SELECT currency, rate_to_eur FROM fx_rates`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{"EUR": 1}
	for rows.Next() {
		var c string
		var r float64
		if err := rows.Scan(&c, &r); err != nil {
			return nil, err
		}
		out[strings.ToUpper(c)] = r
	}
	return out, rows.Err()
}

func (s *Store) UpsertFXRate(currency string, rate float64) error {
	_, err := s.db.Exec(
		`INSERT INTO fx_rates(currency, rate_to_eur, updated_at) VALUES(?,?,?)
		 ON CONFLICT(currency) DO UPDATE SET rate_to_eur=excluded.rate_to_eur, updated_at=excluded.updated_at`,
		strings.ToUpper(currency), rate, nowRFC())
	return err
}

// DistinctCurrencies lists the non-EUR currencies in use (accounts + holdings).
func (s *Store) DistinctCurrencies() ([]string, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT UPPER(currency) FROM (
		    SELECT currency FROM accounts WHERE archived=0
		    UNION SELECT currency FROM holdings
		) WHERE TRIM(currency) <> '' AND UPPER(currency) <> 'EUR'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStrings(rows)
}

// DistinctHoldingSymbols lists the non-empty holding symbols to price.
func (s *Store) DistinctHoldingSymbols() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT symbol FROM holdings WHERE TRIM(symbol) <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStrings(rows)
}

func scanStrings(rows *sql.Rows) ([]string, error) {
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// UpdateHoldingPrice sets the price (and updated_at) for every holding with the
// given symbol (case-insensitive).
func (s *Store) UpdateHoldingPrice(symbol string, price float64) error {
	_, err := s.db.Exec(
		`UPDATE holdings SET price=?, updated_at=? WHERE UPPER(symbol)=UPPER(?)`,
		price, nowRFC(), symbol)
	return err
}

// LatestHoldingUpdate returns the most recent holdings.updated_at, or "".
func (s *Store) LatestHoldingUpdate() (string, error) {
	var t sql.NullString
	err := s.db.QueryRow(`SELECT MAX(updated_at) FROM holdings`).Scan(&t)
	if err != nil {
		return "", err
	}
	return t.String, nil
}

// App meta (key/value)

func (s *Store) GetMeta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM app_meta WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO app_meta(key, value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

const metaLastFetch = "market_last_fetch"

// AutoFetchedToday reports whether a successful market-data fetch already
// happened today (UTC) — used to cap automatic fetches at once per day.
func (s *Store) AutoFetchedToday() bool {
	v, err := s.GetMeta(metaLastFetch)
	if err != nil {
		return false
	}
	return len(v) >= 10 && v[:10] == time.Now().UTC().Format("2006-01-02")
}

// MarkAutoFetched stamps the current time as the last successful fetch.
func (s *Store) MarkAutoFetched() error { return s.SetMeta(metaLastFetch, nowRFC()) }

// NAV history (snapshots)

type NavSnapshot struct {
	ID      int64
	TakenAt string // RFC3339
	NavEUR  float64
	CashEUR float64
	InvEUR  float64
}

func (s *Store) RecordNavSnapshot(cash, inv, total float64) error {
	_, err := s.db.Exec(
		`INSERT INTO nav_snapshots(taken_at, nav_eur, cash_eur, inv_eur) VALUES(?,?,?,?)`,
		nowRFC(), total, cash, inv)
	return err
}

// EnsureDailySnapshot records today's NAV unless a snapshot already exists for
// today (UTC). Idempotent per day; safe to call on boot and on a timer.
func (s *Store) EnsureDailySnapshot() error {
	var latest string
	err := s.db.QueryRow(`SELECT taken_at FROM nav_snapshots ORDER BY taken_at DESC, id DESC LIMIT 1`).Scan(&latest)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	today := time.Now().UTC().Format("2006-01-02")
	if len(latest) >= 10 && latest[:10] == today {
		return nil // already captured today
	}
	cash, inv, total, err := s.ComputeNAV()
	if err != nil {
		return err
	}
	return s.RecordNavSnapshot(cash, inv, total)
}

// ListNavSnapshots returns the most recent `limit` snapshots (0 = all) ordered
// chronologically (ascending) for charting and tables.
func (s *Store) ListNavSnapshots(limit int) ([]NavSnapshot, error) {
	q := `SELECT id, taken_at, nav_eur, cash_eur, inv_eur FROM nav_snapshots ORDER BY taken_at, id`
	if limit > 0 {
		// take the most recent `limit`, then present ascending
		q = `SELECT id, taken_at, nav_eur, cash_eur, inv_eur FROM
		       (SELECT id, taken_at, nav_eur, cash_eur, inv_eur FROM nav_snapshots ORDER BY taken_at DESC, id DESC LIMIT ?)
		     ORDER BY taken_at, id`
	}
	var rows *sql.Rows
	var err error
	if limit > 0 {
		rows, err = s.db.Query(q, limit)
	} else {
		rows, err = s.db.Query(q)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NavSnapshot
	for rows.Next() {
		var n NavSnapshot
		if err := rows.Scan(&n.ID, &n.TakenAt, &n.NavEUR, &n.CashEUR, &n.InvEUR); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---------- Pocket money (PRIVATE, per-user) ----------
//
// Unlike the rest of the store, every pocket-money method is scoped by userID and
// must only ever be called with the logged-in user's own id. Nothing here feeds
// NAV, accounts, or any shared/household view — it is a private side-pot.

// PocketEntry is one private pocket-money movement: + top-up, − spend.
type PocketEntry struct {
	ID        int64
	UserID    int64
	Date      string
	Amount    float64
	Category  string
	Note      string
	CreatedAt string
}

// ListPocketEntries returns a user's own pocket-money entries, newest first.
func (s *Store) ListPocketEntries(userID int64) ([]PocketEntry, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, date, amount, category, note, created_at
		 FROM pocket_entries WHERE user_id=? ORDER BY date DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PocketEntry
	for rows.Next() {
		var e PocketEntry
		if err := rows.Scan(&e.ID, &e.UserID, &e.Date, &e.Amount, &e.Category, &e.Note, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PocketBalance is the running balance of a user's pocket fund (Σ amount).
func (s *Store) PocketBalance(userID int64) (float64, error) {
	var bal sql.NullFloat64
	err := s.db.QueryRow(`SELECT SUM(amount) FROM pocket_entries WHERE user_id=?`, userID).Scan(&bal)
	if err != nil {
		return 0, err
	}
	return bal.Float64, nil
}

// AddPocketEntry records a movement on a user's own fund.
func (s *Store) AddPocketEntry(userID int64, date string, amount float64, category, note string) error {
	_, err := s.db.Exec(
		`INSERT INTO pocket_entries(user_id, date, amount, category, note, created_at) VALUES(?,?,?,?,?,?)`,
		userID, date, amount, category, note, nowRFC())
	return err
}

// DeletePocketEntry removes one of a user's own entries. The user_id predicate
// makes it impossible to delete another user's row even with a guessed id.
func (s *Store) DeletePocketEntry(userID, id int64) error {
	_, err := s.db.Exec(`DELETE FROM pocket_entries WHERE id=? AND user_id=?`, id, userID)
	return err
}

// PocketByCategory totals a user's pocket spends per category (largest first),
// for a private breakdown of where the pocket money goes.
func (s *Store) PocketByCategory(userID int64) ([]CategoryTotal, error) {
	rows, err := s.db.Query(
		`SELECT COALESCE(NULLIF(category,''),'Uncategorized') AS cat, SUM(-amount)
		 FROM pocket_entries WHERE user_id=? AND amount<0
		 GROUP BY cat ORDER BY SUM(-amount) DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CategoryTotal
	for rows.Next() {
		var c CategoryTotal
		if err := rows.Scan(&c.Category, &c.Amount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------- API tokens (personal access tokens for the JSON API) ----------
//
// Only the hash is stored (controller hashes the plaintext, like invites). A
// token is owned by a user, so the API authenticates as that user — keeping
// per-user data (pocket money) private.

type APIToken struct {
	ID         int64
	UserID     int64
	Name       string
	CreatedAt  string
	LastUsedAt string
}

// CreateAPIToken stores a new token's hash for a user.
func (s *Store) CreateAPIToken(userID int64, name, tokenHash string) error {
	_, err := s.db.Exec(
		`INSERT INTO api_tokens(user_id, name, token_hash, created_at) VALUES(?,?,?,?)`,
		userID, name, tokenHash, nowRFC())
	return err
}

// UserByAPITokenHash returns the owner of a token (by its hash) and stamps the
// token's last-used time. Returns sql.ErrNoRows when the token is unknown.
func (s *Store) UserByAPITokenHash(tokenHash string) (*User, error) {
	var u User
	err := s.db.QueryRow(
		`SELECT u.id, u.username, u.role FROM api_tokens t JOIN users u ON u.id=t.user_id WHERE t.token_hash=?`,
		tokenHash).Scan(&u.ID, &u.Username, &u.Role)
	if err != nil {
		return nil, err
	}
	_, _ = s.db.Exec(`UPDATE api_tokens SET last_used_at=? WHERE token_hash=?`, nowRFC(), tokenHash)
	return &u, nil
}

// ListAPITokens returns a user's own tokens (never the secret), newest first.
func (s *Store) ListAPITokens(userID int64) ([]APIToken, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, name, created_at, last_used_at FROM api_tokens WHERE user_id=? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.CreatedAt, &t.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteAPIToken revokes one of a user's own tokens (scoped by user_id).
func (s *Store) DeleteAPIToken(userID, id int64) error {
	_, err := s.db.Exec(`DELETE FROM api_tokens WHERE id=? AND user_id=?`, id, userID)
	return err
}
