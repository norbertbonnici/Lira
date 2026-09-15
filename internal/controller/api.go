package controller

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lira/internal/model"
)

// ---------- JSON API (v1) ----------
//
// A read+write JSON API for native clients (e.g. the iOS app). It is a separate
// surface from the no-JS HTML app: authenticated by a per-user bearer token
// (Authorization: Bearer <token>) rather than the session cookie/CSRF, and it
// emits/accepts JSON. Tokens are minted on the /tokens page. Per-user scoping
// (pocket money) is honoured via the token's owner.

// apiHandler is an API endpoint that already knows the authenticated user.
type apiHandler func(http.ResponseWriter, *http.Request, *model.User)

// APIAuth authenticates a bearer token and passes the owning user to next.
func (c *Controller) APIAuth(next apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, prefix) {
			apiError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		tok := strings.TrimSpace(auth[len(prefix):])
		if tok == "" {
			apiError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		u, err := c.store.UserByAPITokenHash(hashToken(tok))
		if err != nil || u == nil {
			apiError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next(w, r, u)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// decodeJSON reads a JSON request body (capped at 1 MB) into dst.
func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(dst)
}

func (c *Controller) rateFunc() func(string) float64 {
	rates, _ := c.store.GetFXRates()
	return func(ccy string) float64 {
		if v, ok := rates[strings.ToUpper(ccy)]; ok && v > 0 {
			return v
		}
		return 1
	}
}

func nptr(n sql.NullInt64) any {
	if n.Valid {
		return n.Int64
	}
	return nil
}

// ---------- read endpoints ----------

func (c *Controller) APIMe(w http.ResponseWriter, r *http.Request, u *model.User) {
	hh, _ := c.store.GetHousehold()
	out := map[string]any{"user": map[string]any{"id": u.ID, "username": u.Username, "role": u.Role}}
	if hh != nil {
		out["household"] = map[string]any{
			"name": hh.Name, "monthlyIncome": hh.MonthlyIncome, "monthlySavings": hh.MonthlySavings,
		}
	}
	writeJSON(w, 200, out)
}

func (c *Controller) APISummary(w http.ResponseWriter, r *http.Request, u *model.User) {
	cash, inv, total, err := c.store.ComputeNAV()
	if err != nil {
		apiError(w, 500, "nav error")
		return
	}
	income, operating, projectFunding, available := c.budgetSummary()
	writeJSON(w, 200, map[string]any{
		"netWorth":    total,
		"cash":        cash,
		"investments": inv,
		"budget": map[string]any{
			"income": income, "operatingCosts": operating,
			"projectFunding": projectFunding, "available": available,
		},
	})
}

func (c *Controller) APIAccounts(w http.ResponseWriter, r *http.Request, u *model.User) {
	accts, _ := c.store.ListAccounts()
	out := make([]map[string]any, 0, len(accts))
	for _, a := range accts {
		out = append(out, map[string]any{
			"id": a.ID, "name": a.Name, "type": a.Type, "currency": a.Currency,
			"openingBalance": a.OpeningBalance, "balance": a.Balance, "spending": a.Spending,
		})
	}
	writeJSON(w, 200, out)
}

func (c *Controller) APITransactions(w http.ResponseWriter, r *http.Request, u *model.User) {
	limit := 100
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}
	if limit > 1000 {
		limit = 1000
	}
	txs, _ := c.store.ListTransactions(limit)
	out := make([]map[string]any, 0, len(txs))
	for _, t := range txs {
		out = append(out, map[string]any{
			"id": t.ID, "date": t.Date, "amount": t.Amount, "payee": t.Payee, "notes": t.Notes,
			"accountId": t.AccountID, "account": t.AccountName, "category": t.CategoryName,
			"categoryId": nptr(t.CategoryID), "transfer": t.TransferID.Valid,
		})
	}
	writeJSON(w, 200, out)
}

func (c *Controller) APIHoldings(w http.ResponseWriter, r *http.Request, u *model.User) {
	holds, _ := c.store.ListHoldings()
	out := make([]map[string]any, 0, len(holds))
	for _, h := range holds {
		var cost any
		if h.CostBasis.Valid {
			cost = h.CostBasis.Float64
		}
		out = append(out, map[string]any{
			"id": h.ID, "symbol": h.Symbol, "name": h.Name, "kind": h.Kind,
			"quantity": h.Quantity, "price": h.Price, "currency": h.Currency,
			"value": h.Value, "costBasis": cost,
		})
	}
	writeJSON(w, 200, out)
}

func (c *Controller) APIProjects(w http.ResponseWriter, r *http.Request, u *model.User) {
	ps, _ := c.store.ListProjects()
	out := make([]map[string]any, 0, len(ps))
	for _, p := range ps {
		out = append(out, map[string]any{
			"id": p.ID, "name": p.Name, "status": p.Status, "targetDate": p.TargetDate,
			"financeMethod": p.FinanceMethod, "budget": model.ProjectTarget(p.ItemsTotal, p.BufferPct),
			"spent": p.Spent, "loanStartDate": p.LoanStartDate, "depositPaid": p.DepositPaid,
		})
	}
	writeJSON(w, 200, out)
}

func (c *Controller) APICosts(w http.ResponseWriter, r *http.Request, u *model.User) {
	costs, _ := c.store.ListOperatingCosts()
	rate := c.rateFunc()
	out := make([]map[string]any, 0, len(costs))
	for _, oc := range costs {
		out = append(out, map[string]any{
			"id": oc.ID, "name": oc.Name, "category": oc.Category, "amount": oc.Amount,
			"currency": oc.Currency, "frequency": oc.Frequency, "active": oc.Active,
			"monthlyEUR": model.MonthlyAmount(oc.Amount, oc.Frequency) * rate(oc.Currency),
		})
	}
	writeJSON(w, 200, out)
}

func (c *Controller) APIPocket(w http.ResponseWriter, r *http.Request, u *model.User) {
	entries, _ := c.store.ListPocketEntries(u.ID)
	balance, _ := c.store.PocketBalance(u.ID)
	rows := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, map[string]any{
			"id": e.ID, "date": e.Date, "amount": e.Amount, "category": e.Category, "note": e.Note,
		})
	}
	writeJSON(w, 200, map[string]any{"balance": balance, "entries": rows})
}

