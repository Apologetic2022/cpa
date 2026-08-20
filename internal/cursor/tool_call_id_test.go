package cursor

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

// Cursor's own ids carry a newline, which Claude's tool_use.id rule forbids.
func TestNormalizeToolCallIDSurvivesClaudeSanitizer(t *testing.T) {
	raw := "call-13606af0-9595-4bb1-9625-99a61f6847bb-0\nfc_fca72074-4332-97d1-b68a-c16adee45add_0"
	normalized := NormalizeToolCallID(raw)
	if strings.ContainsAny(normalized, "\n\r ") {
		t.Fatalf("normalized id still has whitespace: %q", normalized)
	}
	if got := util.SanitizeClaudeToolID(normalized); got != normalized {
		t.Fatalf("claude sanitizer rewrote the id: %q -> %q", normalized, got)
	}
}

func TestNormalizeToolCallIDLeavesCleanIDsAlone(t *testing.T) {
	if got := NormalizeToolCallID("  call_abc-123  "); got != "call_abc-123" {
		t.Fatalf("unexpected rewrite: %q", got)
	}
}

// A client that echoes the sanitised id must still find the pending call.
func TestSessionManagerResolvesSanitizedToolCallID(t *testing.T) {
	manager := &SessionManager{sessions: map[string]*Session{}, pending: map[string]*Session{}}
	session := &Session{ID: "s1"}
	raw := "call-abc-0\nfc_def_0"
	manager.BindPending(raw, session)

	owner, err := manager.ResolveForToolResults([]ToolResult{{ToolCallID: util.SanitizeClaudeToolID(raw)}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if owner != session {
		t.Fatalf("resolved wrong session: %#v", owner)
	}
}

func TestSessionManagerReportsLostSession(t *testing.T) {
	manager := &SessionManager{sessions: map[string]*Session{}, pending: map[string]*Session{}}
	_, err := manager.ResolveForToolResults([]ToolResult{{ToolCallID: "gone"}})
	if err == nil {
		t.Fatal("expected an error for an unknown tool call")
	}
	if !strings.Contains(err.Error(), "no longer available") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReplayMessagesForLostSessionEndsWithUserTurn(t *testing.T) {
	messages := []ChatMessage{
		{Role: "user", Content: "weather?"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "get_weather"}}},
		{Role: "tool", ToolCallID: "c1", Name: "get_weather", Content: "22C, sunny"},
	}
	replayed := ReplayMessagesForLostSession(messages, extractToolResults(messages))
	if len(replayed) != len(messages)+1 {
		t.Fatalf("expected one appended message, got %d", len(replayed))
	}
	last := replayed[len(replayed)-1]
	if last.Role != "user" {
		t.Fatalf("expected a trailing user turn, got %q", last.Role)
	}
	if !strings.Contains(last.Content, "22C, sunny") || !strings.Contains(last.Content, "get_weather") {
		t.Fatalf("tool output missing from replay: %q", last.Content)
	}
	// buildRunRequest needs the replayed turn to become the active prompt.
	if _, _, _, err := buildRunRequest("default", replayed, nil); err != nil {
		t.Fatalf("build run request: %v", err)
	}
}
