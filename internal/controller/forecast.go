package controller

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"lira/internal/model"
	"lira/internal/view"
)

// actualMonthlySaving estimates the household's real saving rate from the ledger:
// the average net (income − expense, transfers excluded) over the most recent
// months that have data, up to maxMonths. Returns 0 when there is no history.
func (c *Controller) actualMonthlySaving(maxMonths int) float64 {
	totals, _ := c.store.MonthlyTotals()
	if len(totals) == 0 {
		return 0
	}
	months := make([]string, 0, len(totals))
	for m := range totals {
		months = append(months, m)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(months))) // newest first
	if len(months) > maxMonths {
		months = months[:maxMonths]
	}
	var sum float64
	for _, m := range months {
		t := totals[m]
		sum += t.Income - t.Expense
	}
	return sum / float64(len(months))
}

// monthLabels returns months+1 "Jan 2006" labels starting at the 1st of `start`'s
// month. Anchoring to the 1st avoids AddDate skipping months on day 29–31.
func monthLabels(start time.Time, months int) []string {
	base := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
	out := make([]string, 0, months+1)
	for i := 0; i <= months; i++ {
		out = append(out, base.AddDate(0, i, 0).Format("Jan 2006"))
	}
	return out
}

// etaLabel turns a months-from-now count into a friendly target date, or a
// fallback string when the target is unreachable in the horizon.
func etaLabel(start time.Time, months int) string {
	if months < 0 {
		return ""
	}
	base := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
	return base.AddDate(0, months, 0).Format("Jan 2006")
}

// buildCashLedger renders the burn-down as a chronological event list: between
// spend clusters it folds in the saving that accrued (a green "Saving × N mo"
// row), the money set aside for projects in that window (a green "Saving for
// projects × K mo" row) and — as its own explicit red row — any loan
// repayments in that window ("Loan payments × K mo"), then draws each spend
// down sequentially. If loan repayments continue past the final spend, a
// trailing Saving/Loan-payments pair covers the rest of the horizon, so the
// last row's Cash equals the chart's end balance instead of the timeline
// silently stopping while payments keep running. The running balance
// reconciles with BuildBurnDown at every month boundary.
func buildCashLedger(cash, monthlyIn float64, ins, outs map[int]float64, spends []burnSpend, labels []string, months int) []cashRow {
	sort.SliceStable(spends, func(i, j int) bool { return spends[i].Month < spends[j].Month })
	// Prefix sums of the per-month flows, so each window's total is O(1).
	inPre := make([]float64, months+1)
	outPre := make([]float64, months+1)
	for m := 1; m <= months; m++ {
		inPre[m] = inPre[m-1] + ins[m]
		outPre[m] = outPre[m-1] + outs[m]
	}
	activeMonths := func(flow map[int]float64, from, to int) int {
		k := 0
		for i := from + 1; i <= to; i++ {
			if flow[i] > 0 {
				k++
			}
		}
		return k
	}
	ledger := make([]cashRow, 0, len(spends))
	running := cash
	// accrue emits the Saving and Loan-payments rows covering months (from, to].
	accrue := func(from, to int) {
		gap := to - from
		if gap <= 0 {
			return
		}
		if monthlyIn > 0 {
			add := monthlyIn * float64(gap)
			running += add
			ledger = append(ledger, cashRow{Kind: "save", Label: "Saving", When: labels[to], Amount: add, Balance: running, Months: gap})
		}
		if windowIn := inPre[to] - inPre[from]; windowIn > 0 {
			running += windowIn
			ledger = append(ledger, cashRow{Kind: "save", Label: "Saving for projects", When: labels[to],
				Amount: windowIn, Balance: running, Months: activeMonths(ins, from, to)})
		}
		if windowOut := outPre[to] - outPre[from]; windowOut > 0 {
			k := activeMonths(outs, from, to)
			label := "Loan payment"
			if k > 1 {
				label = fmt.Sprintf("Loan payments × %d mo", k)
			}
			running -= windowOut
			ledger = append(ledger, cashRow{Kind: "spend", Label: label, When: "to " + labels[to], Amount: windowOut, Balance: running})
		}
	}
	last := 0
	for i := 0; i < len(spends); {
		m := spends[i].Month
		accrue(last, m)
		for ; i < len(spends) && spends[i].Month == m; i++ {
			running -= spends[i].Amount
			ledger = append(ledger, cashRow{Kind: "spend", Label: spends[i].Label, When: spends[i].When, Amount: spends[i].Amount, Balance: running})
		}
		last = m
	}
	// Flows outlasting the final spend: show the rest of the horizon too.
	if outPre[months]-outPre[last] > 0 || inPre[months]-inPre[last] > 0 {
		accrue(last, months)
	}
	return ledger
}

