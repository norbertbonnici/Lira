// Package view is the presentation layer: it owns the HTML templates, static
// assets, and display formatting. It renders data passed to it and does no
// request handling, cookie, or database work.
package view

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// pages are content templates; each is parsed together with layout.html.
var pages = []string{
	"auth_login.html",
	"auth_register.html",
	"auth_2fa_setup.html",
	"auth_2fa_verify.html",
	"dashboard.html",
	"accounts.html",
	"account_form.html",
	"transactions.html",
	"transaction_form.html",
	"holdings.html",
	"holding_form.html",
	"settings.html",
	"invite.html",
	"projects.html",
	"project_form.html",
	"project_detail.html",
	"item_form.html",
	"project_purchase.html",
	"phase_form.html",
	"project_plan.html",
	"history.html",
	"costs.html",
	"cost_form.html",
	"transfer_form.html",
	"insights.html",
	"import_upload.html",
	"import_map.html",
	"import_result.html",
	"categories.html",
	"category_form.html",
	"projection.html",
	"forecast.html",
	"burndown.html",
	"reports.html",
	"pocket.html",
}

var funcMap = template.FuncMap{
	"eur":           eur,
	"signClass":     signClass,
	"today":         func() string { return time.Now().Format("2006-01-02") },
	"pct":           pct,
	"chart":         navChart,
	"forecastChart": forecastChart,
	"burndownChart": burndownChart,
	"donut":         donut,
	"sparkline":     sparkline,
	"money":         money,
}

// money formats a value in its own currency: EUR (or empty) uses the € symbol,
// other currencies show the amount with the currency code (e.g. "311.78 USD").
func money(v float64, ccy string) string {
	if ccy == "" || strings.EqualFold(ccy, "EUR") {
		return eur(v)
	}
	return strings.Replace(eur(v), "€", "", 1) + " " + strings.ToUpper(ccy)
}

// ChartSeries is plain chart data (no model dependency, keeping view free of
// model) for the net-worth history SVG: one value per point in each slice.
type ChartSeries struct {
	Labels []string
	Cash   []float64
	Inv    []float64
	Total  []float64
}

// shortEUR is a compact axis label, e.g. "€12.3k" / "€1.2M".
func shortEUR(v float64) string {
	a := v
	if a < 0 {
		a = -a
	}
	switch {
	case a >= 1e6:
		return fmt.Sprintf("€%.1fM", v/1e6)
	case a >= 1e3:
		return fmt.Sprintf("€%.1fk", v/1e3)
	default:
		return fmt.Sprintf("€%.0f", v)
	}
}

// navChart renders a stacked-area (cash + investments) net-worth chart as inline
// SVG. The top edge of the stack is total NAV. Returns "" for <2 points (the
// caller shows an empty state). Output is app-generated numbers/dates only, so
// it is safe as template.HTML, and contains no <script> (CSP-compliant).
func navChart(s ChartSeries) template.HTML {
	n := len(s.Total)
	if n < 2 {
		return ""
	}
	const w, h = 760.0, 240.0
	const padL, padR, padT, padB = 58.0, 14.0, 12.0, 28.0
	plotW, plotH := w-padL-padR, h-padT-padB

	yMax, yMin := s.Total[0], 0.0
	for _, v := range s.Total {
		if v > yMax {
			yMax = v
		}
		if v < yMin {
			yMin = v
		}
	}
	if yMax <= yMin {
		yMax = yMin + 1
	}
	x := func(i int) float64 { return padL + float64(i)/float64(n-1)*plotW }
	y := func(v float64) float64 { return padT + (yMax-v)/(yMax-yMin)*plotH }
	base := y(0)

	var b strings.Builder
	b.WriteString(`<svg class="navchart" viewBox="0 0 760 240" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Net worth history">`)

	// horizontal gridlines + € labels
	for i := 0; i <= 3; i++ {
		gv := yMin + (yMax-yMin)*float64(i)/3
		gy := y(gv)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#e6e0d4"/>`, padL, gy, w-padR, gy)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="#726c61" font-size="10" text-anchor="end">%s</text>`, padL-6, gy+3, shortEUR(gv))
	}
	// cash area (baseline → cash line)
	b.WriteString(`<polygon fill="#a9cfc4" fill-opacity="0.9" points="`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%.1f,%.1f ", x(i), y(s.Cash[i]))
	}
	for i := n - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "%.1f,%.1f ", x(i), base)
	}
	b.WriteString(`"/>`)
	// investments area (cash line → total line, stacked)
	b.WriteString(`<polygon fill="#14463f" fill-opacity="0.85" points="`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%.1f,%.1f ", x(i), y(s.Total[i]))
	}
	for i := n - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "%.1f,%.1f ", x(i), y(s.Cash[i]))
	}
	b.WriteString(`"/>`)
	// total line
	b.WriteString(`<polyline fill="none" stroke="#0c2a25" stroke-width="1.5" points="`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%.1f,%.1f ", x(i), y(s.Total[i]))
	}
	b.WriteString(`"/>`)
	// first / last date labels
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="#726c61" font-size="10" text-anchor="start">%s</text>`, padL, h-9, template.HTMLEscapeString(s.Labels[0]))
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="#726c61" font-size="10" text-anchor="end">%s</text>`, w-padR, h-9, template.HTMLEscapeString(s.Labels[n-1]))

	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// ForecastSeries is plain data for the net-worth forecast chart: two NAV lines
