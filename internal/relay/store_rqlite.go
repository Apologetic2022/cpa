package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// RqliteAuthority is the docs-conformant authority backend (F8/F20): tenant->account
// affinity and the ban FSM live in rqlite (Raft + SQLite); reads are linearizable
// (leader-confirmed, never follower-stale — docs: ban state must never be trusted
// from a lagging replica). Every operation is fail-closed: any transport/storage
// error marks the authority unhealthy and denies dispatch until the operator
// restores it. The client only speaks plaintext HTTP to the configured rqlited
// endpoint (expected loopback/host-local); it carries no TLS and no egress.
type RqliteAuthority struct {
	base string // e.g. http://127.0.0.1:4001
	hc   *http.Client

	// unhealthy is a sticky flag set by any failed operation; Healthy() reports it.
	// Operations still attempt the server (it may have recovered), but a failure
	// always denies THAT operation — nothing is ever optimistically admitted.
	unhealthy atomic.Bool
}

// NewRqliteAuthority connects to rqlited at baseURL and ensures the schema.
// Construction is fail-closed: an unreachable server or failed schema init returns
// an authority that denies every dispatch (Healthy() == false until it recovers).
func NewRqliteAuthority(baseURL string) (*RqliteAuthority, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("rqlite url %q invalid: %v", baseURL, err)
	}
	if u.Scheme != "http" {
		return nil, fmt.Errorf("rqlite url %q refused: plaintext http to a host-local rqlited only (no TLS, no egress)", baseURL)
	}
	if host := u.Hostname(); net.ParseIP(host) == nil && !strings.EqualFold(host, "localhost") {
		return nil, fmt.Errorf("rqlite url %q refused: IP literal or localhost only (no DNS on the authority path)", baseURL)
	}
	a := &RqliteAuthority{
		base: strings.TrimRight(u.String(), "/"),
		hc:   &http.Client{Timeout: 5 * time.Second},
	}
	if err := a.ensureSchema(); err != nil {
		a.unhealthy.Store(true)
		// Not fatal at construction: dispatch stays fail-closed until rqlited comes up.
		return a, nil
	}
	return a, nil
}

const rqliteSchema = `CREATE TABLE IF NOT EXISTS relay_tenants (
  tenant_id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL,
  created_at TEXT NOT NULL
)`
const rqliteSchema2 = `CREATE TABLE IF NOT EXISTS relay_accounts (
  account_id TEXT PRIMARY KEY,
  state TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  since TEXT NOT NULL,
  updated_at TEXT NOT NULL
)`

func (a *RqliteAuthority) ensureSchema() error {
	if _, err := a.execute(true, []any{rqliteSchema}, []any{rqliteSchema2}); err != nil {
		return fmt.Errorf("rqlite schema init: %w", err)
	}
	return nil
}

// Healthy reports whether the last operation reached the authority.
func (a *RqliteAuthority) Healthy() bool { return !a.unhealthy.Load() }

