// Package controller is the HTTP layer: request handling, authentication,
// sessions, and CSRF. It depends on model (data) and view (presentation).
package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"lira/internal/config"
	"lira/internal/model"
	"lira/internal/view"
)

type Controller struct {
	store   *model.Store
	view    *view.View
	cfg     config.Config
	refresh func(context.Context) error // market data fetch; nil when disabled
}

func New(store *model.Store, v *view.View, cfg config.Config) *Controller {
	return &Controller{store: store, view: v, cfg: cfg}
}

// SetRefresh wires a market-data refresh function (prices + FX). When set, the
// holdings page shows a "Refresh prices" button.
func (c *Controller) SetRefresh(fn func(context.Context) error) { c.refresh = fn }

// render injects the current user and a CSRF token, then delegates to the view.
func (c *Controller) render(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	if _, ok := data["User"]; !ok {
		data["User"] = currentUser(r)
	}
	data["CSRF"] = c.ensureCSRF(w, r)
	if err := c.view.Render(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// ---------- parse helpers ----------

// parseDecimal parses a human-entered number, tolerating both European
// (decimal comma) and US (decimal point, comma thousands) notation, and rejects
// non-finite values (NaN/Inf). A lone comma before exactly three digits is read
// as a thousands group ("1,234" → 1234); any other lone comma is the decimal
// mark ("1,5" → 1.5), so European input isn't silently scaled by 10/100.
func parseDecimal(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty")
	}
	// Drop spaces used as thousands separators ("1 234,56"); keep everything else
	// so ParseFloat still rejects stray letters (a typo like "12o0" must not parse).
	s = strings.ReplaceAll(s, " ", "")
	lastDot := strings.LastIndexByte(s, '.')
	lastComma := strings.LastIndexByte(s, ',')
	switch {
	case lastDot >= 0 && lastComma >= 0:
		// Both present: the rightmost separator is the decimal mark.
		if lastComma > lastDot {
			s = strings.ReplaceAll(s, ".", "")  // dots group thousands
			s = strings.ReplaceAll(s, ",", ".") // comma is the decimal mark
		} else {
			s = strings.ReplaceAll(s, ",", "") // commas group thousands
		}
	case lastComma >= 0:
		if strings.Count(s, ",") == 1 && len(s)-lastComma-1 != 3 {
			s = strings.Replace(s, ",", ".", 1) // decimal comma
		} else {
			s = strings.ReplaceAll(s, ",", "") // thousands grouping
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, errors.New("not a finite number")
	}
	return f, nil
}

// parseFloat parses a number leniently, returning 0 on blank/invalid input. Use
// formMoney where an unparseable value must be rejected rather than zeroed.
func parseFloat(s string) float64 {
	f, _ := parseDecimal(s)
	return f
}

// formMoney parses a money field; ok is false only when the field is non-empty
// but unparseable, so callers can reject bad input instead of silently storing 0.
func formMoney(s string) (v float64, ok bool) {
	if strings.TrimSpace(s) == "" {
		return 0, true
	}
	f, err := parseDecimal(s)
	if err != nil {
		return 0, false
	}
	return f, true
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func nullableID(s string) *int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return nil
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	return &id
}

func nullableFloat(s string) *float64 {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	f, err := parseDecimal(s)
	if err != nil {
		return nil
	}
	return &f
}

// validateHolding returns a user-facing error for a holding form, or "" if the
// symbol is present and the quantity/price parsed cleanly.
func validateHolding(symbol string, qtyOK, priceOK bool) string {
	if symbol == "" {
		return "Symbol is required."
	}
	if !qtyOK || !priceOK {
		return "Enter a valid quantity and price."
	}
	return ""
}

func absf(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// ---------- Dashboard ----------

func (c *Controller) Dashboard(w http.ResponseWriter, r *http.Request) {
	cash, inv, total, err := c.store.ComputeNAV()
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	accts, _ := c.store.ListAccounts()
	txs, _ := c.store.ListTransactions(8)
	holds, _ := c.store.ListHoldings()
	active, _ := c.store.ListActiveProjects()
	snaps, _ := c.store.ListNavSnapshots(60)
	bIncome, bOperating, bSavings, bAvailable := c.budgetSummary()
	dueCount, dueTotal := c.billsDue(7)
	budgetOver, budgetLow, _ := c.budgetAlerts(time.Now())
	now := time.Now()
	data := map[string]any{
		"NAV":             total,
		"Cash":            cash,
		"Investments":     inv,
		"Accounts":        accts,
		"Transactions":    txs,
		"Holdings":        holds,
		"Projects":        c.projectCards(active, now),
		"Chart":           navSeries(snaps),
		"HasHistory":      len(snaps) >= 2,
		"BudgetIncome":    bIncome,
		"BudgetOperating": bOperating,
		"BudgetSavings":   bSavings,
		"BudgetAvailable": bAvailable,
		"HasBudget":       bIncome > 0 || bOperating > 0,
		"BillsDueCount":   dueCount,
		"BillsDueTotal":   dueTotal,
		"BudgetOver":      budgetOver,
		"BudgetLow":       budgetLow,
	}
	// % of income committed to costs + project funding (for the budget bar).
	if bIncome > 0 {
		data["AllocatedPct"] = int((bOperating+bSavings)/bIncome*100 + 0.5)
	}
	for k, v := range c.dashboardExtras(now, cash, inv, total, holds, snaps) {
		data[k] = v
	}
	c.render(w, r, "dashboard.html", data)
}

// ---------- Accounts ----------

func (c *Controller) Accounts(w http.ResponseWriter, r *http.Request) {
	accts, _ := c.store.ListAccounts()
	var total, nwTotal float64
	for _, a := range accts {
		total += a.Balance
		if !a.Spending {
			nwTotal += a.Balance
		}
	}
	c.render(w, r, "accounts.html", map[string]any{"Accounts": accts, "Total": total, "NetWorthTotal": nwTotal})
}

func (c *Controller) AccountNew(w http.ResponseWriter, r *http.Request) {
	c.render(w, r, "account_form.html", map[string]any{
		"Account": &model.Account{Currency: "EUR", Type: "savings"},
		"Action":  "/accounts",
		"Heading": "New account",
	})
}

func (c *Controller) AccountCreate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		c.render(w, r, "account_form.html", map[string]any{
			"Account": &model.Account{Name: name, Type: r.FormValue("type"), Currency: r.FormValue("currency")},
			"Action":  "/accounts", "Heading": "New account", "Error": "Name is required.",
		})
		return
	}
	if err := c.store.CreateAccount(name, r.FormValue("type"), strings.ToUpper(r.FormValue("currency")),
		parseFloat(r.FormValue("opening_balance")), r.FormValue("spending") != ""); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/accounts", http.StatusSeeOther)
}

func (c *Controller) AccountEdit(w http.ResponseWriter, r *http.Request) {
	acct, err := c.store.GetAccount(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c.render(w, r, "account_form.html", map[string]any{
		"Account": acct,
		"Action":  "/accounts/" + strconv.FormatInt(acct.ID, 10),
		"Heading": "Edit account",
	})
}

func (c *Controller) AccountUpdate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	id := pathID(r)
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Redirect(w, r, "/accounts/"+strconv.FormatInt(id, 10)+"/edit", http.StatusSeeOther)
		return
	}
	if err := c.store.UpdateAccount(id, name, r.FormValue("type"), strings.ToUpper(r.FormValue("currency")),
		parseFloat(r.FormValue("opening_balance")), r.FormValue("spending") != ""); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/accounts", http.StatusSeeOther)
}