// (planned vs actual saving rate) over the same months, plus a target level.
type ForecastSeries struct {
	Labels  []string
	Planned []float64
	Actual  []float64
	Target  float64
}

// forecastChart renders the NAV forecast as inline SVG: a planned line, an actual
// line, and a dashed horizontal target with a marker where the planned line first
// reaches it. Returns "" for <2 points. App-generated numbers only, no <script>
// (CSP-compliant), so it is safe as template.HTML.
func forecastChart(s ForecastSeries) template.HTML {
	n := len(s.Planned)
	if n < 2 {
		return ""
	}
	const w, h = 760.0, 240.0
	const padL, padR, padT, padB = 58.0, 14.0, 12.0, 28.0
	plotW, plotH := w-padL-padR, h-padT-padB

	yMax := 0.0
	for i := 0; i < n; i++ {
		if s.Planned[i] > yMax {
			yMax = s.Planned[i]
		}
		if i < len(s.Actual) && s.Actual[i] > yMax {
			yMax = s.Actual[i]
		}
	}
	if s.Target > yMax {
		yMax = s.Target
	}
	if yMax <= 0 {
		yMax = 1
	}
	yMax *= 1.05
	x := func(i int) float64 { return padL + float64(i)/float64(n-1)*plotW }
	y := func(v float64) float64 { return padT + (yMax-v)/yMax*plotH }

	var b strings.Builder
	b.WriteString(`<svg class="navchart" viewBox="0 0 760 240" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Net worth forecast">`)
	for i := 0; i <= 3; i++ {
		gv := yMax * float64(i) / 3
		gy := y(gv)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#e6e0d4"/>`, padL, gy, w-padR, gy)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="#726c61" font-size="10" text-anchor="end">%s</text>`, padL-6, gy+3, shortEUR(gv))
	}
	// target dashed line + marker where the planned line first reaches it
	if s.Target > 0 {
		ty := y(s.Target)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#b06a3b" stroke-width="1.2" stroke-dasharray="5 3"/>`, padL, ty, w-padR, ty)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="#b06a3b" font-size="10" text-anchor="start">target %s</text>`, padL+4, ty-4, shortEUR(s.Target))
		for i := 0; i < n; i++ {
			if s.Planned[i] >= s.Target {
				fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3.5" fill="#14463f"/>`, x(i), y(s.Planned[i]))
				break
			}
		}
	}
	// actual line (muted, dashed)
	if len(s.Actual) == n {
		b.WriteString(`<polyline fill="none" stroke="#a9947c" stroke-width="1.5" stroke-dasharray="4 2" points="`)
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "%.1f,%.1f ", x(i), y(s.Actual[i]))
		}
		b.WriteString(`"/>`)
	}
	// planned line (accent)
	b.WriteString(`<polyline fill="none" stroke="#14463f" stroke-width="2" points="`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%.1f,%.1f ", x(i), y(s.Planned[i]))
	}
	b.WriteString(`"/>`)
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="#726c61" font-size="10" text-anchor="start">%s</text>`, padL, h-9, template.HTMLEscapeString(s.Labels[0]))
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="#726c61" font-size="10" text-anchor="end">%s</text>`, w-padR, h-9, template.HTMLEscapeString(s.Labels[n-1]))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// BurnSeries is plain data for the cash burn-down chart: a running balance per
// month and the lump outflow (if any) scheduled that month.
type BurnSeries struct {
	Labels  []string
	Balance []float64
	Spend   []float64
}

