package controller

import (
	"net/http"
	"time"
)

// monthRow is one month of the income/expense trend.
type monthRow struct {
	Month   string
	Income  float64
	Expense float64
	Net     float64
	Rate    float64 // savings rate %
}

// catRow is one category's month-to-date spend plus its share-of-max (for the bar).
type catRow struct {
	Category string
	Amount   float64
	Pct      int
}

func (c *Controller) Insights(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	monthly, _ := c.store.MonthlyTotals()

	// Last 12 months, anchored to the 1st so month-end can't skip a month.
	base := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	months := make([]monthRow, 0, 12)
	for i := 11; i >= 0; i-- {
		key := base.AddDate(0, -i, 0).Format("2006-01")
		t := monthly[key]
		net := t.Income - t.Expense
		var rate float64
		if t.Income > 0 {
			rate = net / t.Income * 100
		}
		months = append(months, monthRow{key, t.Income, t.Expense, net, rate})
	}

	cur := monthly[now.Format("2006-01")]
	curNet := cur.Income - cur.Expense
	var curRate float64
	if cur.Income > 0 {
		curRate = curNet / cur.Income * 100
	}

	// Spending by category, month-to-date.
	cats, _ := c.store.SpendByCategory(base.Format("2006-01-02"), now.Format("2006-01-02"))
	var maxCat float64
	for _, ct := range cats {
		if ct.Amount > maxCat {
			maxCat = ct.Amount
		}
	}
	rows := make([]catRow, 0, len(cats))
	for _, ct := range cats {
		pct := 0
		if maxCat > 0 {
			pct = int(ct.Amount / maxCat * 100)
		}
		rows = append(rows, catRow{ct.Category, ct.Amount, pct})
	}

	c.render(w, r, "insights.html", map[string]any{
		"Months":     months,
		"Categories": rows,
		"CurMonth":   now.Format("January 2006"),
		"CurIncome":  cur.Income,
		"CurExpense": cur.Expense,
		"CurNet":     curNet,
		"CurRate":    curRate,
	})
}
