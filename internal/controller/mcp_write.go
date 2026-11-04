package controller

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"lira/internal/model"
)

// ---------- MCP write tools (propose→apply) ----------
//
// Only tokens with the "write" scope see these. propose_* validates and stores
// a plan (changing nothing); apply_* claims it exactly once, writes a backup
// first when more than one row changes, and audit-logs every changed row in
// the same DB transaction (model/changes.go).

const (
	maxRecatChanges     = 500
	maxNewCategories    = 20
	maxCategoryName     = 60
	proposalTTL         = 30 * time.Minute
	preChangeBackupKeep = 10 // newest pre-change backups kept on disk
	auditSourceMCP      = "mcp"
)

// parseNewCategories validates propose_recategorization's newCategories:
// trimmed, non-empty, unique (case-insensitive) names that don't already
// exist, each with kind income|expense. Keyed by lower-cased name.
func parseNewCategories(raw any, existing []model.Category) ([]model.NewCategory, map[string]model.NewCategory, error) {
	byName := map[string]model.NewCategory{}
	if raw == nil {
		return nil, byName, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, nil, errors.New("newCategories must be an array")
	}
	if len(list) > maxNewCategories {
		return nil, nil, fmt.Errorf("at most %d new categories per proposal", maxNewCategories)
	}
	taken := map[string]model.Category{}
	for _, ct := range existing {
		taken[strings.ToLower(strings.TrimSpace(ct.Name))] = ct
	}
	var out []model.NewCategory
	for i, el := range list {
		m, ok := el.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("newCategories[%d] must be an object", i)
		}
		name, _ := m["name"].(string)
		name = strings.Join(strings.Fields(name), " ")
		if name == "" || utf8.RuneCountInString(name) > maxCategoryName {
			return nil, nil, fmt.Errorf("newCategories[%d].name must be 1-%d characters", i, maxCategoryName)
		}
		kind, _ := m["kind"].(string)
		if kind != "income" && kind != "expense" {
			return nil, nil, fmt.Errorf("newCategories[%d].kind must be income or expense", i)
		}
		key := strings.ToLower(name)
		if ct, ok := taken[key]; ok {
			return nil, nil, fmt.Errorf("category %q already exists (id %d, %s) — use its categoryId", ct.Name, ct.ID, ct.Kind)
		}
		if _, dup := byName[key]; dup {
			return nil, nil, fmt.Errorf("new category %q is listed more than once", name)
		}
		nc := model.NewCategory{Name: name, Kind: kind}
		byName[key] = nc
		out = append(out, nc)
	}
	return out, byName, nil
}

