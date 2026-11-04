package controller

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lira/internal/config"
	"lira/internal/model"

	_ "modernc.org/sqlite"
)

// mcpServer spins up /mcp over a fresh DB with two users, each holding a
// read token (tok-alice, tok-bob) and a write token (tok-alice-w, tok-bob-w).
func mcpServer(t *testing.T) (*httptest.Server, *model.Store) {
	srv, store, _ := mcpServerDir(t)
	return srv, store
}

// mcpServerDir is mcpServer that also returns the DB's directory (backups land
// in <dir>/backups).
func mcpServerDir(t *testing.T) (*httptest.Server, *model.Store, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+dir+"/t.db?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	store := model.New(db)
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureHousehold(); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureDefaultCategories(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "bob"} {
		id, err := store.CreateUser(name, "h", "member")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CreateAPIToken(id, "test", hashToken("tok-"+name), model.ScopeRead); err != nil {
			t.Fatal(err)
		}
		if err := store.CreateAPIToken(id, "test-w", hashToken("tok-"+name+"-w"), model.ScopeWrite); err != nil {
			t.Fatal(err)
		}
	}
	c := &Controller{store: store, cfg: config.Config{BaseURL: "https://lira.example", DBPath: dir + "/t.db"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", c.APIAuth(c.MCP))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, store, dir
}

type rpcResp struct {
	ID     json.RawMessage `json:"id"`
	Result map[string]any  `json:"result"`
	Error  *rpcError       `json:"error"`
}

func mcpPost(t *testing.T, srv *httptest.Server, token, body string, hdr ...string) (*http.Response, rpcResp) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out rpcResp
	if res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return res, out
}

// callTool invokes a tool and returns its structuredContent (failing on isError
// unless wantErr).
func callTool(t *testing.T, srv *httptest.Server, token, name string, args any, wantErr bool) map[string]any {
	t.Helper()
	a, _ := json.Marshal(args)
	_, r := mcpPost(t, srv, token, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+name+`","arguments":`+string(a)+`}}`)
	if r.Error != nil {
		t.Fatalf("%s: rpc error %+v", name, r.Error)
	}
	if isErr, _ := r.Result["isError"].(bool); isErr != wantErr {
		t.Fatalf("%s: isError=%v, want %v (%v)", name, isErr, wantErr, r.Result["content"])
	}
	sc, _ := r.Result["structuredContent"].(map[string]any)
	return sc
}

func TestMCPProtocol(t *testing.T) {
	srv, _ := mcpServer(t)

	res, r := mcpPost(t, srv, "tok-alice", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if res.StatusCode != 200 || r.Error != nil || r.Result["protocolVersion"] != "2025-03-26" || string(r.ID) != "1" {
		t.Fatalf("initialize: %d %+v", res.StatusCode, r)
	}
	if _, r = mcpPost(t, srv, "tok-alice", `{"jsonrpc":"2.0","id":"a","method":"initialize","params":{"protocolVersion":"1999-01-01"}}`); r.Result["protocolVersion"] != mcpProtocolVersions[0] {
		t.Errorf("unknown version should fall back to latest, got %v", r.Result["protocolVersion"])
	}

	if res, _ := mcpPost(t, srv, "tok-alice", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); res.StatusCode != http.StatusAccepted {
		t.Errorf("notification: status %d, want 202", res.StatusCode)
	}

	// A read token sees only the read tools, all annotated read-only; a write
	// token sees every tool, with only the mutating ones not read-only.
	readTools := 0
	for _, tl := range mcpTools {
		if tl.Scope != model.ScopeWrite {
			readTools++
		}
	}
	for _, tc := range []struct {
		token string
		want  int
	}{{"tok-alice", readTools}, {"tok-alice-w", len(mcpTools)}} {
		_, r = mcpPost(t, srv, tc.token, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
		tools, _ := r.Result["tools"].([]any)
		if len(tools) != tc.want {
			t.Fatalf("%s: tools/list returned %d tools, want %d", tc.token, len(tools), tc.want)
		}
		for _, tl := range tools {
			m := tl.(map[string]any)
			if s, _ := m["inputSchema"].(map[string]any); s["type"] != "object" {
				t.Errorf("%v: inputSchema must be an object schema", m["name"])
			}
			wantRO := !findMCPTool(m["name"].(string)).Mutating
			if ann, _ := m["annotations"].(map[string]any); ann["readOnlyHint"] != wantRO {
				t.Errorf("%v: readOnlyHint=%v, want %v", m["name"], ann["readOnlyHint"], wantRO)
			}
		}
	}
	if readTools == len(mcpTools) || findMCPTool("apply_recategorization").Mutating != true {
		t.Fatal("expected write-scoped, mutating tools to exist")
	}

	if _, r = mcpPost(t, srv, "tok-alice", `{"jsonrpc":"2.0","id":3,"method":"nope"}`); r.Error == nil || r.Error.Code != rpcMethodNotFound {
		t.Errorf("unknown method: %+v", r)
	}
	if _, r = mcpPost(t, srv, "tok-alice", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"drop_tables"}}`); r.Error == nil || r.Error.Code != rpcInvalidParams {
		t.Errorf("unknown tool: %+v", r)
	}
	if _, r = mcpPost(t, srv, "tok-alice", `{bad`); r.Error == nil || r.Error.Code != rpcParseError {
		t.Errorf("parse error: %+v", r)
	}
	if _, r = mcpPost(t, srv, "tok-alice", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`); r.Error == nil || r.Error.Code != rpcInvalidRequest {
		t.Errorf("batch: %+v", r)
	}
	if _, r = mcpPost(t, srv, "tok-alice", `{"jsonrpc":"2.0","id":5,"method":"ping"}`); r.Error != nil || r.Result == nil {
		t.Errorf("ping: %+v", r)
	}

	// Transport-level guards.
	if res, _ := mcpPost(t, srv, "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); res.StatusCode != 401 || !strings.HasPrefix(res.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("no token: %d %q", res.StatusCode, res.Header.Get("WWW-Authenticate"))
	}
	if res, _ := mcpPost(t, srv, "wrong", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); res.StatusCode != 401 {
		t.Errorf("bad token: %d", res.StatusCode)
	}
	if res, _ := mcpPost(t, srv, "tok-alice", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Origin", "https://evil.example"); res.StatusCode != 403 {
		t.Errorf("foreign origin: %d", res.StatusCode)
	}
	if res, _ := mcpPost(t, srv, "tok-alice", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Origin", "https://lira.example"); res.StatusCode != 200 {
		t.Errorf("own origin: %d", res.StatusCode)
	}
	if res, _ := mcpPost(t, srv, "tok-alice", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "MCP-Protocol-Version", "1999-01-01"); res.StatusCode != 400 {
		t.Errorf("bad protocol header: %d", res.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer tok-alice")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET should be 405: %v %v", res.StatusCode, err)
	}
}

func TestMCPTools(t *testing.T) {
	srv, store := mcpServer(t)

	// Every tool runs on an empty ledger without error.
	for _, tl := range mcpTools {
		args := map[string]any{}
		if tl.Name == "get_project" || tl.Scope == model.ScopeWrite {
			continue
		}
		callTool(t, srv, "tok-alice", tl.Name, args, false)
	}

	if err := store.CreateAccount("Main", "current", "EUR", 1000, false); err != nil {
		t.Fatal(err)
	}
	accts, _ := store.ListAccounts()
	aid := accts[0].ID
	for _, x := range []struct {
		date  string
		amt   float64
		payee string
	}{
		{"2026-01-05", -20, "Lidl"}, {"2026-01-05", -20, "Lidl"}, // duplicate
		{"2026-01-06", -500, "Landlord"}, {"2026-01-07", 2000, "Employer"},
	} {
		if err := store.CreateTransaction(aid, x.date, x.amt, nil, x.payee, ""); err != nil {
			t.Fatal(err)
		}
	}

	sr := callTool(t, srv, "tok-alice", "search_transactions", map[string]any{"query": "lidl", "limit": 1}, false)
	if sr["total"].(float64) != 2 || sr["returned"].(float64) != 1 {
		t.Errorf("search: %+v", sr)
	}
	callTool(t, srv, "tok-alice", "search_transactions", map[string]any{"from": "January"}, true)
	callTool(t, srv, "tok-alice", "search_transactions", map[string]any{"limit": 9999}, true)
	callTool(t, srv, "tok-alice", "search_transactions", map[string]any{"limit": "ten"}, true)
	callTool(t, srv, "tok-alice", "search_transactions", map[string]any{"from": "2026-02-01", "to": "2026-01-01"}, true)

	d := callTool(t, srv, "tok-alice", "find_duplicate_transactions", map[string]any{}, false)
	if d["groupsFound"].(float64) != 1 || d["surplusAmountIfAllDuplicates"].(float64) != 20 {
		t.Errorf("duplicates: %+v", d)
	}
	if d := callTool(t, srv, "tok-alice", "find_duplicate_transactions", map[string]any{"from": "2026-02-01"}, false); d["groupsFound"].(float64) != 0 {
		t.Errorf("duplicates outside range should be filtered: %+v", d)
	}
	u := callTool(t, srv, "tok-alice", "find_uncategorized_transactions", map[string]any{}, false)
	if u["uncategorizedTransactions"].(float64) != 4 {
		t.Errorf("uncategorized: %+v", u)
	}
	s := callTool(t, srv, "tok-alice", "get_financial_summary", map[string]any{}, false)
	if s["netWorthEUR"].(float64) != 1000-20-20-500+2000 {
		t.Errorf("summary net worth: %+v", s["netWorthEUR"])
	}
	callTool(t, srv, "tok-alice", "get_project", map[string]any{"id": 999}, true)

	// Pocket money stays private: each token sees only its owner's pot.
	alice, _ := store.GetUserByUsername("alice")
	if err := store.AddPocketEntry(alice.ID, "2026-01-01", 50, "", "top-up"); err != nil {
		t.Fatal(err)
	}
	if p := callTool(t, srv, "tok-alice", "get_pocket", map[string]any{}, false); p["balance"].(float64) != 50 {
		t.Errorf("alice pocket: %+v", p)
	}
	if p := callTool(t, srv, "tok-bob", "get_pocket", map[string]any{}, false); p["balance"].(float64) != 0 || len(p["recentEntries"].([]any)) != 0 {
		t.Errorf("bob must not see alice's pocket: %+v", p)
	}
}