// ---------- Transactions ----------

func (c *Controller) Transactions(w http.ResponseWriter, r *http.Request) {
	txs, _ := c.store.ListTransactions(250)
	c.render(w, r, "transactions.html", map[string]any{"Transactions": txs})
}

// txFormData builds the transaction form context, prefilled from tx (nil = new).
func (c *Controller) txFormData(tx *model.Transaction, action, heading, errMsg string) map[string]any {
	accts, _ := c.store.ListAccounts()
	cats, _ := c.store.ListCategories()
	data := map[string]any{
		"Accounts": accts, "Categories": cats,
		"Action": action, "Heading": heading, "Error": errMsg,
		"IsExpense": true, "DateVal": time.Now().Format("2006-01-02"),
		"AmountStr": "", "SelAccount": int64(0), "SelCategory": int64(0),
		"Payee": "", "Notes": "",
	}
	if tx != nil {
		data["IsExpense"] = tx.Amount < 0
		if tx.Date != "" {
			data["DateVal"] = tx.Date
		}
		if tx.Amount != 0 {
			data["AmountStr"] = strconv.FormatFloat(absf(tx.Amount), 'f', 2, 64)
		}
		data["SelAccount"] = tx.AccountID
		if tx.CategoryID.Valid {
			data["SelCategory"] = tx.CategoryID.Int64
		}
		data["Payee"], data["Notes"] = tx.Payee, tx.Notes
	}
	return data
}

