package controller

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"lira/internal/model"
	"lira/internal/view"
)

func paramFloat(r *http.Request, name string, def float64) float64 {
	if v := r.FormValue(name); v != "" {
		return parseFloat(v)
	}
	return def
}

func paramInt(r *http.Request, name string, def int) int {
	if v := r.FormValue(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// Projection is a read-only investment growth calculator (compound returns +
// monthly contributions), with an optional FI target and inflation adjustment.
// Inputs come from query params, pre-filled from the user's current data.
func (c *Controller) Projection(w http.ResponseWriter, r *http.Request) {
	_, inv, _, err := c.store.ComputeNAV()
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	_, operating, _, available := c.budgetSummary()
	if available < 0 {
		available = 0
	}

	start := paramFloat(r, "start", inv)
	monthly := paramFloat(r, "monthly", available)
	if monthly < 0 {
		monthly = 0
	}
	ret := paramFloat(r, "return", 7)
	infl := paramFloat(r, "inflation", 0)
	years := paramInt(r, "years", 25)
	if years < 1 {
		years = 1
	} else if years > 60 {
		years = 60
	}
	target := paramFloat(r, "target", operating*12*25) // 4% rule FI number

	rows := model.BuildProjection(start, monthly, ret, years)
	final := rows[len(rows)-1]

	baseYear := time.Now().Year()
	var cs view.ChartSeries
	for _, row := range rows {
		cs.Labels = append(cs.Labels, strconv.Itoa(baseYear+row.Year))
		cs.Cash = append(cs.Cash, row.Contributed)
		cs.Inv = append(cs.Inv, row.Growth)
		cs.Total = append(cs.Total, row.Value)
	}

	realFinal := final.Value
	if infl > 0 {
		realFinal = final.Value / math.Pow(1+infl/100, float64(years))
	}

	fiReached, fiYear := false, 0
	if target > 0 {
		for _, row := range rows {
			if row.Value >= target {
				fiReached, fiYear = true, row.Year
				break
			}
		}
	}

	c.render(w, r, "projection.html", map[string]any{
		"Rows":             rows,
		"Chart":            cs,
		"HasChart":         len(rows) >= 2,
		"Start":            start,
		"Monthly":          monthly,
		"Return":           ret,
		"Years":            years,
		"Target":           target,
		"Inflation":        infl,
		"FinalValue":       final.Value,
		"TotalContributed": final.Contributed,
		"TotalGrowth":      final.Growth,
		"RealFinal":        realFinal,
		"HasInflation":     infl > 0,
		"HasTarget":        target > 0,
		"FIReached":        fiReached,
		"FIYear":           fiYear,
		"FICalendar":       baseYear + fiYear,
	})
}
