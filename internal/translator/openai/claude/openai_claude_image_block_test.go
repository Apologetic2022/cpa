package claude

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const tinyPNGDataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// Clients validate every content block against Anthropic's discriminated
// union and abort the turn on an unknown discriminator ("Type validation
// failed on SSE event", "No matching discriminator"). The union has no image
// member — an assistant turn cannot carry image bytes — so an upstream that
// reports generated images must not leak them into the block stream.
var anthropicContentBlockTypes = map[string]bool{
	"text": true, "thinking": true, "redacted_thinking": true, "tool_use": true,
	"compaction": true, "server_tool_use": true, "mcp_tool_use": true,
	"mcp_tool_result": true, "web_fetch_tool_result": true,
	"web_search_tool_result": true, "code_execution_tool_result": true,
	"bash_code_execution_tool_result": true, "advisor_tool_result": true,
	"text_editor_code_execution_tool_result": true, "tool_search_tool_result": true,
}

const imageBearingChunk = `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"grok-4.6","choices":[{"index":0,"delta":{"images":[{"index":0,"type":"image_url","image_url":{"url":"` + tinyPNGDataURL + `"}}]},"finish_reason":null}]}`

const imageBearingResponse = `{"id":"c1","object":"chat.completion","model":"grok-4.6","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"已生成一张图。","reasoning_content":"thinking","images":[{"index":0,"type":"image_url","image_url":{"url":"` + tinyPNGDataURL + `"}}]}}]}`

func assertKnownBlockTypes(t *testing.T, label string, blockTypes []string) {
	t.Helper()
	for _, blockType := range blockTypes {
		if !anthropicContentBlockTypes[blockType] {
			t.Fatalf("%s emitted content block %q, which no Anthropic client accepts", label, blockType)
		}
	}
}

func TestStreamingNeverEmitsUnknownContentBlocks(t *testing.T) {
	ctx := context.Background()
	original := []byte(`{"model":"grok-4.6","stream":true,"messages":[{"role":"user","content":"draw"}]}`)
	var param any
	var raw strings.Builder
	for _, line := range []string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"grok-4.6","choices":[{"index":0,"delta":{"role":"assistant","content":"已生成一张图。"},"finish_reason":null}]}`,
		"data: " + imageBearingChunk,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"grok-4.6","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"[DONE]",
	} {
		for _, chunk := range ConvertOpenAIResponseToClaude(ctx, "grok-4.6", original, original, []byte(line), &param) {
			raw.Write(chunk)
		}
	}

	var blockTypes []string
	for _, line := range strings.Split(raw.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		event := gjson.Parse(strings.TrimPrefix(line, "data: "))
		if event.Get("type").String() == "content_block_start" {
			blockTypes = append(blockTypes, event.Get("content_block.type").String())
		}
	}
	if len(blockTypes) == 0 {
		t.Fatal("no content blocks emitted")
	}
	assertKnownBlockTypes(t, "streaming", blockTypes)
	if !strings.Contains(raw.String(), "event: message_stop") {
		t.Fatal("stream not terminated")
	}
}

func TestNonStreamNeverEmitsUnknownContentBlocks(t *testing.T) {
	var param any
	out := ConvertOpenAIResponseToClaudeNonStream(context.Background(), "grok-4.6", []byte(`{"model":"grok-4.6"}`), nil, []byte(imageBearingResponse), &param)
	collect := func(payload []byte) []string {
		var types []string
		for _, block := range gjson.GetBytes(payload, "content").Array() {
			types = append(types, block.Get("type").String())
		}
		return types
	}
	assertKnownBlockTypes(t, "non-stream converter", collect(out))

	var streamParam any
	aggregated := ConvertOpenAIResponseToClaude(context.Background(), "grok-4.6", []byte(`{"model":"grok-4.6"}`), nil, []byte("data: "+imageBearingResponse), &streamParam)
	if len(aggregated) != 1 {
		t.Fatalf("expected one aggregated response, got %d", len(aggregated))
	}
	assertKnownBlockTypes(t, "aggregating converter", collect(aggregated[0]))
}
