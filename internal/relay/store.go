package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AccountState is the ban/quarantine FSM state for one account (docs F20).
type AccountState string

const (
	// StateActive means dispatchable.
	StateActive AccountState = "active"
	// StateQuarantine means removed from dispatch immediately, no auto-retry, paging ops.
	// Triggered by disabled-account body signatures or operator action.
	StateQuarantine AccountState = "quarantine"
	// StateBanned means confirmed terminated; never dispatch again.
	StateBanned AccountState = "banned"
)

// banSignatures are upstream body markers that mean immediate quarantine on sight
// (docs 3d/6.7.3: "403 …only authorized for use with Claude Code…" / disabled-body =
// immediate quarantine, no automatic retry, page ops).
var banSignatures = []string{
	"only authorized for use with claude code",
	"account has been disabled",
	"disabled after an automatic review",
	"account is disabled",
	"organization has been disabled",
}

// DetectBanSignature scans an upstream body (error frame or HTTP error payload) for a
// ban/quarantine signature. Returns the matched signature, if any.
func DetectBanSignature(body []byte) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	lower := strings.ToLower(string(body))
	for _, sig := range banSignatures {
		if strings.Contains(lower, sig) {
			return sig, true
		}
	}
	return "", false
}

// accountRecord is the persisted per-account FSM record.
type accountRecord struct {
	State     AccountState `json:"state"`
	Reason    string       `json:"reason,omitempty"`
	Since     time.Time    `json:"since"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// storeData is the on-disk shape.
type storeData struct {
	Tenants  map[string]string         `json:"tenants"`  // tenantID -> accountID, lifetime 1:1
	Accounts map[string]*accountRecord `json:"accounts"` // accountID -> FSM record
}

// Store is the persistent affinity + ban/quarantine authority (docs F8/F20).
//
// Local stand-in for the docs' rqlite/Raft store: a JSON file with atomic replace and
// mutex-serialized linear reads/writes. The interface deliberately mirrors the
// authority semantics so a replicated store can replace it without touching callers.
// Reads are fail-closed: a corrupt or unreadable store denies every dispatch.
type Store struct {
	path string

	mu       sync.RWMutex
	data     storeData
	loadErr  error // non-nil => fail-closed for all dispatch
	haveLoad bool
}

// NewStore loads (or initializes) the store at path. A missing file is an empty store;
// a corrupt file is fail-closed (never silently reset — operator attention required).
func NewStore(path string) *Store {
	s := &Store{path: path}
	s.data.Tenants = make(map[string]string)
	s.data.Accounts = make(map[string]*accountRecord)
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		var d storeData
		if uerr := json.Unmarshal(raw, &d); uerr != nil {
			s.loadErr = fmt.Errorf("relay store corrupt at %s: %w (fail-closed: all dispatch denied)", path, uerr)
			return s
		}
		if d.Tenants != nil {
			s.data.Tenants = d.Tenants
		}
		if d.Accounts != nil {
			s.data.Accounts = d.Accounts
		}
		s.haveLoad = true
	case errors.Is(err, os.ErrNotExist):
		s.haveLoad = true
	default:
		s.loadErr = fmt.Errorf("relay store unreadable at %s: %w (fail-closed: all dispatch denied)", path, err)
	}
	return s
}

// Healthy reports whether the store is readable (dispatch authority available).
func (s *Store) Healthy() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadErr == nil
}

// BindTenant enforces the strict 1 tenant : 1 account lifetime binding (docs S1/6.7.3).
// First use binds tenant->account and persists; a later turn whose selected account
// differs from the binding is rejected — saturated tenants queue or 429, they never
// spill over to another account.
func (s *Store) BindTenant(tenantID, accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return &BanError{Reason: "relay store unavailable (fail-closed): " + s.loadErr.Error()}
	}
	if bound, ok := s.data.Tenants[tenantID]; ok {
		if bound != accountID {
			return &BanError{
				Status: 409,
				Reason: fmt.Sprintf("tenant is lifetime-bound to a different account (strict 1:1, no spillover); bound=%s requested=%s", bound, accountID),
			}
		}
		return nil
	}
	s.data.Tenants[tenantID] = accountID
	return s.persistLocked()
}

// CheckDispatch is the synchronous, fail-closed ban/quarantine gate run before every
// dispatch (docs F20): stale/missing authority denies; quarantined/banned denies.
func (s *Store) CheckDispatch(accountID string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.loadErr != nil {
		return &BanError{Reason: "relay store unavailable (fail-closed): " + s.loadErr.Error()}
	}
	rec, ok := s.data.Accounts[accountID]
	if !ok {
		return nil // no record: never flagged
	}
	switch rec.State {
	case StateQuarantine:
		return &BanError{Status: 503, Reason: fmt.Sprintf("account quarantined since %s (%s); no auto-retry, operator action required", rec.Since.Format(time.RFC3339), rec.Reason)}
	case StateBanned:
		return &BanError{Status: 410, Reason: fmt.Sprintf("account banned (%s); permanently removed from dispatch", rec.Reason)}
	}
	return nil
}

// Quarantine moves an account to quarantine immediately (docs: disabled-body = instant
// quarantine, no automatic retry, page ops). Idempotent; never downgrades Banned.
func (s *Store) Quarantine(accountID, reason string) error {
	return s.setState(accountID, StateQuarantine, reason)
}

// Ban marks an account confirmed-banned (terminal). Operator-only transition.
func (s *Store) Ban(accountID, reason string) error {
	return s.setState(accountID, StateBanned, reason)
}

// Reinstate clears quarantine after operator review (docs: 误报恢复).
func (s *Store) Reinstate(accountID string) error {
	return s.setState(accountID, StateActive, "")
}

func (s *Store) setState(accountID string, state AccountState, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return fmt.Errorf("relay store unavailable: %w", s.loadErr)
	}
	rec, ok := s.data.Accounts[accountID]
	if !ok {
		rec = &accountRecord{Since: time.Now()}
		s.data.Accounts[accountID] = rec
	}
	if rec.State == StateBanned && state != StateBanned {
		return fmt.Errorf("account %s is terminally banned; refusing state downgrade", accountID)
	}
	if rec.State == state && rec.Reason == reason {
		return nil
	}
	rec.State = state
	rec.Reason = reason
	rec.UpdatedAt = time.Now()
	if state != StateActive && rec.Since.IsZero() {
		rec.Since = time.Now()
	}
	return s.persistLocked()
}

// Snapshot returns a copy of tenant bindings and account states (management surface).
func (s *Store) Snapshot() (map[string]string, map[string]AccountState) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tenants := make(map[string]string, len(s.data.Tenants))
	for k, v := range s.data.Tenants {
		tenants[k] = v
	}
	states := make(map[string]AccountState, len(s.data.Accounts))
	for k, v := range s.data.Accounts {
		states[k] = v.State
	}
	return tenants, states
}

// persistLocked writes the store atomically (tmp + rename). Caller holds mu.
func (s *Store) persistLocked() error {
	raw, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal relay store: %w", err)
	}
	dir := filepath.Dir(s.path)
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		return fmt.Errorf("create relay store dir: %w", mkErr)
	}
	tmp, err := os.CreateTemp(dir, ".relay-store-*.tmp")
	if err != nil {
		return fmt.Errorf("create relay store tmp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err = tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write relay store tmp: %w", err)
	}
	if err = tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close relay store tmp: %w", err)
	}
	// Windows cannot rename over an existing file; remove then rename, with restore on
	// failure. Crash between remove and rename loses the old file but never leaves a
	// torn file visible (fail-closed reads recover operator attention).
	_ = os.Remove(s.path)
	if err = os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("commit relay store: %w", err)
	}
	return nil
}
