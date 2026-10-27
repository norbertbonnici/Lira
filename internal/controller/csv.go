package controller

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"lira/internal/model"
)

const csvMaxBytes = 5 << 20 // 5 MB

// dateLayouts offered for import (Go reference-time layouts).
var dateLayouts = []struct{ Value, Label string }{
	{"2006-01-02", "YYYY-MM-DD (2026-01-31)"},
	{"02/01/2006", "DD/MM/YYYY (31/01/2026)"},
	{"01/02/2006", "MM/DD/YYYY (01/31/2026)"},
	{"02.01.2006", "DD.MM.YYYY (31.01.2026)"},
	{"02-01-2006", "DD-MM-YYYY (31-01-2026)"},
	{"2006/01/02", "YYYY/MM/DD"},
}

func delimRune(s string) rune {
	switch s {
	case "semicolon":
		return ';'
	case "tab":
		return '\t'
	default:
		return ','
	}
}

// csvRecords parses CSV text with the chosen delimiter, tolerating ragged rows.
func csvRecords(data, delim string) ([][]string, error) {
	rd := csv.NewReader(strings.NewReader(data))
	rd.Comma = delimRune(delim)
	rd.FieldsPerRecord = -1
	return rd.ReadAll()
}

// ---------- pure parse helpers ----------

// parseCSVDate parses value with the given Go layout and returns YYYY-MM-DD.
func parseCSVDate(value, layout string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || layout == "" {
		return "", false
	}
	t, err := time.Parse(layout, value)
	if err != nil {
		return "", false
	}
	return t.Format("2006-01-02"), true
}

// parseCSVAmount parses a monetary string. With decimalComma, ',' is the decimal
// separator and '.' is thousands; otherwise the reverse. Accepts currency symbols,
// a 3-letter currency code, spaces/apostrophes as digit grouping, a leading or
// trailing minus (ASCII or U+2212), parentheses (accounting negative) and a DR/CR
// suffix (DR = money out).
//
// It refuses anything it would otherwise have to guess at, rather than import a
// wrong number: a thousands separator not followed by exactly 3 digits ("12,50"
// without decimalComma — almost certainly a decimal comma, which would otherwise
// become 1250), a separator after the decimal point ("1.234,56" without
// decimalComma), stray letters ("1e3"), or more than one sign.
func parseCSVAmount(value string, decimalComma bool) (float64, bool) {
	s := strings.TrimSpace(value)
	neg := false
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		neg, s = true, strings.TrimSpace(s[1:len(s)-1])
	}
	// Peel a trailing then a leading run of ASCII letters: a DR/CR marker or a
	// 3-letter currency code. Any other letters make the value unparseable.
	trail := len(s)
	for trail > 0 && isASCIILetter(s[trail-1]) {
		trail--
	}
	switch suffix := strings.ToUpper(s[trail:]); {
	case suffix == "DR":
		neg = true
	case suffix == "" || suffix == "CR" || len(suffix) == 3:
	default:
		return 0, false
	}
	s = s[:trail]
	lead := 0
	for lead < len(s) && isASCIILetter(s[lead]) {
		lead++
	}
	if lead != 0 && lead != 3 {
		return 0, false
	}
	s = s[lead:]

	var b strings.Builder
	for _, r := range s {
		switch {
		case (r >= '0' && r <= '9') || r == '.' || r == ',' || r == '+' || r == '-':
			b.WriteRune(r)
		case r == '\u2212': // typographic minus
			b.WriteByte('-')
		case unicode.IsSpace(r) || r == '\'' || r == '\u2019' || unicode.Is(unicode.Sc, r):
			// digit grouping or a currency symbol
		default:
			return 0, false
		}
	}
	num := b.String()
	if n := len(num); n > 0 && (num[0] == '-' || num[0] == '+') {
		neg = neg || num[0] == '-'
		num = num[1:]
	} else if n > 0 && num[n-1] == '-' { // "12.00-"
		neg, num = true, num[:n-1]
	}
	if strings.ContainsAny(num, "+-") {
		return 0, false
	}

	dec, grp := ".", ","
	if decimalComma {
		dec, grp = ",", "."
	}
	intPart, frac, hasDec := strings.Cut(num, dec)
	if strings.Contains(frac, dec) || strings.Contains(frac, grp) {
		return 0, false
	}
	if strings.Contains(intPart, grp) {
		groups := strings.Split(intPart, grp)
		if len(groups[0]) < 1 || len(groups[0]) > 3 {
			return 0, false
		}
		for _, g := range groups[1:] {
			if len(g) != 3 {
				return 0, false
			}
		}
		intPart = strings.Join(groups, "")
	}
	if intPart == "" && frac == "" {
		return 0, false
	}
	if hasDec {
		intPart += "." + frac
	}
	f, err := strconv.ParseFloat(intPart, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		f = -f
	}
	return f, true
}