// txFromForm builds a Transaction from the posted form (signed amount).
func txFromForm(r *http.Request) *model.Transaction {
	amount := absf(parseFloat(r.FormValue("amount")))
	if r.FormValue("type") == "expense" {
		amount = -amount
	}
	acctID, _ := strconv.ParseInt(r.FormValue("account_id"), 10, 64)
	tx := &model.Transaction{
		AccountID: acctID,
		Date:      strings.TrimSpace(r.FormValue("date")),
		Amount:    amount,
		Payee:     strings.TrimSpace(r.FormValue("payee")),
		Notes:     strings.TrimSpace(r.FormValue("notes")),
	}
	if cid := nullableID(r.FormValue("category_id")); cid != nil {
		tx.CategoryID = sql.NullInt64{Int64: *cid, Valid: true}
	}
	return tx
}

func (c *Controller) TransactionNew(w http.ResponseWriter, r *http.Request) {
	c.render(w, r, "transaction_form.html", c.txFormData(nil, "/transactions", "New transaction", ""))
}

func (c *Controller) TransactionCreate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	tx := txFromForm(r)
	if tx.AccountID == 0 || tx.Amount == 0 || tx.Date == "" {
		c.render(w, r, "transaction_form.html", c.txFormData(tx, "/transactions", "New transaction",
			"Account, a non-zero amount and a date are required."))
		return
	}
	if err := c.store.CreateTransaction(tx.AccountID, tx.Date, tx.Amount,
		nullableID(r.FormValue("category_id")), tx.Payee, tx.Notes); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/transactions", http.StatusSeeOther)
}

func (c *Controller) TransactionEdit(w http.ResponseWriter, r *http.Request) {
	tx, err := c.store.GetTransaction(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if tx.TransferID.Valid { // transfer legs are edited by deleting + re-adding
		http.Redirect(w, r, "/transactions", http.StatusSeeOther)
		return
	}
	c.render(w, r, "transaction_form.html",
		c.txFormData(tx, "/transactions/"+strconv.FormatInt(tx.ID, 10), "Edit transaction", ""))
}

func (c *Controller) TransactionUpdate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	id := pathID(r)
	if existing, err := c.store.GetTransaction(id); err == nil && existing.TransferID.Valid {
		http.Redirect(w, r, "/transactions", http.StatusSeeOther)
		return
	}
	tx := txFromForm(r)
	if tx.AccountID == 0 || tx.Amount == 0 || tx.Date == "" {
		c.render(w, r, "transaction_form.html", c.txFormData(tx, "/transactions/"+strconv.FormatInt(id, 10),
			"Edit transaction", "Account, a non-zero amount and a date are required."))
		return
	}
	if err := c.store.UpdateTransaction(id, tx.AccountID, tx.Date, tx.Amount,
		nullableID(r.FormValue("category_id")), tx.Payee, tx.Notes); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/transactions", http.StatusSeeOther)
}