// addSavingStream adds a project group's monthly saving to the per-month inflows
// for months 1..monthsLeft (at least month 1, so an overdue group's "due now"
// amount lands before its month-1 lump). monthsLeft never exceeds the group's
// lump month (both count calendar months from now; monthsLeft floors by day).
func addSavingStream(ins map[int]float64, monthly float64, monthsLeft int) {
	if monthly <= 0 {
		return
	}
	if monthsLeft < 1 {
		monthsLeft = 1
	}
	for m := 1; m <= monthsLeft; m++ {
		ins[m] += monthly
	}
}

// addLoanStream adds a loan's monthly repayment to the per-month outflows from
// its start month for the remaining term, clipped to the chart horizon.
func addLoanStream(outs map[int]float64, startMonth, monthsLeft, horizon int, monthly float64) {
	if startMonth < 1 {
		startMonth = 1
	}
	for m := startMonth; m <= horizon && m-startMonth < monthsLeft; m++ {
		outs[m] += monthly
	}
}

// depositPaidPreLira reports whether an unpaid-looking loan deposit should be
// left out of the burn-down: its due month (`when`, YYYY-MM-DD) is already
// BEFORE the current month and no payments are recorded, which implies it was
// handed over before Lira tracked it (no phantom lump). A deposit due THIS month
// is still upcoming, and recorded increments prove Lira is tracking it — both
// keep the remainder as a lump (monthsFromNow clamps past dates to next month).
// An unparseable date is skipped too, as there is no month to put it in.
func depositPaidPreLira(now time.Time, when string, depositPaid float64) bool {
	d, err := time.Parse("2006-01-02", when)
	if err != nil {
		return true
	}
	past := (d.Year()*12 + int(d.Month())) < (now.Year()*12 + int(now.Month()))
	return past && depositPaid == 0
}

// monthsFromNow returns how many whole months `dateStr` (YYYY-MM-DD) is ahead of
// `now`, anchored to the 1st of each month. Returns -1 for an empty/unparseable
// date; clamps past/this-month dates to 1 (next month).
func monthsFromNow(now time.Time, dateStr string) int {
	d, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return -1
	}
	diff := (d.Year()*12 + int(d.Month())) - (now.Year()*12 + int(now.Month()))
	if diff < 1 {
		diff = 1
	}
	return diff
}

// burnSpend is a scheduled project outflow collected from the projects.
type burnSpend struct {
	Month  int
	Label  string
	When   string // project/phase target date
	Amount float64
}

// cashRow is one line of the burn-down's chronological cash ledger: either a
// folded-in "save" inflow (the savings accrued since the previous spend) or a
// "spend" outflow, each with the running balance after it.
type cashRow struct {
	Kind    string // "save" | "spend"
	Label   string
	When    string
	Amount  float64
	Balance float64
	Months  int // months of saving folded into a "save" row
}

// Reports renders the index of analytical views (history, forecasts, insights).
func (c *Controller) Reports(w http.ResponseWriter, r *http.Request) {
	c.render(w, r, "reports.html", map[string]any{})
}

