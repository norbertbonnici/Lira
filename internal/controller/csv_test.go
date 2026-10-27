package controller

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"lira/internal/model"
)

func TestParseCSVDate(t *testing.T) {
	cases := []struct {
		val, layout, want string
		ok                bool
	}{
		{"2026-01-31", "2006-01-02", "2026-01-31", true},
		{"31/01/2026", "02/01/2006", "2026-01-31", true},
		{"01/31/2026", "01/02/2006", "2026-01-31", true},
		{"31.01.2026", "02.01.2006", "2026-01-31", true},
		{"not a date", "2006-01-02", "", false},
		{"", "2006-01-02", "", false},
	}
	for _, c := range cases {
		got, ok := parseCSVDate(c.val, c.layout)
		if ok != c.ok || got != c.want {
			t.Errorf("parseCSVDate(%q,%q) = %q,%v want %q,%v", c.val, c.layout, got, ok, c.want, c.ok)
		}
	}
}

func TestParseCSVAmount(t *testing.T) {
	cases := []struct {
		val          string
		decimalComma bool
		want         float64
		ok           bool
	}{
		{"1234.56", false, 1234.56, true},
		{"-1,234.56", false, -1234.56, true}, // thousands comma
		{"1.234,56", true, 1234.56, true},    // European
		{"€ 99,90", true, 99.90, true},       // currency symbol + comma decimal
		{"(50.00)", false, -50, true},        // accounting negative
		{"", false, 0, false},
		{"abc", false, 0, false},
		// Accepted spellings.
		{"1,234,567.89", false, 1234567.89, true},
		{"1.234", true, 1234, true},               // thousands dot
		{"1 234,56", true, 1234.56, true},         // space grouping
		{"1'234.56", false, 1234.56, true},        // Swiss grouping
		{"\u221212.00", false, -12, true},         // typographic minus
		{"12.00-", false, -12, true},              // trailing minus
		{"12.00 DR", false, -12, true},            // debit marker
		{"12.00 cr", false, 12, true},             // credit marker
		{"EUR -5.50", false, -5.5, true},          // leading currency code
		{"€-5,50", true, -5.5, true},              // symbol before the sign
		{"7.25 USD", false, 7.25, true},           // trailing currency code
		{"(\u20ac 1,200.00)", false, -1200, true}, // parens around symbol
		{".50", false, 0.5, true},
		// Ambiguous or garbled: refuse rather than guess.
		{"12,50", false, 0, false},    // decimal comma with the option off (was 1250)
		{"0,99", false, 0, false},     // ditto
		{"12.50", true, 0, false},     // decimal dot with the option on (was 1250)
		{"1.234,56", false, 0, false}, // European with the option off (was 1.23456)
		{"1,234.56", true, 0, false},  // US with the option on (was 1.23456)
		{"1,2345.00", false, 0, false},
		{"1e3", false, 0, false}, // was 13
		{"12.3.4", false, 0, false},
		{"--12", false, 0, false},
		{"1+2", false, 0, false},
		{"-12-", false, 0, false},
		{"12 EURO", false, 0, false},
		{"€", false, 0, false},
		{"EUR", false, 0, false},
		{"n/a", false, 0, false},
	}
	for _, c := range cases {
		got, ok := parseCSVAmount(c.val, c.decimalComma)
		if ok != c.ok || (ok && (got < c.want-0.001 || got > c.want+0.001)) {
			t.Errorf("parseCSVAmount(%q,%v) = %v,%v want %v,%v", c.val, c.decimalComma, got, ok, c.want, c.ok)
		}
	}
}

// An import with a decimal-comma file and the option left off must skip the
// ambiguous rows as unparseable, never book them 100x too large; a lone "-" in
// the unused debit/credit column still counts as blank.
func TestImportCommitRejectsAmbiguousAmounts(t *testing.T) {
	c, store := testController(t)
	_ = store.CreateAccount("Current", "checking", "EUR", 0, false)
	accts, _ := store.ListAccounts()
	acct := strconv.FormatInt(accts[0].ID, 10)

	commit := func(csvText string, extra url.Values) *httptest.ResponseRecorder {
		t.Helper()
		v := url.Values{"csv": {csvText}, "delim": {","}, "account_id": {acct},
			"date_layout": {"2006-01-02"}, "date_col": {"0"}, "payee_col": {"1"}}
		for k, vals := range extra {
			v[k] = vals
		}
		w := httptest.NewRecorder()
		c.ImportCommit(w, postForm("/transactions/import/commit", v))
		return w
	}

	w := commit("2026-10-01,Coffee,\"-3,50\"\n2026-10-02,Shop,\"-1,234.00\"\n", url.Values{"amount_col": {"2"}})
	if !strings.Contains(w.Body.String(), "Unparseable amount") {
		t.Errorf("result page doesn't report the unparseable row")
	}
	txs, _ := store.ListTransactions(10)
	if len(txs) != 1 || txs[0].Amount != -1234 {
		t.Fatalf("imported %+v, want only the unambiguous -1234 row", txs)
	}

	commit("2026-10-03,Salary,-,\"2,000.00\"\n2026-10-04,Rent,900.00,-\n",
		url.Values{"amount_mode": {"debitcredit"}, "debit_col": {"2"}, "credit_col": {"3"}})
	got := map[string]float64{}
	txs, _ = store.ListTransactions(10)
	for _, tx := range txs {
		got[tx.Payee] = tx.Amount
	}
	if got["Salary"] != 2000 || got["Rent"] != -900 {
		t.Errorf("debit/credit import = %v, want Salary 2000 / Rent -900 (dash = blank)", got)
	}
}