// burndownChart renders the cash burn-down as inline SVG: a single balance line
// that steps down at scheduled spends (marked with dots), a zero baseline, and a
// shaded "below zero" region when the balance would run out. Returns "" for <2
// points. App-generated numbers only, no <script> (CSP-compliant).
func burndownChart(s BurnSeries) template.HTML {
	n := len(s.Balance)
	if n < 2 {
		return ""
	}
	const w, h = 760.0, 240.0
	const padL, padR, padT, padB = 58.0, 14.0, 12.0, 28.0
	plotW, plotH := w-padL-padR, h-padT-padB

	yMax, yMin := 0.0, 0.0
	for _, v := range s.Balance {
		if v > yMax {
			yMax = v
		}
		if v < yMin {
			yMin = v
		}
	}
	if yMax <= yMin {
		yMax = yMin + 1
	}
	x := func(i int) float64 { return padL + float64(i)/float64(n-1)*plotW }
	y := func(v float64) float64 { return padT + (yMax-v)/(yMax-yMin)*plotH }

	var b strings.Builder
	b.WriteString(`<svg class="navchart" viewBox="0 0 760 240" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Cash burn-down">`)
	for i := 0; i <= 3; i++ {
		gv := yMin + (yMax-yMin)*float64(i)/3
		gy := y(gv)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#e6e0d4"/>`, padL, gy, w-padR, gy)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="#726c61" font-size="10" text-anchor="end">%s</text>`, padL-6, gy+3, shortEUR(gv))
	}
	// zero baseline, emphasised when the balance dips negative
	if yMin < 0 {
		zy := y(0)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#b5403a" stroke-width="1"/>`, padL, zy, w-padR, zy)
	}
	// balance line
	b.WriteString(`<polyline fill="none" stroke="#14463f" stroke-width="2" points="`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%.1f,%.1f ", x(i), y(s.Balance[i]))
	}
	b.WriteString(`"/>`)
	// spend markers
	if len(s.Spend) == n {
		for i := 0; i < n; i++ {
			if s.Spend[i] > 0 {
				fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3.5" fill="#b06a3b"/>`, x(i), y(s.Balance[i]))
			}
		}
	}
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="#726c61" font-size="10" text-anchor="start">%s</text>`, padL, h-9, template.HTMLEscapeString(s.Labels[0]))
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="#726c61" font-size="10" text-anchor="end">%s</text>`, w-padR, h-9, template.HTMLEscapeString(s.Labels[n-1]))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// DonutSeg is one slice of the allocation donut.
type DonutSeg struct {
	Label string
	Value float64
	Color string
}

