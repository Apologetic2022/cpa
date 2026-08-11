package cursor

import (
	"fmt"
	"sync"
	"time"
)

const (
	sessionIdleTTL    = 30 * time.Minute
	sessionSweepEvery = 2 * time.Minute
)

// SessionManager tracks live Agent sessions waiting on client tool results.
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]*Session // session id
	pending  map[string]*Session // tool_call_id -> session
}

var defaultSessionManager = NewSessionManager()

// DefaultSessionManager returns the process-wide manager.
func DefaultSessionManager() *SessionManager { return defaultSessionManager }

// NewSessionManager creates an empty manager.
func NewSessionManager() *SessionManager {
	m := &SessionManager{
		sessions: map[string]*Session{},
		pending:  map[string]*Session{},
	}
	go m.reaper()
	return m
}

func (m *SessionManager) reaper() {
	ticker := time.NewTicker(sessionSweepEvery)
	defer ticker.Stop()
	for range ticker.C {
		m.Sweep(sessionIdleTTL)
	}
}

// Register stores a live session.
func (m *SessionManager) Register(session *Session) {
	if m == nil || session == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[session.ID] = session
	session.touch()
}

// BindPending indexes a tool_call_id to its owning session.
func (m *SessionManager) BindPending(toolCallID string, session *Session) {
	if m == nil || session == nil || toolCallID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[toolCallID] = session
	session.touch()
}

// UnbindPending removes a tool_call_id index.
func (m *SessionManager) UnbindPending(toolCallID string) {
	if m == nil || toolCallID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pending, toolCallID)
}

// LookupPending returns the session waiting for the given tool call.
func (m *SessionManager) LookupPending(toolCallID string) *Session {
	if m == nil || toolCallID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.pending[toolCallID]
	if session != nil {
		session.touch()
	}
	return session
}

// ResolveForToolResults finds the single live session that owns all results.
func (m *SessionManager) ResolveForToolResults(results []ToolResult) (*Session, error) {
	if len(results) == 0 {
		return nil, fmt.Errorf("cursor: no tool results")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var owner *Session
	for _, result := range results {
		if result.ToolCallID == "" {
			return nil, fmt.Errorf("cursor: tool result missing tool_call_id")
		}
		session := m.pending[result.ToolCallID]
		if session == nil {
			return nil, fmt.Errorf("cursor: unknown or expired tool_call_id %s", result.ToolCallID)
		}
		if owner == nil {
			owner = session
			continue
		}
		if owner.ID != session.ID {
			return nil, fmt.Errorf("cursor: tool results belong to different sessions")
		}
	}
	if owner != nil {
		owner.touch()
	}
	return owner, nil
}

// Remove drops a session and all of its pending indexes.
func (m *SessionManager) Remove(session *Session) {
	if m == nil || session == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, session.ID)
	for id, owner := range m.pending {
		if owner != nil && owner.ID == session.ID {
			delete(m.pending, id)
		}
	}
}

// Sweep closes idle sessions older than ttl.
func (m *SessionManager) Sweep(ttl time.Duration) {
	if m == nil {
		return
	}
	cutoff := time.Now().Add(-ttl)
	m.mu.Lock()
	stale := make([]*Session, 0)
	for _, session := range m.sessions {
		if session.lastActivity.Before(cutoff) {
			stale = append(stale, session)
		}
	}
	m.mu.Unlock()
	for _, session := range stale {
		_ = session.Close()
		m.Remove(session)
	}
}
