package executor

import (
	"strings"
	"testing"

	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
	"github.com/tidwall/gjson"
)

func TestRewriteImageChatMessagesUsesLastUserPrompt(t *testing.T) {
	messages := []cursorlib.ChatMessage{
		{Role: "system", Content: "be helpful"},
		{Role: "user", Content: "draw a cat"},
		{Role: "assistant", Content: "done"},
		{Role: "user", Content: "now a red fox"},
	}
	got, err := rewriteImageChatMessages(messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Role != "user" {
		t.Fatalf("unexpected messages: %#v", got)
	}
	if !strings.Contains(got[0].Content, "now a red fox") {
		t.Fatalf("prompt missing from instruction: %q", got[0].Content)
	}
}

func TestRewriteImageChatMessagesRequiresUserPrompt(t *testing.T) {
	if _, err := rewriteImageChatMessages([]cursorlib.ChatMessage{{Role: "system", Content: "x"}}); err == nil {
		t.Fatal("expected error for missing user prompt")
	}
}

func TestCursorImagesPrompt(t *testing.T) {
	prompt, err := cursorImagesPrompt([]byte(`{"model":"cursor-image","prompt":"a red fox"}`))
	if err != nil || prompt != "a red fox" {
		t.Fatalf("prompt = %q err = %v", prompt, err)
	}
	if _, err = cursorImagesPrompt([]byte(`{"model":"cursor-image"}`)); err == nil {
		t.Fatal("expected error for missing prompt")
	}
	if _, err = cursorImagesPrompt([]byte(`{"prompt":"x","image":{"url":"data:..."}}`)); err == nil {
		t.Fatal("expected error for edit request")
	}
	if _, err = cursorImagesPrompt([]byte("--multipart--")); err == nil {
		t.Fatal("expected error for non-JSON body")
	}
}

func TestBuildCursorImagesResponse(t *testing.T) {
	payload := buildCursorImagesResponse([]cursorlib.GeneratedImage{
		{Base64: "QUJD", MimeType: "image/png"},
		{Base64: "REVG"},
	})
	if !gjson.ValidBytes(payload) {
		t.Fatalf("invalid JSON: %s", payload)
	}
	data := gjson.GetBytes(payload, "data")
	if !data.IsArray() || len(data.Array()) != 2 {
		t.Fatalf("unexpected data: %s", payload)
	}
	if data.Array()[0].Get("b64_json").String() != "QUJD" {
		t.Fatalf("missing b64_json: %s", payload)
	}
	if data.Array()[0].Get("mime_type").String() != "image/png" {
		t.Fatalf("missing mime_type: %s", payload)
	}
	if gjson.GetBytes(payload, "created").Int() <= 0 {
		t.Fatalf("missing created: %s", payload)
	}
}

func TestBuildOpenAIChatCompletionIncludesImages(t *testing.T) {
	result := &cursorlib.ChatResult{
		Text: "here you go",
		Images: []cursorlib.GeneratedImage{
			{Base64: "QUJD", MimeType: "image/png"},
		},
	}
	payload := buildOpenAIChatCompletion("cursor-image", result)
	images := gjson.GetBytes(payload, "choices.0.message.images")
	if !images.IsArray() || len(images.Array()) != 1 {
		t.Fatalf("missing images array: %s", payload)
	}
	url := images.Array()[0].Get("image_url.url").String()
	if url != "data:image/png;base64,QUJD" {
		t.Fatalf("unexpected image url: %q", url)
	}
}