// donut renders a proportional ring (asset allocation) as inline SVG. Segments
// with non-positive values are skipped; returns "" when the total is zero.
// App-generated numbers only, no <script> (CSP-compliant).
func donut(segs []DonutSeg) template.HTML {
	total := 0.0
	for _, s := range segs {
		if s.Value > 0 {
			total += s.Value
		}
	}
	if total <= 0 {
		return ""
	}
	const size, sw = 112.0, 16.0
	r := (size - sw) / 2
	circ := 2 * math.Pi * r
	cx := size / 2
	var b strings.Builder
	// Explicit width/height (not just viewBox) so the ring can't balloon to the
	// container width if the stylesheet is stale/unavailable.
	fmt.Fprintf(&b, `<svg class="donut" width="%g" height="%g" viewBox="0 0 %g %g" role="img" aria-label="Asset allocation">`, size, size, size, size)
	fmt.Fprintf(&b, `<circle cx="%g" cy="%g" r="%.2f" fill="none" stroke="#e6e0d4" stroke-width="%g"/>`, cx, cx, r, sw)
	offset := 0.0
	for _, s := range segs {
		if s.Value <= 0 {
			continue
		}
		seg := s.Value / total * circ
		fmt.Fprintf(&b, `<circle cx="%g" cy="%g" r="%.2f" fill="none" stroke="%s" stroke-width="%g" stroke-linecap="butt" stroke-dasharray="%.2f %.2f" stroke-dashoffset="%.2f" transform="rotate(-90 %g %g)"/>`,
			cx, cx, r, s.Color, sw, seg, circ-seg, -offset, cx, cx)
		offset += seg
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// sparkline renders a tiny trend line as inline SVG; it inherits color via
// currentColor (so a .pos / .neg class on the parent tints it). Returns "" for
// <2 points. App-generated numbers only, no <script> (CSP-compliant).
func sparkline(vals []float64) template.HTML {
	n := len(vals)
	if n < 2 {
		return ""
	}
	const w, h = 104.0, 30.0
	lo, hi := vals[0], vals[0]
	for _, v := range vals {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	if hi <= lo {
		hi = lo + 1
	}
	x := func(i int) float64 { return float64(i) / float64(n-1) * w }
	y := func(v float64) float64 { return h - 2 - (v-lo)/(hi-lo)*(h-4) }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="spark" viewBox="0 0 %g %g" preserveAspectRatio="none" aria-hidden="true">`, w, h)
	b.WriteString(`<polyline fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round" points="`)
	for i, v := range vals {
		fmt.Fprintf(&b, "%.1f,%.1f ", x(i), y(v))
	}
	b.WriteString(`"/></svg>`)
	return template.HTML(b.String())
}

// pct returns part/whole as an integer percentage clamped to [0,100], for
// progress-bar widths (used in inline style, which the CSP permits).
func pct(part, whole float64) int {
	if whole <= 0 {
		return 0
	}
	p := int(part / whole * 100)
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

type View struct {
	tmpls map[string]*template.Template
}

// New parses all templates; it panics on a malformed template, surfacing
// template bugs at startup rather than at request time.
func New() *View {
	out := make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		out[p] = template.Must(template.New("layout.html").Funcs(funcMap).
			ParseFS(templateFS, "templates/layout.html", "templates/"+p))
	}
	return &View{tmpls: out}
}

// Render executes the named page (wrapped in layout.html) with data, buffering
// first so a mid-render error doesn't emit a half-written response.
func (v *View) Render(w http.ResponseWriter, name string, data any) error {
	t, ok := v.tmpls[name]
	if !ok {
		return errUnknownTemplate(name)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout.html", data); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := buf.WriteTo(w)
	return err
}

type errUnknownTemplate string

func (e errUnknownTemplate) Error() string { return "view: unknown template " + string(e) }

// StaticHandler serves the embedded static assets under /static/.
func (v *View) StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	// Embedded assets have no mod-time, so revalidate each load — this keeps the
	// stylesheet from going stale in the browser after a UI change.
	noCache := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
	return http.StripPrefix("/static/", noCache)
}

// ---------- display funcs ----------

func eur(val float64) string {
	// A NaN/Inf (e.g. from a division upstream) has no "." to split on and
	// would panic, failing the whole page render; show a dash instead.
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return "\u20ac—"
	}
	neg := val < 0
	s := strconv.FormatFloat(math.Abs(val), 'f', 2, 64) // Abs also clears a -0
	if s == "0.00" {
		neg = false // float leftovers like -0.001 show as €0.00, not "-€0.00"
	}
	dot := strings.IndexByte(s, '.')
	intPart, frac := s[:dot], s[dot+1:]
	var b strings.Builder
	n := len(intPart)
	for i := 0; i < n; i++ {
		if i > 0 && (n-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(intPart[i])
	}
	res := "\u20ac" + b.String() + "." + frac
	if neg {
		res = "-" + res
	}
	return res
}

func signClass(val float64) string {
	// Under half a cent either way is zero, matching what eur displays, so a
	// float leftover like -0.001 isn't coloured as a loss.
	switch {
	case val >= 0.005:
		return "pos"
	case val <= -0.005:
		return "neg"
	default:
		return "zero"
	}
}
