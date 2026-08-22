package cursor

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestExtractToolResultsTrailingOnly(t *testing.T) {
	messages := []ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "old", Name: "x", Arguments: map[string]any{}}}},
		{Role: "tool", ToolCallID: "old", Content: "stale"},
		{Role: "user", Content: "again"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "c1", Name: "get_weather", Arguments: map[string]any{"city": "NY"}}}},
		{Role: "tool", ToolCallID: "c1", Name: "get_weather", Content: `{"ok":true}`},
	}
	got := extractToolResults(messages)
	if len(got) != 1 || got[0].ToolCallID != "c1" {
		t.Fatalf("expected trailing c1, got %#v", got)
	}
}

// Cursor's server sometimes runs GenerateImage without asking the client
// first, so a run that may not generate has to be told up front — otherwise it
// spends the turn producing an image the caller never receives.
func TestBuildRunRequestTellsPlainChatsImagesAreUnavailable(t *testing.T) {
	systemPromptFor := func(allowImages bool) string {
		_, blobs, _, err := buildRunRequest("default", []ChatMessage{
			{Role: "user", Content: "draw me a fox"},
		}, nil, allowImages)
		if err != nil {
			t.Fatal(err)
		}
		for _, data := range blobs {
			var payload map[string]any
			if json.Unmarshal(data, &payload) != nil {
				continue
			}
			if payload["role"] != "system" {
				continue
			}
			if content, ok := payload["content"].(string); ok {
				return content
			}
		}
		t.Fatal("run request carries no system prompt")
		return ""
	}

	if got := systemPromptFor(false); !strings.Contains(got, "cannot generate images") {
		t.Fatalf("plain chat is not told images are unavailable: %q", got)
	}
	if got := systemPromptFor(true); strings.Contains(got, "cannot generate images") {
		t.Fatalf("the image model was told it cannot generate: %q", got)
	}
}

func TestBuildRunRequestIncludesMcpTools(t *testing.T) {
	msg, _, _, err := buildRunRequest("default", []ChatMessage{
		{Role: "user", Content: "weather?"},
	}, []ToolDefinition{{
		Name:        "get_weather",
		Description: "weather",
		Parameters:  map[string]any{"type": "object"},
	}}, false)
	if err != nil {
		t.Fatal(err)
	}
	run := msg.GetRunRequest()
	if run == nil || run.GetMcpTools() == nil || len(run.GetMcpTools().GetMcpTools()) != 1 {
		t.Fatalf("expected mcp tools on run request, got %#v", run.GetMcpTools())
	}
	tool := run.GetMcpTools().GetMcpTools()[0]
	if tool.GetProviderIdentifier() != MCPProviderIdentifier || tool.GetName() != "get_weather" {
		t.Fatalf("unexpected tool: %#v", tool)
	}
}

func TestBuildRunRequestNativeToolJSON(t *testing.T) {
	msg, blobs, _, err := buildRunRequest("default", []ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "thinking", ToolCalls: []ToolCall{
			{ID: "c1", Name: "get_weather", Arguments: map[string]any{"city": "NY"}},
		}},
		{Role: "tool", ToolCallID: "c1", Name: "get_weather", Content: `{"ok":true}`},
		{Role: "user", Content: "again"},
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if msg.GetRunRequest() == nil {
		t.Fatal("missing run request")
	}
	var sawToolCall, sawToolResult bool
	for _, data := range blobs {
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			continue
		}
		role, _ := payload["role"].(string)
		content, _ := payload["content"].([]any)
		switch role {
		case "assistant":
			for _, part := range content {
				m, _ := part.(map[string]any)
				if m["type"] == "tool-call" && m["toolCallId"] == "c1" {
					sawToolCall = true
				}
			}
			if _, ok := payload["tool_calls"]; ok {
				t.Fatalf("expected Cursor-native tool-call parts, not OpenAI tool_calls: %#v", payload)
			}
		case "tool":
			if payload["id"] != "c1" {
				continue
			}
			for _, part := range content {
				m, _ := part.(map[string]any)
				if m["type"] == "tool-result" && m["toolCallId"] == "c1" {
					sawToolResult = true
				}
			}
			if _, ok := payload["tool_call_id"]; ok {
				t.Fatalf("expected Cursor-native tool id/content, not tool_call_id: %#v", payload)
			}
		}
	}
	if !sawToolCall || !sawToolResult {
		t.Fatalf("native tool history missing call=%v result=%v", sawToolCall, sawToolResult)
	}
}

func TestBuildRunRequestFoldsToolHistoryForClaude(t *testing.T) {
	// Cursor's Anthropic upstream rejects replayed histories with structured
	// tool-call/tool-result parts (thinking blocks cannot be restored), so a
	// claude replay must fold them into plain text.
	msg, blobs, _, err := buildRunRequest("claude-sonnet-5", []ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "checking", ToolCalls: []ToolCall{
			{ID: "c1", Name: "get_weather", Arguments: map[string]any{"city": "NY"}},
		}},
		{Role: "tool", ToolCallID: "c1", Name: "get_weather", Content: `{"ok":true}`},
		{Role: "user", Content: "again"},
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if msg.GetRunRequest() == nil {
		t.Fatal("missing run request")
	}
	var sawFoldedCall, sawFoldedResult bool
	for _, data := range blobs {
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			continue
		}
		role, _ := payload["role"].(string)
		if role == "tool" {
			t.Fatalf("claude replay must not contain tool-role blobs: %#v", payload)
		}
		content, _ := payload["content"].([]any)
		for _, part := range content {
			m, _ := part.(map[string]any)
			switch m["type"] {
			case "tool-call", "tool-result":
				t.Fatalf("claude replay must not contain structured tool parts: %#v", payload)
			case "text":
				text, _ := m["text"].(string)
				if role == "assistant" && strings.Contains(text, "get_weather") && strings.Contains(text, "c1") {
					sawFoldedCall = true
				}
				if role == "user" && strings.Contains(text, `{"ok":true}`) && strings.Contains(text, "c1") {
					sawFoldedResult = true
				}
			}
		}
	}
	if !sawFoldedCall || !sawFoldedResult {
		t.Fatalf("folded tool history missing call=%v result=%v", sawFoldedCall, sawFoldedResult)
	}
}

func TestCookieJarRemembersSetCookie(t *testing.T) {
	jar := &CookieJar{byHost: map[string]map[string]string{}}
	hdr := make(http.Header)
	hdr.Add("Set-Cookie", "CursorCookie=server-issued; Path=/; HttpOnly")
	jar.RememberResponse("https://api2.cursor.sh", hdr)
	got := jar.Header("https://api2.cursor.sh")
	if !strings.Contains(got, "CursorCookie=server-issued") {
		t.Fatalf("expected server cookie, got %q", got)
	}
}

func TestProtobufValueRoundTrip(t *testing.T) {
	in := map[string]any{"city": "北京", "n": float64(1), "ok": true}
	v, err := toProtobufValue(in)
	if err != nil {
		t.Fatal(err)
	}
	out := fromProtobufValue(v)
	raw, _ := json.Marshal(out)
	if string(raw) == "" {
		t.Fatal("empty roundtrip")
	}
}