// ---------- write endpoints ----------

func (c *Controller) APICreateTransaction(w http.ResponseWriter, r *http.Request, u *model.User) {
	var in struct {
		AccountID  int64   `json:"accountId"`
		Date       string  `json:"date"`
		Amount     float64 `json:"amount"`
		CategoryID *int64  `json:"categoryId"`
		Payee      string  `json:"payee"`
		Notes      string  `json:"notes"`
	}
	if err := decodeJSON(r, &in); err != nil {
		apiError(w, 400, "invalid JSON")
		return
	}
	if in.AccountID == 0 || in.Date == "" || in.Amount == 0 {
		apiError(w, 422, "accountId, date and a non-zero amount are required")
		return
	}
	if err := c.store.CreateTransaction(in.AccountID, in.Date, in.Amount, in.CategoryID, strings.TrimSpace(in.Payee), strings.TrimSpace(in.Notes)); err != nil {
		apiError(w, 500, "database error")
		return
	}
	writeJSON(w, 201, map[string]any{"ok": true})
}

func (c *Controller) APIDeleteTransaction(w http.ResponseWriter, r *http.Request, u *model.User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := c.store.DeleteTransaction(id); err != nil {
		apiError(w, 500, "database error")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (c *Controller) APICreatePocket(w http.ResponseWriter, r *http.Request, u *model.User) {
	var in struct {
		Date     string  `json:"date"`
		Amount   float64 `json:"amount"`
		Category string  `json:"category"`
		Note     string  `json:"note"`
	}
	if err := decodeJSON(r, &in); err != nil {
		apiError(w, 400, "invalid JSON")
		return
	}
	if in.Amount == 0 {
		apiError(w, 422, "a non-zero amount is required (+ top-up, − spend)")
		return
	}
	date := strings.TrimSpace(in.Date)
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	if err := c.store.AddPocketEntry(u.ID, date, in.Amount, strings.TrimSpace(in.Category), strings.TrimSpace(in.Note)); err != nil {
		apiError(w, 500, "database error")
		return
	}
	writeJSON(w, 201, map[string]any{"ok": true})
}

func (c *Controller) APIDeletePocket(w http.ResponseWriter, r *http.Request, u *model.User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := c.store.DeletePocketEntry(u.ID, id); err != nil {
		apiError(w, 500, "database error")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (c *Controller) APICreateAccount(w http.ResponseWriter, r *http.Request, u *model.User) {
	var in struct {
		Name           string  `json:"name"`
		Type           string  `json:"type"`
		Currency       string  `json:"currency"`
		OpeningBalance float64 `json:"openingBalance"`
		Spending       bool    `json:"spending"`
	}
	if err := decodeJSON(r, &in); err != nil {
		apiError(w, 400, "invalid JSON")
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		apiError(w, 422, "name is required")
		return
	}
	if in.Type == "" {
		in.Type = "savings"
	}
	if in.Currency == "" {
		in.Currency = "EUR"
	}
	if err := c.store.CreateAccount(strings.TrimSpace(in.Name), in.Type, strings.ToUpper(in.Currency), in.OpeningBalance, in.Spending); err != nil {
		apiError(w, 500, "database error")
		return
	}
	writeJSON(w, 201, map[string]any{"ok": true})
}

func (c *Controller) APICreateHolding(w http.ResponseWriter, r *http.Request, u *model.User) {
	var in struct {
		Symbol    string   `json:"symbol"`
		Name      string   `json:"name"`
		Kind      string   `json:"kind"`
		Quantity  float64  `json:"quantity"`
		Price     float64  `json:"price"`
		Currency  string   `json:"currency"`
		CostBasis *float64 `json:"costBasis"`
	}
	if err := decodeJSON(r, &in); err != nil {
		apiError(w, 400, "invalid JSON")
		return
	}
	if strings.TrimSpace(in.Symbol) == "" {
		apiError(w, 422, "symbol is required")
		return
	}
	if in.Kind == "" {
		in.Kind = "stock"
	}
	if in.Currency == "" {
		in.Currency = "EUR"
	}
	if err := c.store.CreateHolding(strings.ToUpper(strings.TrimSpace(in.Symbol)), strings.TrimSpace(in.Name), in.Kind, in.Quantity, in.Price, strings.ToUpper(in.Currency), in.CostBasis); err != nil {
		apiError(w, 500, "database error")
		return
	}
	writeJSON(w, 201, map[string]any{"ok": true})
}
