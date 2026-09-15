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
// separator and '.' is thousands; otherwise the reverse. Strips currency symbols
// and spaces, and treats parenthesised values as negative (accounting style).
func parseCSVAmount(value string, decimalComma bool) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	neg := false
	if strings.HasPrefix(value, "(") && strings.HasSuffix(value, ")") {
		neg, value = true, value[1:len(value)-1]
	}
	var b strings.Builder
	for _, r := range value {
		if (r >= '0' && r <= '9') || r == '.' || r == ',' || r == '-' || r == '+' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if decimalComma {
		s = strings.ReplaceAll(s, ".", "")
		s = strings.ReplaceAll(s, ",", ".")
	} else {
		s = strings.ReplaceAll(s, ",", "")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		f = -f
	}
	return f, true
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
		var aok bool
		if debitCredit {
			if d := get(row, debitCol); d != "" {
				if v, vok := parseCSVAmount(d, decimalComma); vok {
					amount, aok = amount-absf(v), true
				}
			}
			if cr := get(row, creditCol); cr != "" {
				if v, vok := parseCSVAmount(cr, decimalComma); vok {
					amount, aok = amount+absf(v), true
				}
			}
		} else {
			amount, aok = parseCSVAmount(get(row, amountCol), decimalComma)
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