// ---------- Transfers ----------

func (c *Controller) TransferNew(w http.ResponseWriter, r *http.Request) {
	accts, _ := c.store.ListAccounts()
	c.render(w, r, "transfer_form.html", map[string]any{"Accounts": accts})
}

func (c *Controller) TransferCreate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	fromID, _ := strconv.ParseInt(r.FormValue("from_id"), 10, 64)
	toID, _ := strconv.ParseInt(r.FormValue("to_id"), 10, 64)
	amount := absf(parseFloat(r.FormValue("amount")))
	date := strings.TrimSpace(r.FormValue("date"))
	fail := func(msg string) {
		accts, _ := c.store.ListAccounts()
		c.render(w, r, "transfer_form.html", map[string]any{"Accounts": accts, "Error": msg})
	}
	if fromID == 0 || toID == 0 || fromID == toID || amount == 0 || date == "" {
		fail("Pick two different accounts, a non-zero amount and a date.")
		return
	}
	from, ferr := c.store.GetAccount(fromID)
	to, terr := c.store.GetAccount(toID)
	if ferr != nil || terr != nil {
		fail("Pick two different accounts, a non-zero amount and a date.")
		return
	}
	// Same currency: what leaves arrives. Different currencies: the same number
	// on both legs would be wrong (100 EUR out, 100 USD in shifts net worth by
	// the FX gap), so the amount received in the destination currency is required.
	received := amount
	if !strings.EqualFold(from.Currency, to.Currency) {
		received = absf(parseFloat(r.FormValue("received")))
		if received == 0 {
			fail(fmt.Sprintf("%s is in %s and %s is in %s — enter the amount received in %s too.",
				from.Name, from.Currency, to.Name, to.Currency, to.Currency))
			return
		}
	}
	if err := c.store.CreateTransfer(fromID, toID, date, amount, received, strings.TrimSpace(r.FormValue("notes"))); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/transactions", http.StatusSeeOther)
}

func (c *Controller) TransactionDelete(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	_ = c.store.DeleteTransaction(pathID(r))
	http.Redirect(w, r, "/transactions", http.StatusSeeOther)
}

// ---------- Holdings ----------

func (c *Controller) Holdings(w http.ResponseWriter, r *http.Request) {
	holds, _ := c.store.ListHoldings()
	rates, _ := c.store.GetFXRates()
	rate := func(ccy string) float64 {
		if r, ok := rates[strings.ToUpper(ccy)]; ok && r > 0 {
			return r
		}
		return 1
	}
	var total, totalGain, totalCost float64 // EUR
	rows := make([]holdingRow, 0, len(holds))
	subtot := map[string]*currencyTotal{} // per-currency value, native + EUR
	anyFX := false
	for _, x := range holds {
		fx := rate(x.Currency)
		ccy := strings.ToUpper(x.Currency)
		if ccy == "" {
			ccy = "EUR"
		}
		if ccy != "EUR" {
			anyFX = true
		}
		row := holdingRow{Holding: x}
		total += x.Value * fx
		ct := subtot[ccy]
		if ct == nil {
			ct = &currencyTotal{Currency: ccy}
			subtot[ccy] = ct
		}
		ct.Value += x.Value
		ct.ValueEUR += x.Value * fx
		if x.CostBasis.Valid && x.CostBasis.Float64 > 0 {
			gain, pct := model.HoldingGain(x.Value, x.CostBasis.Float64)
			row.GainEUR, row.GainPct, row.HasGain = gain*fx, pct, true
			totalGain += row.GainEUR
			totalCost += x.CostBasis.Float64 * fx
		}
		rows = append(rows, row)
	}
	// Per-currency subtotals (sorted), shown only when a conversion actually
	// happens, so the EUR grand total reconciles with the native per-row values.
	currencies := make([]currencyTotal, 0, len(subtot))
	for _, ct := range subtot {
		currencies = append(currencies, *ct)
	}
	sort.Slice(currencies, func(i, j int) bool { return currencies[i].Currency < currencies[j].Currency })
	var totalGainPct float64
	if totalCost > 0 {
		totalGainPct = totalGain / totalCost * 100
	}
	updated, _ := c.store.LatestHoldingUpdate()
	if len(updated) >= 10 {
		updated = updated[:10]
	}
	c.render(w, r, "holdings.html", map[string]any{
		"Rows":         rows,
		"Currencies":   currencies,
		"ShowFX":       anyFX,
		"Total":        total,
		"TotalGain":    totalGain,
		"TotalGainPct": totalGainPct,
		"HasGains":     totalCost > 0,
		"Updated":      updated,
		"Refreshable":  c.refresh != nil,
	})
}