// BurnDown is a read-only cash burn-down: starting from current liquid cash it
// adds the planned monthly saving and subtracts each active save-financed
// project's remaining budget as a lump at its target date, so you can see, month
// by month, how much spendable money you'll have. Loan-financed projects
// contribute no lump for the borrowed part (repaid monthly through the budget) —
// only their still-unpaid deposit, as a lump at the loan start date.
func (c *Controller) BurnDown(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	cash, _, _, err := c.store.ComputeNAV()
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	var monthlyIn float64
	if hh, _ := c.store.GetHousehold(); hh != nil {
		monthlyIn = hh.MonthlySavings
	}
	months := paramInt(r, "months", 36)
	if months < 6 {
		months = 6
	}
	if months > 360 {
		months = 360
	}

	events := map[int]float64{}
	ins := map[int]float64{}  // money set aside for save-financed projects
	outs := map[int]float64{} // recurring outflows: upcoming loan repayments
	var spends []burnSpend
	var undated, beyond int
	active, _ := c.store.ListActiveProjects()
	for i := range active {
		p := &active[i]
		if loanFinanced(p) {
			// Financed by a loan: the borrowed part is repaid monthly through the
			// budget (no lump) — but the DEPOSIT is real cash leaving at loan
			// start. Include it only while unpaid AND the loan hasn't started:
			// once paid it's already in the ledger, and a loan that has started
			// implies the deposit was handed over even if Lira never recorded it
			// (pre-feature projects), so no phantom future lump.
			_, agg := c.projectGroups(p, now)
			// A loan that hasn't started yet is NOT in today's budget (its
			// MonthlyToSave is 0), so the planned monthly saving can't already
			// account for its repayments — deduct them from the projected cash
			// from the start month for the term. Running loans are left alone:
			// their payment is already inside the budget's Available, which is
			// what the Settings saving figure is based on (no double-count).
			if agg.LoanStart != "" && agg.UpcomingMonthly > 0 {
				addLoanStream(outs, monthsFromNow(now, agg.LoanStart), agg.MonthsLeft, months, agg.UpcomingMonthly)
			}
			if rem := agg.Financed - p.DepositPaid; rem > 0.005 {
				when := p.LoanStartDate
				if when == "" {
					when = p.TargetDate
				}
				if when == "" {
					undated++
					continue
				}
				if depositPaidPreLira(now, when, p.DepositPaid) {
					continue
				}
				m := monthsFromNow(now, when)
				if m > months {
					beyond++
					continue
				}
				events[m] += rem
				spends = append(spends, burnSpend{Month: m, Label: p.Name + " · loan deposit", When: when, Amount: rem})
			}
			continue
		}
		groups, _ := c.projectGroups(p, now)
		for _, g := range groups {
			if g.Done || g.Plan.Remaining <= 0 {
				continue
			}
			if g.TargetDate == "" {
				undated++
				continue
			}
			m := monthsFromNow(now, g.TargetDate)
			if m > months {
				beyond++
				continue
			}
			events[m] += g.Plan.Remaining
			// The planned saving (Settings) is what the budget leaves free AFTER
			// project commitments, so this group's own monthly saving isn't in it:
			// add it back until its date, else the lump would be paid from cash
			// that was never credited. It covers ToSave by the lump month (an
			// overdue group's is due now, month 1), so the lump nets down to just
			// the financed share drawn from existing cash.
			addSavingStream(ins, g.Plan.MonthlyToSave, g.Plan.MonthsLeft)
			label := p.Name
			if !g.IsGeneral {
				label = p.Name + " · " + g.Name
			}
			spends = append(spends, burnSpend{Month: m, Label: label, When: g.TargetDate, Amount: g.Plan.Remaining})
		}
	}

	pts := model.BuildBurnDown(cash, monthlyIn, ins, outs, events, months)
	labels := monthLabels(now, months)
	bal := make([]float64, len(pts))
	spd := make([]float64, len(pts))
	low, lowMonth := pts[0].Balance, 0
	for i, p := range pts {
		bal[i], spd[i] = p.Balance, p.Spend
		if p.Balance < low {
			low, lowMonth = p.Balance, p.Month
		}
	}
	ledger := buildCashLedger(cash, monthlyIn, ins, outs, spends, labels, months)

	c.render(w, r, "burndown.html", map[string]any{
		"StartCash":    cash,
		"MonthlyIn":    monthlyIn,
		"Months":       months,
		"Chart":        view.BurnSeries{Labels: labels, Balance: bal, Spend: spd},
		"Ledger":       ledger,
		"EndBalance":   pts[len(pts)-1].Balance,
		"Low":          low,
		"LowDate":      etaLabel(now, lowMonth),
		"GoesNegative": low < 0,
		"Undated":      undated,
		"Beyond":       beyond,
		"NoSpends":     len(spends) == 0,
		"NoSavings":    monthlyIn <= 0,
		"HasLoanOuts":  len(outs) > 0, // upcoming loan repayments are deducted
		"HasProjectIn": len(ins) > 0,  // project savings are added back until each purchase
	})
}

