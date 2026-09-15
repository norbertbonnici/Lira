package controller

import (
	"sort"
	"strings"
	"time"

	"lira/internal/model"
	"lira/internal/view"
)

// dashboardExtras computes the richer dashboard widgets (monthly change, asset
// allocation donut, savings rate, portfolio gain, next bill, a net-trend
// sparkline) as a map merged into the dashboard render data.
func (c *Controller) dashboardExtras(now time.Time, cash, inv, total float64, holds []model.Holding, snaps []model.NavSnapshot) map[string]any {
	rates, _ := c.store.GetFXRates()
	rate := func(ccy string) float64 {
		if v, ok := rates[strings.ToUpper(ccy)]; ok && v > 0 {
			return v
		}
		return 1
	}
	out := map[string]any{}

	// Monthly change: current NAV vs the last snapshot before this month started.
	if len(snaps) > 0 {
		firstOfMonth := now.Format("2006-01") + "-01"
		baseline := snaps[0].NavEUR // oldest in window as a fallback
		for _, sn := range snaps {
			if len(sn.TakenAt) >= 10 && sn.TakenAt[:10] < firstOfMonth {
				baseline = sn.NavEUR
			}
		}
		change := total - baseline
		out["MonthlyChange"] = change
		out["HasChange"] = true
		if baseline != 0 {
			out["MonthlyChangePct"] = change / baseline * 100
		}
	}

	// Asset allocation (donut + percentages).
	if total > 0 {
		cp := int(cash/total*100 + 0.5)
		out["CashPct"] = cp
		out["InvPct"] = 100 - cp
	}
	out["Alloc"] = []view.DonutSeg{
		{Label: "Investments", Value: inv, Color: "#14463f"},
		{Label: "Cash", Value: cash, Color: "#a9cfc4"},
	}

	// This month's savings rate (saved ÷ income) from the ledger.
	monthly, _ := c.store.MonthlyTotals()
	cur := monthly[now.Format("2006-01")]
	saved := cur.Income - cur.Expense
	out["SavedThisMonth"] = saved
	if cur.Income > 0 {
		out["SavingsRate"] = int(saved/cur.Income*100 + 0.5)
		out["HasSavings"] = true
	}

	// Portfolio unrealized gain (EUR), across holdings with a cost basis.
	var pv, pc float64
	for _, h := range holds {
		fx := rate(h.Currency)
		pv += h.Value * fx
		if h.CostBasis.Valid && h.CostBasis.Float64 > 0 {
			pc += h.CostBasis.Float64 * fx
		}
	}
	out["PortfolioValue"] = pv
	if pc > 0 {
		out["PortfolioGainPct"] = (pv - pc) / pc * 100
		out["HasPortfolio"] = true
	}

	// Next bill due (earliest upcoming active operating cost with a due day).
	costs, _ := c.store.ListOperatingCosts()
	best := ""
	for _, oc := range costs {
		if !oc.Active {
			continue
		}
		d, ok := model.NextDueDate(oc.DueDay, now)
		if !ok {
			continue
		}
		if best == "" || d < best {
			best = d
			out["NextBillName"] = oc.Name
			out["NextBillAmount"] = oc.Amount * rate(oc.Currency)
			out["NextBillDate"] = d
		}
	}
	if best != "" {
		out["HasNextBill"] = true
		if pd, err := time.Parse("2006-01-02", best); err == nil {
			out["NextBillDays"] = int(pd.Sub(now.Truncate(24*time.Hour)).Hours()/24 + 0.5)
		}
	}

	// Net-trend sparkline: last up to 6 months of (income − expense).
	keys := make([]string, 0, len(monthly))
	for k := range monthly {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 6 {
		keys = keys[len(keys)-6:]
	}
	var spark []float64
	for _, k := range keys {
		spark = append(spark, monthly[k].Income-monthly[k].Expense)
	}
	if len(spark) >= 2 {
		out["NetSpark"] = spark
		out["HasSpark"] = true
	}
	return out
}