// currencyTotal is a per-currency value subtotal: the sum in the holding's own
// currency plus its EUR conversion, so the Investments table shows each currency
// before the final EUR total.
type currencyTotal struct {
	Currency string
	Value    float64 // native sum
	ValueEUR float64
}

// holdingRow is a holding plus its EUR-converted unrealized gain.
type holdingRow struct {
	model.Holding
	GainEUR float64
	GainPct float64
	HasGain bool
}

func (c *Controller) HoldingNew(w http.ResponseWriter, r *http.Request) {
	c.render(w, r, "holding_form.html", map[string]any{
		"Holding": &model.Holding{Kind: "stock", Currency: "EUR"},
		"Action":  "/holdings",
		"Heading": "New holding",
	})
}

func (c *Controller) HoldingCreate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(r.FormValue("symbol")))
	qty, qok := formMoney(r.FormValue("quantity"))
	price, pok := formMoney(r.FormValue("price"))
	if msg := validateHolding(symbol, qok, pok); msg != "" {
		c.render(w, r, "holding_form.html", map[string]any{
			"Holding": &model.Holding{Symbol: symbol, Name: strings.TrimSpace(r.FormValue("name")),
				Kind: r.FormValue("kind"), Currency: r.FormValue("currency")},
			"Action": "/holdings", "Heading": "New holding", "Error": msg,
		})
		return
	}
	if err := c.store.CreateHolding(symbol, strings.TrimSpace(r.FormValue("name")),
		r.FormValue("kind"), qty, price,
		strings.ToUpper(r.FormValue("currency")), nullableFloat(r.FormValue("cost_basis"))); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/holdings", http.StatusSeeOther)
}

func (c *Controller) HoldingEdit(w http.ResponseWriter, r *http.Request) {
	hd, err := c.store.GetHolding(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c.render(w, r, "holding_form.html", map[string]any{
		"Holding": hd,
		"Action":  "/holdings/" + strconv.FormatInt(hd.ID, 10),
		"Heading": "Edit holding",
	})
}

func (c *Controller) HoldingUpdate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	id := pathID(r)
	symbol := strings.ToUpper(strings.TrimSpace(r.FormValue("symbol")))
	qty, qok := formMoney(r.FormValue("quantity"))
	price, pok := formMoney(r.FormValue("price"))
	if validateHolding(symbol, qok, pok) != "" {
		http.Redirect(w, r, "/holdings/"+strconv.FormatInt(id, 10)+"/edit", http.StatusSeeOther)
		return
	}
	if err := c.store.UpdateHolding(id, symbol, strings.TrimSpace(r.FormValue("name")),
		r.FormValue("kind"), qty, price,
		strings.ToUpper(r.FormValue("currency")), nullableFloat(r.FormValue("cost_basis"))); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/holdings", http.StatusSeeOther)
}