// BindTenant enforces the lifetime 1:1 binding (docs S1/6.7.3). INSERT OR IGNORE then
// linearizable SELECT: rqlite serializes writes on the leader, so a concurrent
// conflicting first-bind loses deterministically and is rejected (no spillover).
func (a *RqliteAuthority) BindTenant(tenantID, accountID string) error {
	stmt := `INSERT OR IGNORE INTO relay_tenants (tenant_id, account_id, created_at) VALUES (?, ?, ?)`
	if _, err := a.execute(true, []any{stmt, tenantID, accountID, time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
		return &BanError{Reason: "relay authority unavailable (fail-closed): " + err.Error()}
	}
	rows, err := a.query(`SELECT account_id FROM relay_tenants WHERE tenant_id = ?`, tenantID)
	if err != nil {
		return &BanError{Reason: "relay authority unreadable (fail-closed): " + err.Error()}
	}
	if len(rows) == 0 {
		a.unhealthy.Store(true)
		return &BanError{Reason: "relay authority lost the tenant binding write (fail-closed)"}
	}
	bound, _ := rows[0][0].(string)
	if bound != accountID {
		return &BanError{
			Status: 409,
			Reason: fmt.Sprintf("tenant is lifetime-bound to a different account (strict 1:1, no spillover); bound=%s requested=%s", bound, accountID),
		}
	}
	return nil
}

// CheckDispatch is the synchronous, fail-closed ban/quarantine gate (docs F20):
// linearizable read before every dispatch; unreachable authority denies.
func (a *RqliteAuthority) CheckDispatch(accountID string) error {
	rows, err := a.query(`SELECT state, reason, since FROM relay_accounts WHERE account_id = ?`, accountID)
	if err != nil {
		return &BanError{Reason: "relay authority unreadable (fail-closed): " + err.Error()}
	}
	if len(rows) == 0 {
		return nil // no record: never flagged
	}
	state, _ := rows[0][0].(string)
	reason, _ := rows[0][1].(string)
	since, _ := rows[0][2].(string)
	switch AccountState(state) {
	case StateQuarantine:
		return &BanError{Status: 503, Reason: fmt.Sprintf("account quarantined since %s (%s); no auto-retry, operator action required", since, reason)}
	case StateBanned:
		return &BanError{Status: 410, Reason: fmt.Sprintf("account banned (%s); permanently removed from dispatch", reason)}
	}
	return nil
}

// Quarantine moves an account to quarantine immediately (docs: disabled-body = instant
// quarantine, no auto-retry, page ops). Idempotent; never downgrades Banned.
func (a *RqliteAuthority) Quarantine(accountID, reason string) error {
	return a.setState(accountID, StateQuarantine, reason)
}

// Ban marks an account confirmed-banned (terminal). Operator-only transition.
func (a *RqliteAuthority) Ban(accountID, reason string) error {
	return a.setState(accountID, StateBanned, reason)
}

// Reinstate clears quarantine after operator review (docs: 误报恢复).
func (a *RqliteAuthority) Reinstate(accountID string) error {
	return a.setState(accountID, StateActive, "")
}

func (a *RqliteAuthority) setState(accountID string, state AccountState, reason string) error {
	// Banned is terminal: the guarded UPDATE refuses any downgrade.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	upsert := `INSERT INTO relay_accounts (account_id, state, reason, since, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(account_id) DO UPDATE SET
  state = CASE WHEN relay_accounts.state = 'banned' AND excluded.state != 'banned'
               THEN relay_accounts.state ELSE excluded.state END,
  reason = CASE WHEN relay_accounts.state = 'banned' AND excluded.state != 'banned'
                THEN relay_accounts.reason ELSE excluded.reason END,
  updated_at = excluded.updated_at`
	if _, err := a.execute(true, []any{upsert, accountID, string(state), reason, now, now}); err != nil {
		return fmt.Errorf("relay authority state write failed: %w", err)
	}
	return nil
}

// Snapshot copies tenant bindings and account states (management surface).
func (a *RqliteAuthority) Snapshot() (map[string]string, map[string]AccountState) {
	tenants := map[string]string{}
	states := map[string]AccountState{}
	if rows, err := a.query(`SELECT tenant_id, account_id FROM relay_tenants`); err == nil {
		for _, r := range rows {
			t, _ := r[0].(string)
			acct, _ := r[1].(string)
			tenants[t] = acct
		}
	}
	if rows, err := a.query(`SELECT account_id, state FROM relay_accounts`); err == nil {
		for _, r := range rows {
			acct, _ := r[0].(string)
			st, _ := r[1].(string)
			states[acct] = AccountState(st)
		}
	}
	return tenants, states
}

// execute POSTs parameterized statements to /db/execute. Writes are leader-executed;
// transaction=true makes multi-statement batches atomic.
func (a *RqliteAuthority) execute(transaction bool, stmts ...[]any) (map[string]any, error) {
	u := a.base + "/db/execute"
	if transaction {
		u += "?transaction"
	}
	return a.post(u, stmts)
}

// query POSTs a parameterized read to /db/query with linearizable consistency
// (docs F20: the ban gate must never read a stale follower).
// rqlite expects an array of statements, each statement itself an array:
// [[sql, arg1, ...]] — a flat [sql, arg1] is parsed as N separate statements.
func (a *RqliteAuthority) query(stmt string, args ...any) ([][]any, error) {
	u := a.base + "/db/query?level=linearizable"
	stmtArray := make([]any, 0, len(args)+1)
	stmtArray = append(stmtArray, stmt)
	stmtArray = append(stmtArray, args...)
	resp, err := a.post(u, []any{stmtArray})
	if err != nil {
		return nil, err
	}
	results, _ := resp["results"].([]any)
	if len(results) == 0 {
		return nil, nil
	}
	first, _ := results[0].(map[string]any)
	if errText, _ := first["error"].(string); errText != "" {
		a.unhealthy.Store(true)
		return nil, fmt.Errorf("rqlite query error: %s", errText)
	}
	rawValues, _ := first["values"].([]any)
	rows := make([][]any, 0, len(rawValues))
	for _, rv := range rawValues {
		if row, ok := rv.([]any); ok {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (a *RqliteAuthority) post(endpoint string, payload any) (map[string]any, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal rqlite request: %w", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build rqlite request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		a.unhealthy.Store(true)
		return nil, fmt.Errorf("rqlite unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		a.unhealthy.Store(true)
		return nil, fmt.Errorf("rqlite HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		a.unhealthy.Store(true)
		return nil, fmt.Errorf("rqlite response undecodable: %w", err)
	}
	a.unhealthy.Store(false)
	return decoded, nil
}
