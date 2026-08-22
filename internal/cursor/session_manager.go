package cursor

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
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

// BindPending indexes a tool_call_id to its owning session. Keys are
// normalised so a client that echoes back a protocol-sanitised id still
// resolves to the session that issued the call.
func (m *SessionManager) BindPending(toolCallID string, session *Session) {
	key := NormalizeToolCallID(toolCallID)
	if m == nil || session == nil || key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[key] = session
	session.touch()
}

// UnbindPending removes a tool_call_id index.
func (m *SessionManager) UnbindPending(toolCallID string) {
	key := NormalizeToolCallID(toolCallID)
	if m == nil || key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pending, key)
}

// ErrToolSessionLost reports that the Agent run a tool result belongs to is no
// longer available: it expired, the gateway restarted, or the run was torn
// down. Callers replay the conversation instead of failing the request.
var ErrToolSessionLost = errors.New("cursor: tool call session is no longer available")

// ResolveForToolResults finds the single live session that owns all results.
func (m *SessionManager) ResolveForToolResults(results []ToolResult) (*Session, error) {
	if len(results) == 0 {
		return nil, fmt.Errorf("cursor: no tool results")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var owner *Session
	for _, result := range results {
		key := NormalizeToolCallID(result.ToolCallID)
		if key == "" {
			return nil, fmt.Errorf("cursor: tool result missing tool_call_id")
		}
		session := m.pending[key]
		if session == nil {
			log.Debugf("cursor session lookup miss: tool_call_id=%s live_sessions=%d pending_calls=%d", key, len(m.sessions), len(m.pending))
			return nil, fmt.Errorf("%w (tool_call_id %s)", ErrToolSessionLost, key)
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

// CloseSupersededConversation closes live sessions parked on conversationID
// waiting for client tool results. A checkpoint resume is about to fork that
// conversation onto a new run, which means the client abandoned the branch
// the parked session is holding open; without this the leaked stream idles
// until the sweep while its pending tool calls shadow the conversation.
// Sessions that are actively streaming are left alone.
func (m *SessionManager) CloseSupersededConversation(conversationID string) {
	if m == nil || strings.TrimSpace(conversationID) == "" {
		return
	}
	m.mu.Lock()
	candidates := make([]*Session, 0, 1)
	for _, session := range m.sessions {
		if session.ConversationID == conversationID {
			candidates = append(candidates, session)
		}
	}
	m.mu.Unlock()
	for _, session := range candidates {
		session.mu.Lock()
		parked := session.waitingTools && !session.closed
		session.mu.Unlock()
		if !parked {
			continue
		}
		log.Infof("cursor: closing superseded tool-wait session %s conv=%s", session.ID, conversationID)
		_ = session.closeWith("superseded_by_resume")
	}
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