// NavForecast is a read-only net-worth burn-down: it projects NAV forward at the
// planned monthly saving (from Settings) and at the actual recent saving rate
// (from transactions), against a target line. Inputs come from query params.
func (c *Controller) NavForecast(w http.ResponseWriter, r *http.Request) {
	_, _, nav, err := c.store.ComputeNAV()
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	var planned float64
	if hh, _ := c.store.GetHousehold(); hh != nil {
		planned = hh.MonthlySavings
	}
	actual := c.actualMonthlySaving(6)

	// Default target is the FI number (25× annual operating costs); overridable.
	_, operating, _, _ := c.budgetSummary()
	target := paramFloat(r, "target", operating*12*25)
	ret := paramFloat(r, "return", 0)
	months := paramInt(r, "months", 120)
	if months < 12 {
		months = 12
	}
	if months > 600 {
		months = 600
	}

	pts := model.BuildNavForecast(nav, planned, actual, ret, months)
	labels := monthLabels(time.Now(), months)
	plannedSeries := make([]float64, len(pts))
	actualSeries := make([]float64, len(pts))
	for i, p := range pts {
		plannedSeries[i], actualSeries[i] = p.Planned, p.Actual
	}

	// Year-by-year rows (every 12th month) for a compact table.
	type yearRow struct {
		Year            int
		Label           string
		Planned, Actual float64
		PlannedHit      bool
		ActualHit       bool
	}
	var years []yearRow
	for i := 0; i < len(pts); i += 12 {
		years = append(years, yearRow{
			Year: pts[i].Month / 12, Label: labels[i],
			Planned: pts[i].Planned, Actual: pts[i].Actual,
			PlannedHit: target > 0 && pts[i].Planned >= target,
			ActualHit:  target > 0 && pts[i].Actual >= target,
		})
	}

	plannedETA := model.MonthsToTarget(nav, planned, ret, target, months)
	actualETA := model.MonthsToTarget(nav, actual, ret, target, months)

	c.render(w, r, "forecast.html", map[string]any{
		"StartNAV":  nav,
		"Planned":   planned,
		"Actual":    actual,
		"Target":    target,
		"HasTarget": target > 0,
		"Return":    ret,
		"Months":    months,
		"Years":     years,
		"Chart": view.ForecastSeries{
			Labels: labels, Planned: plannedSeries, Actual: actualSeries, Target: target,
		},
		"PlannedETAMonths": plannedETA,
		"PlannedETA":       etaLabel(time.Now(), plannedETA),
		"ActualETAMonths":  actualETA,
		"ActualETA":        etaLabel(time.Now(), actualETA),
		"NoSavings":        planned <= 0,
	})
}