// multipartUpload builds a CSRF-valid import preview POST carrying `body` as the file.
func multipartUpload(t *testing.T, body string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("csrf_token", "tok")
	fw, _ := mw.CreateFormFile("file", "bank.csv")
	_, _ = io.WriteString(fw, body)
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/transactions/import/preview", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: "tok"})
	return withUser(r, &model.Session{MFAPassed: true}, &model.User{ID: 1, Username: "a", Role: "admin"})
}

// A file over the 5 MB cap is refused with a message — never silently cut at
// 5 MB (which dropped every later row, possibly mid-amount).
func TestImportPreviewRejectsOversizedFile(t *testing.T) {
	c, _ := testController(t)
	row := "2026-10-01,Coffee,-3.50\n"
	big := strings.Repeat(row, csvMaxBytes/len(row)+10)

	w := httptest.NewRecorder()
	c.ImportPreview(w, multipartUpload(t, big))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "over 5 MB") {
		t.Errorf("oversized upload: status %d, want the over-5-MB message", w.Code)
	}

	w = httptest.NewRecorder()
	c.ImportPreview(w, multipartUpload(t, "Date,Payee,Amount\n"+row))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "over 5 MB") || !strings.Contains(w.Body.String(), "Coffee") {
		t.Errorf("small upload: status %d, want the mapping preview", w.Code)
	}
}

func TestCSVSafe(t *testing.T) {
	cases := map[string]string{
		"=HYPERLINK(\"http://x\",\"hi\")": "'=HYPERLINK(\"http://x\",\"hi\")",
		"+cmd|' /C calc'!A0":              "'+cmd|' /C calc'!A0",
		"-2+3":                            "'-2+3",
		"@SUM(A1)":                        "'@SUM(A1)",
		"\tTab":                           "'\tTab",
		"-12.50":                          "-12.50", // amounts stay numeric
		"+3":                              "+3",
		"12.50":                           "12.50",
		"Lidl":                            "Lidl",
		"":                                "",
		"-":                               "'-",
		"1.2.3":                           "1.2.3", // starts with a digit: not a formula
	}
	for in, want := range cases {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q want %q", in, got, want)
		}
	}
}

// Exports neutralise formula-looking payees but keep negative amounts numeric.
func TestExportTransactionsEscapesFormulas(t *testing.T) {
	c, store := testController(t)
	_ = store.CreateAccount("Bank", "checking", "EUR", 0, false)
	accts, _ := store.ListAccounts()
	_ = store.CreateTransaction(accts[0].ID, "2026-10-01", -12.5, nil, `=HYPERLINK("http://evil","x")`, "@note")
	w := httptest.NewRecorder()
	c.ExportTransactions(w, httptest.NewRequest(http.MethodGet, "/export/transactions.csv", nil))
	body := w.Body.String()
	if !strings.Contains(body, `"'=HYPERLINK(""http://evil"",""x"")"`) || !strings.Contains(body, "'@note") {
		t.Errorf("formula cells not escaped:\n%s", body)
	}
	if !strings.Contains(body, ",-12.50,") {
		t.Errorf("amount should stay a plain number:\n%s", body)
	}
}

// Duplicate detection counts copies: genuine repeats in a file are imported,
// only as many as the account already holds are skipped, and re-importing the
// same file adds nothing.
func TestImportCommitCountsDuplicates(t *testing.T) {
	c, store := testController(t)
	_ = store.CreateAccount("Current", "checking", "EUR", 0, false)
	accts, _ := store.ListAccounts()
	acct := strconv.FormatInt(accts[0].ID, 10)
	_ = store.CreateTransaction(accts[0].ID, "2026-10-01", -3.5, nil, "Cafe", "") // entered by hand

	file := "2026-10-01,Cafe,-3.50\n2026-10-01,Cafe,-3.50\n2026-10-02,Shop,-20.00\n"
	commit := func() {
		v := url.Values{"csv": {file}, "delim": {","}, "account_id": {acct}, "date_layout": {"2006-01-02"},
			"date_col": {"0"}, "payee_col": {"1"}, "amount_col": {"2"}, "skip_dupes": {"1"}}
		c.ImportCommit(httptest.NewRecorder(), postForm("/transactions/import/commit", v))
	}
	count := func() map[string]int {
		n := map[string]int{}
		txs, _ := store.ListTransactions(100)
		for _, tx := range txs {
			n[tx.Payee]++
		}
		return n
	}
	commit()
	if n := count(); n["Cafe"] != 2 || n["Shop"] != 1 {
		t.Errorf("after first import: %v want Cafe 2 (1 by hand + 1 new), Shop 1", n)
	}
	commit()
	if n := count(); n["Cafe"] != 2 || n["Shop"] != 1 {
		t.Errorf("re-importing the same file added rows: %v", n)
	}
}