func (c *Controller) mcpProposeRecat(u *model.User, a mcpArgs) (map[string]any, error) {
	var raw []any
	if v, present := a["changes"]; present && v != nil {
		var ok bool
		if raw, ok = v.([]any); !ok {
			return nil, errors.New("changes must be an array")
		}
	}
	if len(raw) > maxRecatChanges {
		return nil, fmt.Errorf("at most %d changes per proposal", maxRecatChanges)
	}
	cats, err := c.store.ListCategories()
	if err != nil {
		return nil, errors.New("could not load categories")
	}
	newCats, newByName, err := parseNewCategories(a["newCategories"], cats)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 && len(newCats) == 0 {
		return nil, errors.New("give at least one change or new category")
	}
	byID := make(map[int64]model.Category, len(cats))
	for _, ct := range cats {
		byID[ct.ID] = ct
	}
	catName := func(id *int64) any {
		if id == nil {
			return nil
		}
		return byID[*id].Name
	}

	var changes []model.RecatChange
	preview := []map[string]any{}
	skipped := []map[string]any{}
	skip := func(id int64, why string) {
		skipped = append(skipped, map[string]any{"transactionId": id, "reason": why})
	}
	seen := map[int64]bool{}
	used := map[string]int{} // new category key → changes using it
	for i, el := range raw {
		m, ok := el.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("changes[%d] must be an object", i)
		}
		ca := mcpArgs(m)
		txID, err := ca.integer("transactionId", 0, 1, math.MaxInt32)
		if err != nil || txID == 0 {
			return nil, fmt.Errorf("changes[%d].transactionId must be a positive integer", i)
		}
		id := int64(txID)
		if seen[id] {
			return nil, fmt.Errorf("transaction %d appears more than once", id)
		}
		seen[id] = true
		cv, hasID := m["categoryId"]
		nv, hasNew := m["newCategory"]
		if hasID && hasNew {
			return nil, fmt.Errorf("changes[%d]: give categoryId or newCategory, not both", i)
		}
		if !hasID && !hasNew {
			return nil, fmt.Errorf("changes[%d].categoryId is required (null clears the category), or newCategory to use a category from newCategories", i)
		}
		var to *int64
		var newCat *model.NewCategory
		switch {
		case hasNew:
			name, _ := nv.(string)
			nc, ok := newByName[strings.ToLower(strings.Join(strings.Fields(name), " "))]
			if !ok {
				return nil, fmt.Errorf("changes[%d].newCategory %q is not in newCategories", i, name)
			}
			newCat = &nc
		case cv != nil:
			n, err := ca.integer("categoryId", 0, 1, math.MaxInt32)
			if err != nil || n == 0 {
				return nil, fmt.Errorf("changes[%d].categoryId must be a positive integer or null", i)
			}
			cid := int64(n)
			if _, ok := byID[cid]; !ok {
				skip(id, fmt.Sprintf("category %d does not exist", cid))
				continue
			}
			to = &cid
		}

		t, err := c.store.GetTransaction(id)
		if errors.Is(err, sql.ErrNoRows) {
			skip(id, "transaction not found")
			continue
		}
		if err != nil {
			return nil, errors.New("could not load transactions")
		}
		if t.TransferID.Valid {
			skip(id, "transfer legs are not categorized")
			continue
		}
		var from *int64
		if t.CategoryID.Valid {
			v := t.CategoryID.Int64
			from = &v
		}
		if newCat == nil && ((from == nil && to == nil) || (from != nil && to != nil && *from == *to)) {
			skip(id, "already in that category")
			continue
		}
		row := map[string]any{
			"transactionId": id, "date": t.Date, "payee": t.Payee, "amount": r2(t.Amount),
			"fromCategory": catName(from), "toCategory": catName(to),
		}
		kind := ""
		switch {
		case newCat != nil:
			row["toCategory"], row["newCategory"] = newCat.Name, true
			kind = newCat.Kind
		case to != nil:
			kind = byID[*to].Kind
		}
		switch {
		case kind == "expense" && t.Amount > 0:
			row["warning"] = "positive amount (income/refund) into an expense category"
		case kind == "income" && t.Amount < 0:
			row["warning"] = "negative amount (spending) into an income category"
		}
		preview = append(preview, row)
		ch := model.RecatChange{TxID: id, From: from, To: to}
		if newCat != nil {
			ch.NewCat = newCat.Name
			used[strings.ToLower(newCat.Name)]++
		}
		changes = append(changes, ch)
	}

	newPreview := make([]map[string]any, 0, len(newCats))
	for _, nc := range newCats {
		newPreview = append(newPreview, map[string]any{"name": nc.Name, "kind": nc.Kind, "transactions": used[strings.ToLower(nc.Name)]})
	}
	out := map[string]any{"changes": len(changes), "preview": preview, "skipped": skipped, "newCategories": newPreview}
	if len(changes) == 0 && len(newCats) == 0 {
		out["proposalId"] = nil
		out["message"] = "Nothing to apply — every change was skipped."
		return out, nil
	}
	payload, err := json.Marshal(model.RecatPlan{NewCategories: newCats, Changes: changes})
	if err != nil {
		return nil, errors.New("could not encode proposal")
	}
	id, exp, err := c.store.CreateProposal(u.ID, model.ActionRecategorize, payload, proposalTTL)
	if err != nil {
		return nil, errors.New("could not store proposal")
	}
	out["proposalId"], out["expiresAt"] = id, exp
	out["next"] = "Nothing has changed yet. Show this preview (including any new categories) to the user and call apply_recategorization with this proposalId only if they approve it."
	return out, nil
}

