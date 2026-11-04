package controller

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"lira/internal/model"
)

func TestMCPRecategorize(t *testing.T) {
	srv, store, dir := mcpServerDir(t)

	for _, n := range []string{"Main", "Savings"} {
		if err := store.CreateAccount(n, "current", "EUR", 0, false); err != nil {
			t.Fatal(err)
		}
	}
	accts, _ := store.ListAccounts()
	a1, a2 := accts[0].ID, accts[1].ID
	cats, _ := store.ListCategories()
	var expense, income int64
	for _, c := range cats {
		if c.Kind == "expense" && expense == 0 {
			expense = c.ID
		}
		if c.Kind == "income" && income == 0 {
			income = c.ID
		}
	}
	for _, x := range []struct {
		amt   float64
		payee string
		cat   *int64
	}{
		{-20, "Lidl", nil}, {-30, "Lidl", nil}, {-40, "Lidl", nil}, {-5, "Kiosk", &expense},
	} {
		if err := store.CreateTransaction(a1, "2026-01-05", x.amt, x.cat, x.payee, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CreateTransfer(a1, a2, "2026-01-06", 10, 10, ""); err != nil {
		t.Fatal(err)
	}
	txs, _ := store.SearchTransactions(model.TxFilter{IncludeTransfers: true})
	id := map[string]int64{}
	for _, tx := range txs {
		switch {
		case tx.TransferID.Valid:
			id["transfer"] = tx.ID
		case tx.Payee == "Lidl":
			id[map[float64]string{-20: "l20", -30: "l30", -40: "l40"}[tx.Amount]] = tx.ID
		default:
			id["kiosk"] = tx.ID
		}
	}
	ch := func(tx int64, cat any) map[string]any { return map[string]any{"transactionId": tx, "categoryId": cat} }

	// A read token can't use the write tools.
	callTool(t, srv, "tok-alice", "propose_recategorization", map[string]any{"changes": []any{ch(id["l20"], expense)}}, true)

	// Malformed input is an in-band error; state problems are per-row skips.
	for _, bad := range []map[string]any{
		{},
		{"changes": []any{}},
		{"changes": []any{map[string]any{"transactionId": id["l20"]}}}, // categoryId missing
		{"changes": []any{ch(id["l20"], expense), ch(id["l20"], income)}},
		{"changes": []any{ch(0, expense)}},
	} {
		callTool(t, srv, "tok-alice-w", "propose_recategorization", bad, true)
	}

	p := callTool(t, srv, "tok-alice-w", "propose_recategorization", map[string]any{"changes": []any{
		ch(id["l20"], expense),
		ch(id["l30"], income), // negative into income → warning, still proposed
		ch(id["l40"], expense),
		ch(id["kiosk"], expense),    // already there
		ch(id["transfer"], expense), // transfer leg
		ch(999999, expense),         // missing
	}}, false)
	if p["changes"].(float64) != 3 || len(p["skipped"].([]any)) != 3 || p["proposalId"] == nil {
		t.Fatalf("propose: %+v", p)
	}
	warned := 0
	for _, r := range p["preview"].([]any) {
		if _, ok := r.(map[string]any)["warning"]; ok {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("want 1 kind-mismatch warning, got %d: %+v", warned, p["preview"])
	}
	// Proposing changed nothing.
	if tx, _ := store.GetTransaction(id["l20"]); tx.CategoryID.Valid {
		t.Fatal("propose must not change the ledger")
	}
	pid := p["proposalId"].(string)

	// A human edits one row after the proposal: apply must skip it, not overwrite.
	l40, _ := store.GetTransaction(id["l40"])
	if err := store.UpdateTransaction(l40.ID, l40.AccountID, l40.Date, l40.Amount, &income, l40.Payee, ""); err != nil {
		t.Fatal(err)
	}

	// Another user can't apply alice's proposal; a read token can't either.
	callTool(t, srv, "tok-bob-w", "apply_recategorization", map[string]any{"proposalId": pid}, true)
	callTool(t, srv, "tok-alice", "apply_recategorization", map[string]any{"proposalId": pid}, true)

	res := callTool(t, srv, "tok-alice-w", "apply_recategorization", map[string]any{"proposalId": pid}, false)
	if res["applied"].(float64) != 2 || len(res["skipped"].([]any)) != 1 {
		t.Fatalf("apply: %+v", res)
	}
	if tx, _ := store.GetTransaction(id["l20"]); tx.CategoryID.Int64 != expense {
		t.Errorf("l20 not recategorized: %+v", tx.CategoryID)
	}
	if tx, _ := store.GetTransaction(id["l40"]); tx.CategoryID.Int64 != income {
		t.Errorf("human edit to l40 was overwritten: %+v", tx.CategoryID)
	}
	b, _ := res["backup"].(string)
	if b == "" {
		t.Fatalf("batch apply should report a backup: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "backups", b)); err != nil {
		t.Errorf("backup file missing: %v", err)
	}

	// Single use.
	callTool(t, srv, "tok-alice-w", "apply_recategorization", map[string]any{"proposalId": pid}, true)

	// Audit log: one entry per applied row, visible to read tokens.
	log := callTool(t, srv, "tok-bob", "list_recent_changes", map[string]any{}, false)
	if n := len(log["changes"].([]any)); n != 2 {
		t.Fatalf("audit entries = %d, want 2: %+v", n, log)
	}
	e := log["changes"].([]any)[0].(map[string]any)
	if e["user"] != "alice" || e["proposalId"] != pid || e["action"] != "recategorize" {
		t.Errorf("audit entry: %+v", e)
	}

	// Undo: the returned reverse changes round-trip through propose→apply.
	undo := callTool(t, srv, "tok-alice-w", "propose_recategorization", map[string]any{"changes": res["undoChanges"]}, false)
	if undo["changes"].(float64) != 2 {
		t.Fatalf("undo propose: %+v", undo)
	}
	callTool(t, srv, "tok-alice-w", "apply_recategorization", map[string]any{"proposalId": undo["proposalId"]}, false)
	for _, k := range []string{"l20", "l30"} {
		if tx, _ := store.GetTransaction(id[k]); tx.CategoryID.Valid {
			t.Errorf("%s should be uncategorized again", k)
		}
	}

	// An expired proposal can't be applied.
	alice, _ := store.GetUserByUsername("alice")
	old, _, err := store.CreateProposal(alice.ID, model.ActionRecategorize, []byte(`[]`), -time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	callTool(t, srv, "tok-alice-w", "apply_recategorization", map[string]any{"proposalId": old}, true)
}

func TestPreChangeBackupPrunes(t *testing.T) {
	_, store, dir := mcpServerDir(t)
	c := &Controller{store: store}
	c.cfg.DBPath = filepath.Join(dir, "t.db")
	bdir := filepath.Join(dir, "backups")
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < preChangeBackupKeep+3; i++ { // older, already on disk
		if err := os.WriteFile(filepath.Join(bdir, "lira-pre-mcp-20000101T0000"+string(rune('a'+i))+"Z-x.db"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	name, err := c.preChangeBackup("abcdefghijk")
	if err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(bdir, "lira-pre-mcp-*.db"))
	if len(left) != preChangeBackupKeep {
		t.Errorf("kept %d backups, want %d", len(left), preChangeBackupKeep)
	}
	if _, err := os.Stat(filepath.Join(bdir, name)); err != nil {
		t.Errorf("newest backup was pruned: %v", err)
	}
}