func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// blankAmount reports an empty amount cell, or the lone-dash placeholder some
// banks put in the unused debit/credit column.
func blankAmount(s string) bool {
	switch s {
	case "", "-", "\u2013", "\u2014":
		return true
	}
	return false
}

// ---------- export ----------

func writeCSV(w http.ResponseWriter, filename string, header []string, rows [][]string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	cw := csv.NewWriter(w)
	_ = cw.Write(header)
	_ = cw.WriteAll(rows)
	cw.Flush()
}

func f2(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

func (c *Controller) ExportTransactions(w http.ResponseWriter, r *http.Request) {
	txs, _ := c.store.ListTransactions(1_000_000)
	rows := make([][]string, 0, len(txs))
	for _, t := range txs {
		rows = append(rows, []string{t.Date, t.AccountName, f2(t.Amount), t.CategoryName, t.Payee, t.Notes})
	}
	writeCSV(w, "transactions.csv", []string{"date", "account", "amount", "category", "payee", "notes"}, rows)
}

func (c *Controller) ExportHoldings(w http.ResponseWriter, r *http.Request) {
	holds, _ := c.store.ListHoldings()
	rows := make([][]string, 0, len(holds))
	for _, h := range holds {
		cost := ""
		if h.CostBasis.Valid {
			cost = f2(h.CostBasis.Float64)
		}
		rows = append(rows, []string{h.Symbol, h.Name, h.Kind, strconv.FormatFloat(h.Quantity, 'f', -1, 64),
			f2(h.Price), h.Currency, cost, f2(h.Value)})
	}
	writeCSV(w, "holdings.csv", []string{"symbol", "name", "kind", "quantity", "price", "currency", "cost_basis", "value"}, rows)
}

func (c *Controller) ExportCosts(w http.ResponseWriter, r *http.Request) {
	costs, _ := c.store.ListOperatingCosts()
	rows := make([][]string, 0, len(costs))
	for _, oc := range costs {
		active := "yes"
		if !oc.Active {
			active = "no"
		}
		rows = append(rows, []string{oc.Name, oc.Category, f2(oc.Amount), oc.Currency, oc.Frequency,
			strconv.Itoa(oc.DueDay), active, f2(model.MonthlyAmount(oc.Amount, oc.Frequency))})
	}
	writeCSV(w, "operating_costs.csv", []string{"name", "category", "amount", "currency", "frequency", "due_day", "active", "monthly"}, rows)
}

// ---------- import (upload → map → commit) ----------

func (c *Controller) ImportUpload(w http.ResponseWriter, r *http.Request) {
	accts, _ := c.store.ListAccounts()
	c.render(w, r, "import_upload.html", map[string]any{"Accounts": accts})
}

func (c *Controller) uploadError(w http.ResponseWriter, r *http.Request, msg string) {
	accts, _ := c.store.ListAccounts()
	c.render(w, r, "import_upload.html", map[string]any{"Accounts": accts, "Error": msg})
}

func (c *Controller) ImportPreview(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if err := r.ParseMultipartForm(csvMaxBytes); err != nil {
		c.uploadError(w, r, "Upload too large or malformed (max 5 MB).")
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		c.uploadError(w, r, "Please choose a CSV file.")
		return
	}
	defer file.Close()
	data, _ := io.ReadAll(io.LimitReader(file, csvMaxBytes))
	delim := r.FormValue("delim")
	records, err := csvRecords(string(data), delim)
	if err != nil || len(records) == 0 {
		c.uploadError(w, r, "Couldn't read that CSV — check the delimiter.")
		return
	}

	hasHeader := r.FormValue("has_header") != ""
	dataStart := 0
	var headers []string
	if hasHeader {
		headers, dataStart = records[0], 1
	} else {
		for i := range records[0] {
			headers = append(headers, fmt.Sprintf("Column %d", i+1))
		}
	}
	type col struct {
		Index int
		Label string
	}
	cols := make([]col, 0, len(headers))
	for i, h := range headers {
		cols = append(cols, col{i, h})
	}
	preview := records[dataStart:]
	if len(preview) > 5 {
		preview = preview[:5]
	}
	c.render(w, r, "import_map.html", map[string]any{
		"Columns":     cols,
		"Preview":     preview,
		"CSV":         string(data),
		"HasHeader":   hasHeader,
		"Delim":       delim,
		"AccountID":   r.FormValue("account_id"),
		"DateLayouts": dateLayouts,
		"RowCount":    len(records) - dataStart,
	})
}

func (c *Controller) ImportCommit(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	records, err := csvRecords(r.FormValue("csv"), r.FormValue("delim"))
	if err != nil || len(records) == 0 {
		http.Redirect(w, r, "/transactions/import", http.StatusSeeOther)
		return
	}
	accountID, _ := strconv.ParseInt(r.FormValue("account_id"), 10, 64)
	layout := r.FormValue("date_layout")
	decimalComma := r.FormValue("decimal_comma") != ""
	debitCredit := r.FormValue("amount_mode") == "debitcredit"
	col := func(name string) int {
		v, e := strconv.Atoi(r.FormValue(name))
		if e != nil {
			return -1
		}
		return v
	}
	dateCol, amountCol := col("date_col"), col("amount_col")
	debitCol, creditCol := col("debit_col"), col("credit_col")
	payeeCol, catCol, notesCol := col("payee_col"), col("category_col"), col("notes_col")

	cats, _ := c.store.ListCategories()
	catByName := map[string]int64{}
	for _, ct := range cats {
		catByName[strings.ToLower(ct.Name)] = ct.ID
	}

	// Duplicate detection: skip rows already in the account (and within this file).
	skipDupes := r.FormValue("skip_dupes") != ""
	seen := map[string]bool{}
	if skipDupes {
		if keys, err := c.store.AccountTxKeys(accountID); err == nil {
			seen = keys
		}
	}

	get := func(row []string, i int) string {
		if i >= 0 && i < len(row) {
			return strings.TrimSpace(row[i])
		}
		return ""
	}
	dataStart := 0
	if r.FormValue("has_header") != "" {
		dataStart = 1
	}
	var imported, skipped int
	reasons := map[string]int{}
	for _, row := range records[dataStart:] {
		if len(row) == 0 {
			continue
		}
		dateStr, ok := parseCSVDate(get(row, dateCol), layout)
		if !ok {
			skipped++
			reasons["Unparseable or missing date"]++
			continue
		}
		var amount float64
		var aok, bad bool // bad: a non-blank amount cell that didn't parse
		if debitCredit {
			if d := get(row, debitCol); !blankAmount(d) {
				if v, vok := parseCSVAmount(d, decimalComma); vok {
					amount, aok = amount-absf(v), true
				} else {
					bad = true
				}
			}
			if cr := get(row, creditCol); !blankAmount(cr) {
				if v, vok := parseCSVAmount(cr, decimalComma); vok {
					amount, aok = amount+absf(v), true
				} else {
					bad = true
				}
			}
		} else if a := get(row, amountCol); !blankAmount(a) {
			amount, aok = parseCSVAmount(a, decimalComma)
			bad = !aok
		}
		if bad {
			skipped++
			reasons["Unparseable amount (check the comma-decimals option)"]++
			continue
		}
		if !aok || amount == 0 {
			skipped++
			reasons["Zero or blank amount"]++
			continue
		}
		payee := get(row, payeeCol)
		if skipDupes {
			k := model.TxDedupKey(dateStr, amount, payee)
			if seen[k] {
				skipped++
				reasons["Duplicate (already in account)"]++
				continue
			}
			seen[k] = true
		}
		var catID *int64
		if catCol >= 0 {
			if id, found := catByName[strings.ToLower(get(row, catCol))]; found {
				v := id
				catID = &v
			}
		}
		if err := c.store.CreateTransaction(accountID, dateStr, amount, catID, payee, get(row, notesCol)); err == nil {
			imported++
		} else {
			skipped++
			reasons["Database error"]++
		}
	}

	type reasonRow struct {
		Reason string
		Count  int
	}
	var rrows []reasonRow
	for k, v := range reasons {
		rrows = append(rrows, reasonRow{k, v})
	}
	sort.Slice(rrows, func(i, j int) bool { return rrows[i].Reason < rrows[j].Reason })

	c.render(w, r, "import_result.html", map[string]any{
		"Imported": imported,
		"Skipped":  skipped,
		"Reasons":  rrows,
	})
}
