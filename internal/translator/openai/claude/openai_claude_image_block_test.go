package claude

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const tinyPNGDataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// A markdown data: URL is replaced by "[Image blocked: …]" in clients that
// harden their renderer, so the bytes have to arrive as a content block.
func TestNonStreamingImagesBecomeContentBlocks(t *testing.T) {
	body := `{"id":"c1","object":"chat.completion","model":"grok-4.6","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"已生成一张图。","images":[{"index":0,"type":"image_url","image_url":{"url":"` + tinyPNGDataURL + `"}}]}}]}`
	var param any
	out := ConvertOpenAIResponseToClaude(context.Background(), "grok-4.6", []byte(`{"model":"grok-4.6"}`), nil, []byte("data: "+body), &param)
	if len(out) != 1 {
		t.Fatalf("expected one response, got %d", len(out))
	}
	content := gjson.GetBytes(out[0], "content")
	types := make([]string, 0, len(content.Array()))
	for _, block := range content.Array() {
		types = append(types, block.Get("type").String())
	}
	if len(types) != 2 || types[0] != "text" || types[1] != "image" {
		t.Fatalf("unexpected content blocks %v: %s", types, out[0])
	}
	image := content.Array()[1]
	if got := image.Get("source.type").String(); got != "base64" {
		t.Fatalf("source.type = %q", got)
	}
	if got := image.Get("source.media_type").String(); got != "image/png" {
		t.Fatalf("media_type = %q", got)
	}
	if got := image.Get("source.data").String(); !strings.HasPrefix(got, "iVBORw0KGgo") {
		t.Fatalf("payload is not the raw base64: %.20s", got)
	}
	if strings.Contains(image.Get("source.data").String(), "data:") {
		t.Fatal("payload still carries the data: prefix")
	}
}

func TestStreamingImagesBecomeContentBlocks(t *testing.T) {
	ctx := context.Background()
	original := []byte(`{"model":"grok-4.6","stream":true,"messages":[{"role":"user","content":"draw"}]}`)
	var param any
	feed := func(line string) string {
		var sb strings.Builder
		for _, chunk := range ConvertOpenAIResponseToClaude(ctx, "grok-4.6", original, original, []byte(line), &param) {
			sb.Write(chunk)
		}
		return sb.String()
	}
	feed(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"grok-4.6","choices":[{"index":0,"delta":{"role":"assistant","content":"已生成一张图。"},"finish_reason":null}]}`)
	imageEvents := feed(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"grok-4.6","choices":[{"index":0,"delta":{"images":[{"index":0,"type":"image_url","image_url":{"url":"` + tinyPNGDataURL + `"}}]},"finish_reason":null}]}`)
	tail := feed(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"grok-4.6","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) + feed("[DONE]")

	if !strings.Contains(imageEvents, `"content_block":{"type":"image"`) {
		t.Fatalf("no image content block emitted: %s", imageEvents)
	}
	if !strings.Contains(imageEvents, `"media_type":"image/png"`) || !strings.Contains(imageEvents, `"type":"base64"`) {
		t.Fatalf("image block is not a base64 source: %s", imageEvents)
	}
	// The text block must be closed before the image opens, and the image
	// block must be closed itself, or the client waits forever.
	textStop := strings.Index(imageEvents, `{"type":"content_block_stop","index":0}`)
	imageStart := strings.Index(imageEvents, `"content_block":{"type":"image"`)
	if textStop < 0 || imageStart < 0 || textStop > imageStart {
		t.Fatalf("blocks are not properly sequenced: %s", imageEvents)
	}
	if !strings.Contains(imageEvents, `{"type":"content_block_stop","index":1}`) {
		t.Fatalf("image block left open: %s", imageEvents)
	}
	if !strings.Contains(tail, "event: message_stop") {
		t.Fatalf("stream not terminated: %s", tail)
	}
}

// The non-streaming /v1/messages handler goes through its own converter, so it
// needs the same image channel as the streaming one.
func TestNonStreamConverterCarriesImageBlock(t *testing.T) {
	body := `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"已生成一张图。","reasoning_content":"thinking","images":[{"index":0,"type":"image_url","image_url":{"url":"` + tinyPNGDataURL + `"}}]}}]}`
	var param any
	out := ConvertOpenAIResponseToClaudeNonStream(context.Background(), "grok-4.6", []byte(`{"model":"grok-4.6"}`), nil, []byte(body), &param)

	content := gjson.GetBytes(out, "content")
	types := make([]string, 0, len(content.Array()))
	for _, block := range content.Array() {
		types = append(types, block.Get("type").String())
	}
	if len(types) != 3 || types[0] != "text" || types[1] != "image" || types[2] != "thinking" {
		t.Fatalf("unexpected content blocks %v: %s", types, out)
	}
	if got := content.Array()[1].Get("source.media_type").String(); got != "image/png" {
		t.Fatalf("media_type = %q", got)
	}
	if text := content.Array()[0].Get("text").String(); strings.Contains(text, "data:") {
		t.Fatalf("payload leaked into the text: %q", text)
	}
}

func TestNonImageDataURLsAreIgnored(t *testing.T) {
	body := `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"hi","images":[{"image_url":{"url":"https://example.com/a.png"}},{"image_url":{"url":"data:text/plain;base64,aGk="}}]}}]}`
	var param any
	out := ConvertOpenAIResponseToClaude(context.Background(), "m", []byte(`{"model":"m"}`), nil, []byte("data: "+body), &param)
	if strings.Contains(string(out[0]), `"type":"image"`) {
		t.Fatalf("non-inline image was turned into a block: %s", out[0])
	}
}
