package model

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

// ---------- Filtered transaction search ----------

// TxFilter narrows SearchTransactions. Zero values mean "no constraint".
type TxFilter struct {
	From, To         string // inclusive YYYY-MM-DD
	AccountID        int64
	CategoryID       int64
	Uncategorized    bool    // only rows with no category
	Query            string  // case-insensitive substring of payee or notes
	MinAbs, MaxAbs   float64 // bounds on |amount| (MaxAbs 0 = unbounded)
	Kind             string  // "income" (amount>0) | "expense" (amount<0) | ""
	IncludeTransfers bool    // transfer legs are excluded unless set
	Limit, Offset    int     // Limit <= 0 = no limit
}

func (f TxFilter) where() (string, []any) {
	var conds []string
	var args []any
	add := func(c string, a ...any) { conds = append(conds, c); args = append(args, a...) }
	if f.From != "" {
		add("t.date >= ?", f.From)
	}
	if f.To != "" {
		add("t.date <= ?", f.To)
	}
	if f.AccountID > 0 {
		add("t.account_id = ?", f.AccountID)
	}
	if f.CategoryID > 0 {
		add("t.category_id = ?", f.CategoryID)
	}
	if f.Uncategorized {
		add("t.category_id IS NULL")
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
		like := "%" + esc + "%"
		add(`(t.payee LIKE ? ESCAPE '\' OR t.notes LIKE ? ESCAPE '\')`, like, like)
	}
	if f.MinAbs > 0 {
		add("ABS(t.amount) >= ?", f.MinAbs)
	}
	if f.MaxAbs > 0 {
		add("ABS(t.amount) <= ?", f.MaxAbs)
	}
	switch f.Kind {
	case "income":
		add("t.amount > 0")
	case "expense":
		add("t.amount < 0")
	}
	if !f.IncludeTransfers {
		add("t.transfer_id IS NULL")
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// SearchTransactions returns transactions matching f, newest first, with the
// account name/currency and category name/kind joined in.
func (s *Store) SearchTransactions(f TxFilter) ([]Transaction, error) {
	where, args := f.where()
	q := `SELECT t.id, t.account_id, t.date, t.amount, t.category_id, t.payee, t.notes, t.project_id, t.transfer_id,
	             a.name, a.currency, COALESCE(c.name,''), COALESCE(c.kind,''),
	             t.project_item_id, t.deposit_project_id, COALESCE(p.name,''), COALESCE(pi.name,'')
	      FROM transactions t
	      JOIN accounts a ON a.id = t.account_id
	      LEFT JOIN categories c ON c.id = t.category_id
	      LEFT JOIN projects p ON p.id = COALESCE(t.project_id, t.deposit_project_id)
	      LEFT JOIN project_items pi ON pi.id = t.project_item_id` + where + `
	      ORDER BY t.date DESC, t.id DESC`
	if f.Limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, f.Limit, max(f.Offset, 0))
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transaction
	for rows.Next() {
		var t Transaction
		if err := rows.Scan(&t.ID, &t.AccountID, &t.Date, &t.Amount, &t.CategoryID, &t.Payee, &t.Notes,
			&t.ProjectID, &t.TransferID, &t.AccountName, &t.AccountCurrency, &t.CategoryName, &t.CategoryKind,
			&t.ProjectItemID, &t.DepositProjectID, &t.ProjectName, &t.ItemName); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CountTransactions returns how many rows SearchTransactions would return
// without Limit/Offset (for pagination).
func (s *Store) CountTransactions(f TxFilter) (int, error) {
	where, args := f.where()
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM transactions t`+where, args...).Scan(&n)
	return n, err
}

// ---------- Ledger audits (pure) ----------
//
// These read a slice of transactions and return findings. They never modify
// anything — they exist so an assistant (or a future UI) can point at likely
// bookkeeping mistakes for a human to fix.

// NormalizePayee folds a bank payee string into a comparable key: lower-case
// letters only, whitespace-collapsed, so "CARD 1234 LIDL MT 03/11" and
// "Lidl MT" share "card lidl mt"/"lidl mt"-style keys that drop card numbers
// and dates. Pure.
func NormalizePayee(p string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(p) {
		if unicode.IsLetter(r) {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

// DuplicateGroup is a set of transactions that look like the same real-world
// payment recorded more than once.
type DuplicateGroup struct {
	AccountID   int64
	AccountName string
	Amount      float64
	Payee       string
	Dates       []string
	IDs         []int64
}

// FindDuplicates groups non-transfer transactions on the same account with the
// same amount (to the cent) and payee (case-insensitive, as TxDedupKey) whose
// dates fall within windowDays of the previous one in the cluster. windowDays 0
// means the same date only (exactly what CSV import dedups on); a small window
// (1–3) also catches a bank re-exporting a row under its value date. Groups are
// returned newest first. Pure.
func FindDuplicates(txs []Transaction, windowDays int) []DuplicateGroup {
	type key struct {
		acct  int64
		cents int64
		payee string
	}
	buckets := map[key][]Transaction{}
	for _, t := range txs {
		if t.TransferID.Valid {
			continue
		}
		k := key{t.AccountID, int64(math.Round(t.Amount * 100)), strings.ToLower(strings.TrimSpace(t.Payee))}
		buckets[k] = append(buckets[k], t)
	}
	var out []DuplicateGroup
	for _, b := range buckets {
		if len(b) < 2 {
			continue
		}
		sort.Slice(b, func(i, j int) bool {
			if b[i].Date != b[j].Date {
				return b[i].Date < b[j].Date
			}
			return b[i].ID < b[j].ID
		})
		flush := func(cl []Transaction) {
			if len(cl) < 2 {
				return
			}
			g := DuplicateGroup{AccountID: cl[0].AccountID, AccountName: cl[0].AccountName, Amount: cl[0].Amount, Payee: cl[0].Payee}
			for _, t := range cl {
				g.Dates = append(g.Dates, t.Date)
				g.IDs = append(g.IDs, t.ID)
			}
			out = append(out, g)
		}
		cluster := []Transaction{b[0]}
		for _, t := range b[1:] {
			if daysBetween(cluster[len(cluster)-1].Date, t.Date) <= windowDays {
				cluster = append(cluster, t)
				continue
			}
			flush(cluster)
			cluster = []Transaction{t}
		}
		flush(cluster)
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := out[i].Dates[len(out[i].Dates)-1], out[j].Dates[len(out[j].Dates)-1]
		if di != dj {
			return di > dj
		}
		return out[i].IDs[0] > out[j].IDs[0]
	})
	return out
}

// daysBetween returns |b − a| in whole days for YYYY-MM-DD dates (a large
// number when either fails to parse, so bad dates never cluster).
func daysBetween(a, b string) int {
	ta, err1 := time.Parse("2006-01-02", a)
	tb, err2 := time.Parse("2006-01-02", b)
	if err1 != nil || err2 != nil {
		return math.MaxInt32
	}
	d := int(math.Round(tb.Sub(ta).Hours() / 24))
	if d < 0 {
		d = -d
	}
	return d
}

// UncategorizedGroup is the uncategorized transactions sharing a normalized
// payee, with the category that payee is most often filed under elsewhere.
type UncategorizedGroup struct {
	Payee       string // a representative raw payee
	Count       int
	Total       float64 // signed sum
	IDs         []int64
	SuggestedID int64 // 0 = no suggestion
	Suggested   string
	// Confidence is the share of this payee's categorized history (same sign)
	// that uses the suggested category; Basis is how many rows that history has.
	Confidence float64
	Basis      int
}

// Suggestion thresholds: a category is suggested (for a payee, or as a
// project's fallback hint) only when its history has at least
// SuggestMinHistory categorized rows and the winning category covers at least
// SuggestMinConfidence of them — so one or two stray rows can't put a
// confident-looking suggestion in front of a model or a person.
const (
	SuggestMinHistory    = 3
	SuggestMinConfidence = 0.6
)

// confidentSuggestion reports whether best of n rows clears the thresholds.
func confidentSuggestion(best, n int) bool {
	return n >= SuggestMinHistory && float64(best) >= SuggestMinConfidence*float64(n)
}

// SuggestCategories groups the uncategorized, non-transfer transactions by
// normalized payee and sign, and suggests the category most often used for the
// same payee and sign among the categorized rows, when that history clears the
// suggestion thresholds. Groups are ordered by size, then |total|. Pure.
func SuggestCategories(txs []Transaction) []UncategorizedGroup {
	type key struct {
		payee string
		pos   bool
	}
	type hist struct {
		counts map[int64]int
		names  map[int64]string
		n      int
	}
	history := map[key]*hist{}
	groups := map[key]*UncategorizedGroup{}
	var order []key
	for _, t := range txs {
		if t.TransferID.Valid {
			continue
		}
		k := key{NormalizePayee(t.Payee), t.Amount > 0}
		if t.CategoryID.Valid {
			if k.payee == "" {
				continue
			}
			h := history[k]
			if h == nil {
				h = &hist{counts: map[int64]int{}, names: map[int64]string{}}
				history[k] = h
			}
			h.counts[t.CategoryID.Int64]++
			h.names[t.CategoryID.Int64] = t.CategoryName
			h.n++
			continue
		}
		g := groups[k]
		if g == nil {
			g = &UncategorizedGroup{Payee: t.Payee}
			groups[k] = g
			order = append(order, k)
		}
		g.Count++
		g.Total += t.Amount
		g.IDs = append(g.IDs, t.ID)
	}
	out := make([]UncategorizedGroup, 0, len(order))
	for _, k := range order {
		g := groups[k]
		if h := history[k]; h != nil && h.n > 0 {
			var best int64
			bestN := 0
			for id, n := range h.counts {
				if n > bestN || (n == bestN && id < best) {
					best, bestN = id, n
				}
			}
			if confidentSuggestion(bestN, h.n) {
				g.SuggestedID, g.Suggested = best, h.names[best]
				g.Confidence = math.Round(float64(bestN)/float64(h.n)*100) / 100
				g.Basis = h.n
			}
		}
		out = append(out, *g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return math.Abs(out[i].Total) > math.Abs(out[j].Total)
	})
	return out
}

// TxProject is the project a transaction is linked to (item payment, loan
// deposit, or plain project tag), 0 = none. Pure.
func TxProject(t Transaction) int64 {
	switch {
	case t.ProjectID.Valid:
		return t.ProjectID.Int64
	case t.DepositProjectID.Valid:
		return t.DepositProjectID.Int64
	}
	return 0
}

// ProjectHint is the category most often used by a project's categorized
// transactions of one sign: a fallback suggestion for its uncategorized ones.
type ProjectHint struct {
	CategoryID int64
	Category   string
	Confidence float64 // share of that history using CategoryID
	Basis      int     // categorized rows in that history
}

// ProjectHints maps project id → sign (true = positive) → the most-used
// category among that project's categorized, non-transfer transactions, when
// that history clears the suggestion thresholds. Pure.
func ProjectHints(txs []Transaction) map[int64]map[bool]ProjectHint {
	type key struct {
		project int64
		pos     bool
	}
	counts := map[key]map[int64]int{}
	names := map[int64]string{}
	totals := map[key]int{}
	for _, t := range txs {
		pid := TxProject(t)
		if pid == 0 || t.TransferID.Valid || !t.CategoryID.Valid {
			continue
		}
		k := key{pid, t.Amount > 0}
		if counts[k] == nil {
			counts[k] = map[int64]int{}
		}
		counts[k][t.CategoryID.Int64]++
		names[t.CategoryID.Int64] = t.CategoryName
		totals[k]++
	}
	out := map[int64]map[bool]ProjectHint{}
	for k, cs := range counts {
		var best int64
		bestN := 0
		for id, n := range cs {
			if n > bestN || (n == bestN && id < best) {
				best, bestN = id, n
			}
		}
		if !confidentSuggestion(bestN, totals[k]) {
			continue
		}
		if out[k.project] == nil {
			out[k.project] = map[bool]ProjectHint{}
		}
		out[k.project][k.pos] = ProjectHint{
			CategoryID: best, Category: names[best], Basis: totals[k],
			Confidence: math.Round(float64(bestN)/float64(totals[k])*100) / 100,
		}
	}
	return out
}

// Anomaly is one transaction that looks like a bookkeeping mistake.
type Anomaly struct {
	Tx     Transaction
	Kind   string // sign_mismatch | outlier | future_dated | zero_amount
	Detail string
}

// Outlier thresholds: a payee needs outlierMinHistory rows before its typical
// amount is trusted, and a row must exceed outlierFactor × the median AND be
// at least outlierMinGap above it, so small habitual amounts don't trip it.
const (
	outlierMinHistory = 4
	outlierFactor     = 3.0
	outlierMinGap     = 20.0
)

// FindAnomalies flags non-transfer transactions that are likely mistakes:
//   - sign_mismatch: positive amount in an expense category or negative in an
//     income category (could be a legitimate refund — a human should check);
//   - outlier: |amount| far above the payee's median (a typo'd extra digit);
//   - future_dated: dated after today (YYYY-MM-DD);
//   - zero_amount: amount is 0.
//
// Newest first. Pure.
func FindAnomalies(txs []Transaction, today string) []Anomaly {
	var out []Anomaly
	type key struct {
		payee string
		pos   bool
	}
	byPayee := map[key][]float64{}
	for _, t := range txs {
		if t.TransferID.Valid {
			continue
		}
		if p := NormalizePayee(t.Payee); p != "" && t.Amount != 0 {
			k := key{p, t.Amount > 0}
			byPayee[k] = append(byPayee[k], math.Abs(t.Amount))
		}
	}
	medians := map[key]float64{}
	for k, v := range byPayee {
		if len(v) >= outlierMinHistory {
			medians[k] = median(v)
		}
	}
	for _, t := range txs {
		if t.TransferID.Valid {
			continue
		}
		switch {
		case t.Amount == 0:
			out = append(out, Anomaly{t, "zero_amount", "amount is 0"})
		case t.CategoryKind == "expense" && t.Amount > 0:
			out = append(out, Anomaly{t, "sign_mismatch", "positive amount in expense category " + t.CategoryName + " (refund, or sign flipped?)"})
		case t.CategoryKind == "income" && t.Amount < 0:
			out = append(out, Anomaly{t, "sign_mismatch", "negative amount in income category " + t.CategoryName + " (sign flipped?)"})
		}
		if today != "" && t.Date > today {
			out = append(out, Anomaly{t, "future_dated", "dated after " + today})
		}
		if m, ok := medians[key{NormalizePayee(t.Payee), t.Amount > 0}]; ok {
			a := math.Abs(t.Amount)
			if a > m*outlierFactor && a-m >= outlierMinGap {
				out = append(out, Anomaly{t, "outlier", "about " + trimFloat(a/m) + "× this payee's typical amount of " + trimFloat(m)})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Tx.Date != out[j].Tx.Date {
			return out[i].Tx.Date > out[j].Tx.Date
		}
		return out[i].Tx.ID > out[j].Tx.ID
	})
	return out
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func trimFloat(f float64) string {
	s := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", f), "0"), ".")
	if s == "" {
		return "0"
	}
	return s
}
