package claude

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// A generated image reaches an Anthropic client only through the assistant's
// text: the Messages protocol has no image block in a reply, and the
// OpenAI-only "images" array is dropped in translation. These tests pin the
// markdown link the Cursor executor emits to the bytes a /v1/messages caller
// actually receives.

const generatedImageMarkdown = "![Generated image](https://gw.example.com/media/0123456789abcdef0123456789abcdef.png)"

func TestClaudeStreamCarriesGeneratedImageLink(t *testing.T) {
	const head = `{"id":"chatcmpl_1","model":"image","created":1,`
	events := runStream(t, `{"model":"image","stream":true,"messages":[]}`,
		head+`"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		head+`"choices":[{"index":0,"delta":{"content":"已经生成一张写实肖像。"},"finish_reason":null}]}`,
		head+`"choices":[{"index":0,"delta":{"content":"\n\n`+generatedImageMarkdown+`\n\n"},"finish_reason":null}]}`,
		head+`"choices":[{"index":0,"delta":{"images":[{"index":0,"type":"image_url","image_url":{"url":"https://gw.example.com/media/0123456789abcdef0123456789abcdef.png"}}]},"finish_reason":null}]}`,
		head+`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)

	var text strings.Builder
	for _, ev := range events {
		if ev.Type != "content_block_delta" {
			continue
		}
		if gjson.Get(ev.Payload, "delta.type").String() == "text_delta" {
			text.WriteString(gjson.Get(ev.Payload, "delta.text").String())
		}
	}
	if !strings.Contains(text.String(), generatedImageMarkdown) {
		t.Fatalf("generated image link missing from claude stream: %q", text.String())
	}
}

func TestClaudeNonStreamCarriesGeneratedImageLink(t *testing.T) {
	payload := `{"id":"chatcmpl-1","object":"chat.completion","model":"image","choices":[{"index":0,"message":{"role":"assistant","content":"图片如下：\n\n` + generatedImageMarkdown + `","images":[{"index":0,"type":"image_url","image_url":{"url":"https://gw.example.com/media/0123456789abcdef0123456789abcdef.png"}}]},"finish_reason":"stop"}]}`

	var param any
	out := ConvertOpenAIResponseToClaudeNonStream(
		context.Background(),
		"",
		[]byte(`{"model":"image","messages":[]}`),
		nil,
		[]byte(payload),
		&param,
	)

	blocks := gjson.GetBytes(out, "content")
	if !blocks.IsArray() || len(blocks.Array()) == 0 {
		t.Fatalf("no content blocks: %s", out)
	}
	text := blocks.Array()[0].Get("text").String()
	if !strings.Contains(text, generatedImageMarkdown) {
		t.Fatalf("generated image link missing from claude response: %q", text)
	}
}
