package relay

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// Authority is the persistent, fail-closed affinity + ban/quarantine authority
// (docs F8/F20): strict 1:1 tenant->account lifetime bindings and the ban FSM,
// read synchronously before every dispatch. Any read/write failure denies dispatch
// (fail-closed); a stale or missing authority is never trusted optimistically.
type Authority interface {
	// Healthy reports whether the authority is currently readable.
	Healthy() bool
	// BindTenant enforces the lifetime 1:1 binding (first use binds; conflict -> 409).
	BindTenant(tenantID, accountID string) error
	// CheckDispatch is the synchronous fail-closed ban gate run before every dispatch.
	CheckDispatch(accountID string) error
	// Quarantine removes an account from dispatch immediately (no auto-retry).
	Quarantine(accountID, reason string) error
	// Ban marks an account terminally banned (operator-only; irreversible via this API).
	Ban(accountID, reason string) error
	// Reinstate clears quarantine after operator review.
	Reinstate(accountID string) error
	// Snapshot copies tenant bindings and account states for management surfaces.
	Snapshot() (map[string]string, map[string]AccountState)
}

// NewAuthority builds the configured authority (docs F8/F20). Default driver is
// rqlite (replicated Raft store with linearizable reads); "file" is a single-node
// JSON stand-in for dev/tests only.
func NewAuthority(cfg config.RelayConfig, authDir string) (Authority, error) {
	driver := strings.ToLower(strings.TrimSpace(cfg.Store.Driver))
	switch driver {
	case "", "rqlite":
		url := strings.TrimSpace(cfg.Store.RqliteURL)
		if url == "" {
			url = "http://127.0.0.1:4001"
		}
		return NewRqliteAuthority(url)
	case "file":
		path := strings.TrimSpace(cfg.Store.Path)
		if path == "" {
			path = strings.TrimSuffix(authDir, "/") + "/relay-store.json"
		}
		return NewStore(path), nil
	default:
		return nil, fmt.Errorf("relay store driver %q unknown (rqlite|file)", cfg.Store.Driver)
	}
}
