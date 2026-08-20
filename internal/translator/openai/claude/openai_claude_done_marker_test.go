package claude

import (
	"context"
	"strings"
	"testing"
)

// Executors such as the Cursor one terminate a stream with a bare "[DONE]"
// marker. Without the terminal Anthropic events the client sees a truncated
// message and reconnects.
func TestBareDoneMarkerClosesClaudeStream(t *testing.T) {
	for _, marker := range []string{"[DONE]", "data: [DONE]"} {
		t.Run(marker, func(t *testing.T) {
			ctx := context.Background()
			original := []byte(`{"model":"grok-4.6","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			var param any
			feed := func(line string) []string {
				out := ConvertOpenAIResponseToClaude(ctx, "grok-4.6", original, original, []byte(line), &param)
				events := make([]string, 0, len(out))
				for _, chunk := range out {
					events = append(events, string(chunk))
				}
				return events
			}
			feed(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"grok-4.6","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`)
			feed(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"grok-4.6","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
			tail := strings.Join(feed(marker), "")
			if !strings.Contains(tail, "event: message_delta") {
				t.Fatalf("missing message_delta for %q: %q", marker, tail)
			}
			if !strings.Contains(tail, "event: message_stop") {
				t.Fatalf("missing message_stop for %q: %q", marker, tail)
			}
			if !strings.Contains(tail, `"stop_reason":"end_turn"`) {
				t.Fatalf("missing stop_reason for %q: %q", marker, tail)
			}
		})
	}
}

func TestBareDoneMarkerReportsToolUseStop(t *testing.T) {
	ctx := context.Background()
	original := []byte(`{"model":"grok-4.6","stream":true,"messages":[{"role":"user","content":"weather?"}]}`)
	var param any
	feed := func(line string) string {
		out := ConvertOpenAIResponseToClaude(ctx, "grok-4.6", original, original, []byte(line), &param)
		var sb strings.Builder
		for _, chunk := range out {
			sb.Write(chunk)
		}
		return sb.String()
	}
	feed(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"grok-4.6","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},"finish_reason":null}]}`)
	feed(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"grok-4.6","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
	tail := feed("[DONE]")
	if !strings.Contains(tail, `"stop_reason":"tool_use"`) {
		t.Fatalf("expected tool_use stop reason, got %q", tail)
	}
	if !strings.Contains(tail, "event: message_stop") {
		t.Fatalf("missing message_stop: %q", tail)
	}
}
