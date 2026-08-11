package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeRqlited implements just enough of the rqlite HTTP API to exercise the
// RqliteAuthority client: /db/execute records writes, /db/query serves rows from an
// in-memory model interpreting the client's two statement shapes.
type fakeRqlited struct {
	srv *httptest.Server

	mu       sync.Mutex
	tenants  map[string]string
	accounts map[string][]string // account -> [state, reason, since]
	schemas  int
}

func newFakeRqlited(t *testing.T) *fakeRqlited {
	t.Helper()
	f := &fakeRqlited{tenants: map[string]string{}, accounts: map[string][]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/db/execute", func(w http.ResponseWriter, r *http.Request) {
		var stmts [][]any
		_ = json.NewDecoder(r.Body).Decode(&stmts)
		_ = r.Body.Close()
		f.mu.Lock()
		for _, st := range stmts {
			sql, _ := st[0].(string)
			switch {
			case strings.HasPrefix(sql, "CREATE TABLE"):
				f.schemas++
			case strings.HasPrefix(sql, "INSERT OR IGNORE INTO relay_tenants"):
				tenant, _ := st[1].(string)
				account, _ := st[2].(string)
				if _, exists := f.tenants[tenant]; !exists {
					f.tenants[tenant] = account
				}
			case strings.HasPrefix(sql, "INSERT INTO relay_accounts"):
				account, _ := st[1].(string)
				state, _ := st[2].(string)
				reason, _ := st[3].(string)
				since, _ := st[4].(string)
				// Terminal-ban guard mirrored from the upsert CASE.
				if cur, ok := f.accounts[account]; ok && cur[0] == string(StateBanned) && state != string(StateBanned) {
					continue
				}
				f.accounts[account] = []string{state, reason, since}
			}
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{}]}`))
	})
	mux.HandleFunc("/db/query", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "level=linearizable") {
			// Docs F20: the ban gate must use linearizable reads; flag anything weaker.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"linearizable level required"}`))
			return
		}
		// Real rqlite shape: an array of statements, each [sql, args...].
		var stmts [][]any
		_ = json.NewDecoder(r.Body).Decode(&stmts)
		_ = r.Body.Close()
		if len(stmts) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"no statement"}`))
			return
		}
		stmt := stmts[0]
		sql, _ := stmt[0].(string)
		f.mu.Lock()
		var values []any
		switch {
		case strings.Contains(sql, "FROM relay_tenants WHERE tenant_id"):
			if acct, ok := f.tenants[stmt[1].(string)]; ok {
				values = []any{[]any{acct}}
			}
		case strings.Contains(sql, "FROM relay_accounts WHERE account_id"):
			if rec, ok := f.accounts[stmt[1].(string)]; ok {
				values = []any{[]any{rec[0], rec[1], rec[2]}}
			}
		case strings.Contains(sql, "FROM relay_tenants"):
			for tn, acct := range f.tenants {
				values = append(values, []any{tn, acct})
			}
		case strings.Contains(sql, "FROM relay_accounts"):
			for acct, rec := range f.accounts {
				values = append(values, []any{acct, rec[0]})
			}
		}
		f.mu.Unlock()
		out := map[string]any{"results": []any{map[string]any{
			"columns": []string{"c1"},
			"types":   []string{"text"},
		}}}
		if values != nil {
			out["results"].([]any)[0].(map[string]any)["values"] = values
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestRqliteAuthorityLifecycle(t *testing.T) {
	fake := newFakeRqlited(t)
	a, err := NewRqliteAuthority(fake.srv.URL)
	if err != nil {
		t.Fatalf("NewRqliteAuthority: %v", err)
	}
	if !a.Healthy() {
		t.Fatal("authority must be healthy against a live rqlited")
	}
	if fake.schemas == 0 {
		t.Fatal("schema init must run at construction")
	}

	// Lifetime 1:1 binding (T6-equivalent semantics on the rqlite backend).
	if err := a.BindTenant("t-1", "acct-1"); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if err := a.BindTenant("t-1", "acct-1"); err != nil {
		t.Fatalf("idempotent rebind: %v", err)
	}
	if err := a.BindTenant("t-1", "acct-2"); err == nil {
		t.Fatal("conflicting bind must be rejected (no spillover)")
	} else if be, ok := err.(*BanError); !ok || be.Status != 409 {
		t.Fatalf("conflict must be 409, got %T (%v)", err, err)
	}

	// Ban FSM: quarantine gate, terminal ban, no downgrade.
	if err := a.CheckDispatch("acct-1"); err != nil {
		t.Fatalf("clean account must dispatch: %v", err)
	}
	if err := a.Quarantine("acct-1", "test-signature"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if err := a.CheckDispatch("acct-1"); err == nil {
		t.Fatal("quarantined account must be denied")
	}
	if err := a.Ban("acct-1", "confirmed"); err != nil {
		t.Fatalf("ban: %v", err)
	}
	if err := a.Reinstate("acct-1"); err != nil {
		t.Fatalf("reinstate call: %v", err)
	}
	if err := a.CheckDispatch("acct-1"); err == nil {
		t.Fatal("terminal ban must not be downgraded by reinstate")
	}

	// Snapshot surfaces both maps.
	tenants, states := a.Snapshot()
	if tenants["t-1"] != "acct-1" || states["acct-1"] != StateBanned {
		t.Fatalf("snapshot wrong: %v %v", tenants, states)
	}
}

func TestRqliteAuthorityFailClosed(t *testing.T) {
	// Unreachable server: every gate denies, Healthy reports false.
	a, err := NewRqliteAuthority("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("construction must not fail hard (fail-closed instead): %v", err)
	}
	if a.Healthy() {
		t.Fatal("authority against a dead rqlited must report unhealthy")
	}
	if err := a.CheckDispatch("acct-x"); err == nil {
		t.Fatal("unreachable authority must deny dispatch (fail-closed)")
	}
	if err := a.BindTenant("t", "a"); err == nil {
		t.Fatal("unreachable authority must deny binds (fail-closed)")
	}
	if err := a.Quarantine("a", "r"); err == nil {
		t.Fatal("unreachable authority must fail state writes")
	}
}

func TestRqliteAuthorityRejectsUnsafeEndpoints(t *testing.T) {
	if _, err := NewRqliteAuthority("https://127.0.0.1:4001"); err == nil {
		t.Fatal("https must be refused: the authority path is plaintext host-local only")
	}
	if _, err := NewRqliteAuthority("http://rqlite.internal:4001"); err == nil {
		t.Fatal("DNS hostnames must be refused on the authority path")
	}
	if _, err := NewRqliteAuthority("http://127.0.0.1:4001"); err != nil {
		t.Fatalf("loopback IP literal must be accepted: %v", err)
	}
}