func (c *Controller) mcpApplyRecat(u *model.User, a mcpArgs) (map[string]any, error) {
	id, err := a.str("proposalId")
	if err != nil || id == "" {
		return nil, errors.New("proposalId is required")
	}
	payload, err := c.store.PendingProposal(u.ID, id, model.ActionRecategorize)
	if err != nil {
		if errors.Is(err, model.ErrProposalUnavailable) {
			return nil, err
		}
		return nil, errors.New("could not load proposal")
	}
	plan, err := model.DecodeRecatPlan(payload)
	if err != nil {
		return nil, errors.New("proposal is corrupt")
	}
	backup := ""
	if len(plan.Changes)+len(plan.NewCategories) > 1 {
		if backup, err = c.preChangeBackup(id); err != nil {
			return nil, errors.New("refusing to apply: could not write the pre-change database backup")
		}
	}
	applied, skips, created, err := c.store.ApplyRecategorization(u.ID, id, auditSourceMCP, plan)
	if err != nil {
		if errors.Is(err, model.ErrProposalUnavailable) {
			return nil, err
		}
		return nil, errors.New("could not apply proposal; nothing was changed")
	}
	ids := make([]int64, 0, len(applied))
	undo := make([]map[string]any, 0, len(applied))
	for _, ch := range applied {
		ids = append(ids, ch.TxID)
		undo = append(undo, map[string]any{"transactionId": ch.TxID, "categoryId": nptrInt(ch.From)})
	}
	skipped := make([]map[string]any, 0, len(skips))
	for _, s := range skips {
		skipped = append(skipped, map[string]any{"transactionId": s.TxID, "reason": s.Reason})
	}
	out := map[string]any{
		"proposalId": id, "applied": len(applied), "appliedTransactionIds": ids, "skipped": skipped,
		"undoChanges": undo,
	}
	if len(created) > 0 {
		cs := make([]map[string]any, 0, len(created))
		for _, cc := range created {
			row := map[string]any{"name": cc.Name, "kind": cc.Kind}
			switch {
			case cc.Reason != "":
				row["created"], row["reason"] = false, cc.Reason
			case cc.Reused:
				row["categoryId"], row["created"], row["reason"] = cc.ID, false, "an identical category already existed; reused it"
			default:
				row["categoryId"], row["created"] = cc.ID, true
			}
			cs = append(cs, row)
		}
		out["newCategories"] = cs
		out["undoNote"] = "undoChanges restores the transactions; new categories stay — delete them on the Lira Categories page if unwanted."
	}
	if backup != "" {
		out["backup"] = backup
	}
	return out, nil
}

func nptrInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func (c *Controller) mcpChangeLog(u *model.User, a mcpArgs) (map[string]any, error) {
	limit, err := a.integer("limit", 50, 1, 200)
	if err != nil {
		return nil, err
	}
	entries, err := c.store.ListAuditLog(limit)
	if err != nil {
		return nil, errors.New("could not load the change log")
	}
	rows := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		var oldV, newV any
		_ = json.Unmarshal([]byte(e.OldValue), &oldV)
		_ = json.Unmarshal([]byte(e.NewValue), &newV)
		rows = append(rows, map[string]any{
			"id": e.ID, "at": e.At, "user": e.Username, "source": e.Source, "action": e.Action,
			"entity": e.Entity, "entityId": e.EntityID, "old": oldV, "new": newV, "proposalId": e.Ref,
		})
	}
	return map[string]any{"changes": rows}, nil
}

// preChangeBackup snapshots the DB into backups/ next to it before a batch
// write and prunes older pre-change backups beyond preChangeBackupKeep. It
// returns the backup's file name.
func (c *Controller) preChangeBackup(tag string) (string, error) {
	if c.cfg.DBPath == "" {
		return "", errors.New("no database path configured")
	}
	dir := filepath.Join(filepath.Dir(c.cfg.DBPath), "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if len(tag) > 8 {
		tag = tag[:8]
	}
	name := fmt.Sprintf("lira-pre-mcp-%s-%s.db", time.Now().UTC().Format("20060102T150405Z"), tag)
	path := filepath.Join(dir, name)
	if err := c.store.Backup(path); err != nil {
		return "", err
	}
	_ = os.Chmod(path, 0o600)
	if old, err := filepath.Glob(filepath.Join(dir, "lira-pre-mcp-*.db")); err == nil && len(old) > preChangeBackupKeep {
		sort.Strings(old) // timestamped names sort oldest first
		for _, p := range old[:len(old)-preChangeBackupKeep] {
			_ = os.Remove(p)
		}
	}
	return name, nil
}
