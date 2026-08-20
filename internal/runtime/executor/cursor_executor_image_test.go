package executor

import (
	"bytes"
	"encoding/base64"
	"mime/multipart"
	"strings"
	"testing"

	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
	"github.com/tidwall/gjson"
)

// Smallest valid 1x1 PNG, used as a stand-in input image.
var testPNGBase64 = base64.StdEncoding.EncodeToString([]byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E,
	0x44, 0xAE, 0x42, 0x60, 0x82,
})

func TestRewriteImageChatMessagesUsesLastUserPrompt(t *testing.T) {
	messages := []cursorlib.ChatMessage{
		{Role: "system", Content: "be helpful"},
		{Role: "user", Content: "draw a cat"},
		{Role: "assistant", Content: "done"},
		{Role: "user", Content: "now a red fox"},
	}
	got, err := rewriteImageChatMessages(messages, nil)
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
	if _, err := rewriteImageChatMessages([]cursorlib.ChatMessage{{Role: "system", Content: "x"}}, nil); err == nil {
		t.Fatal("expected error for missing user prompt")
	}
}

func TestRewriteImageChatMessagesUsesEditInstructionWithRefs(t *testing.T) {
	messages := []cursorlib.ChatMessage{{Role: "user", Content: "make the hat red"}}
	refs := []cursorlib.ReferenceImage{{Path: "/ws/assets/references/reference-1.png"}}

	got, err := rewriteImageChatMessages(messages, refs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got[0].Content, refs[0].Path) {
		t.Fatalf("edit instruction missing reference path: %q", got[0].Content)
	}
	if !strings.Contains(got[0].Content, "make the hat red") {
		t.Fatalf("edit instruction missing prompt: %q", got[0].Content)
	}

	// Without refs the same turn must stay a plain generation.
	got, err = rewriteImageChatMessages(messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got[0].Content, "reference") {
		t.Fatalf("generation instruction leaked edit wording: %q", got[0].Content)
	}
}

func TestExtractChatImageInputs(t *testing.T) {
	body := []byte(`{"model":"cursor-image","messages":[
      {"role":"user","content":[{"type":"text","text":"first turn"},
                                {"type":"image_url","image_url":{"url":"data:image/png;base64,` + testPNGBase64 + `"}}]},
      {"role":"assistant","content":"ok"},
      {"role":"user","content":[{"type":"text","text":"make the hat red"},
                                {"type":"image_url","image_url":{"url":"data:image/png;base64,` + testPNGBase64 + `"}}]}]}`)
	refs, err := extractChatImageInputs(body)
	if err != nil {
		t.Fatal(err)
	}
	// Only the latest user turn contributes; the earlier image must not leak in.
	if len(refs) != 1 {
		t.Fatalf("expected 1 reference image from the latest turn, got %d", len(refs))
	}
	if refs[0].MimeType != "image/png" || len(refs[0].Data) == 0 {
		t.Fatalf("unexpected reference: %#v", refs[0])
	}
	if !strings.HasSuffix(refs[0].Path, "reference-1.png") {
		t.Fatalf("unexpected path %q", refs[0].Path)
	}
}

func TestExtractChatImageInputsPlainTextIsNoop(t *testing.T) {
	refs, err := extractChatImageInputs([]byte(`{"messages":[{"role":"user","content":"draw a fox"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("plain text turn produced %d refs", len(refs))
	}

	refs, err = extractChatImageInputs([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"draw a fox"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("text-only array produced %d refs", len(refs))
	}
}

func TestExtractChatImageInputsRejectsRemoteURL(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[
      {"type":"text","text":"edit"},
      {"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`)
	if _, err := extractChatImageInputs(body); err == nil {
		t.Fatal("expected error for remote image URL in chat")
	}
}

func TestCursorImagesRequestGeneration(t *testing.T) {
	prompt, refs, err := cursorImagesRequest([]byte(`{"model":"cursor-image","prompt":"a red fox"}`))
	if err != nil || prompt != "a red fox" {
		t.Fatalf("prompt = %q err = %v", prompt, err)
	}
	if len(refs) != 0 {
		t.Fatalf("expected no reference images, got %d", len(refs))
	}
	if _, _, err = cursorImagesRequest([]byte(`{"model":"cursor-image"}`)); err == nil {
		t.Fatal("expected error for missing prompt")
	}
	if _, _, err = cursorImagesRequest([]byte(`not json`)); err == nil {
		t.Fatal("expected error for non-JSON body")
	}
}

func TestCursorImagesRequestJSONEdit(t *testing.T) {
	body := `{"model":"cursor-image","prompt":"make it green","images":[{"image_url":"data:image/png;base64,` + testPNGBase64 + `"}]}`
	prompt, refs, err := cursorImagesRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if prompt != "make it green" {
		t.Fatalf("prompt = %q", prompt)
	}
	if len(refs) != 1 {
		t.Fatalf("expected 1 reference image, got %d", len(refs))
	}
	if refs[0].MimeType != "image/png" {
		t.Fatalf("mime = %q", refs[0].MimeType)
	}
	if !strings.HasSuffix(refs[0].Path, "reference-1.png") {
		t.Fatalf("unexpected reference path %q", refs[0].Path)
	}
	if len(refs[0].Data) == 0 {
		t.Fatal("reference image data is empty")
	}
}

func TestCursorImagesRequestAcceptsBareImageField(t *testing.T) {
	body := `{"prompt":"tweak","image":"data:image/png;base64,` + testPNGBase64 + `"}`
	_, refs, err := cursorImagesRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("expected 1 reference image, got %d", len(refs))
	}
}

func TestCursorImagesRequestRejectsMaskAndRemoteURL(t *testing.T) {
	mask := `{"prompt":"x","images":[{"image_url":"data:image/png;base64,` + testPNGBase64 + `"}],"mask":{"image_url":"data:image/png;base64,x"}}`
	if _, _, err := cursorImagesRequest([]byte(mask)); err == nil {
		t.Fatal("expected error for mask")
	}
	remote := `{"prompt":"x","images":[{"image_url":"https://example.com/a.png"}]}`
	if _, _, err := cursorImagesRequest([]byte(remote)); err == nil {
		t.Fatal("expected error for remote URL")
	}
}

func TestCursorImagesRequestRejectsTooManyImages(t *testing.T) {
	parts := make([]string, 0, cursorMaxReferenceImages+1)
	for i := 0; i <= cursorMaxReferenceImages; i++ {
		parts = append(parts, `{"image_url":"data:image/png;base64,`+testPNGBase64+`"}`)
	}
	body := `{"prompt":"x","images":[` + strings.Join(parts, ",") + `]}`
	if _, _, err := cursorImagesRequest([]byte(body)); err == nil {
		t.Fatalf("expected error for more than %d images", cursorMaxReferenceImages)
	}
}

func TestCursorImagesRequestRejectsNonImageData(t *testing.T) {
	body := `{"prompt":"x","images":[{"image_url":"data:image/png;base64,` +
		base64.StdEncoding.EncodeToString([]byte("this is plain text, not an image at all")) + `"}]}`
	if _, _, err := cursorImagesRequest([]byte(body)); err == nil {
		t.Fatal("expected error for non-image payload")
	}
}

func TestCursorImagesRequestMultipartEdit(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("prompt", "make it blue"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("image", "input.png")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(testPNGBase64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}

	prompt, refs, err := cursorImagesRequest(body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if prompt != "make it blue" {
		t.Fatalf("prompt = %q", prompt)
	}
	if len(refs) != 1 || refs[0].MimeType != "image/png" {
		t.Fatalf("unexpected refs: %#v", refs)
	}
	if !bytes.Equal(refs[0].Data, raw) {
		t.Fatal("multipart image bytes did not round-trip")
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

func TestInlineGeneratedImagesRewritesLocalPath(t *testing.T) {
	imgs := []cursorlib.GeneratedImage{{Base64: "QUJD", MimeType: "image/png", FilePath: "/home/cliproxy/.cursor/projects/w/assets/assets/portrait-woman.png"}}
	text := `已经生成一张写实肖像。<img src="/home/cliproxy/.cursor/projects/w/assets/assets/portrait-woman.png" alt="Generated image" />后续可继续调整。`

	got := inlineGeneratedImages(text, imgs)
	if strings.Contains(got, "/home/cliproxy") {
		t.Fatalf("local path survived rewrite: %q", got)
	}
	if !strings.Contains(got, `src="data:image/png;base64,QUJD"`) {
		t.Fatalf("data URL not inlined: %q", got)
	}
	// Surrounding prose and other attributes must be preserved.
	if !strings.Contains(got, "已经生成一张写实肖像。") || !strings.Contains(got, "后续可继续调整。") {
		t.Fatalf("surrounding text damaged: %q", got)
	}
	if !strings.Contains(got, `alt="Generated image"`) {
		t.Fatalf("alt attribute lost: %q", got)
	}
}

func TestInlineGeneratedImagesFallsBackToOrder(t *testing.T) {
	imgs := []cursorlib.GeneratedImage{{Base64: "QUJD", MimeType: "image/png", FilePath: "somewhere-else.png"}}
	got := inlineGeneratedImages(`<img src="assets/unknown-name.png" />`, imgs)
	if !strings.Contains(got, "data:image/png;base64,QUJD") {
		t.Fatalf("expected order fallback, got %q", got)
	}
}

func TestInlineGeneratedImagesLeavesAddressableSources(t *testing.T) {
	imgs := []cursorlib.GeneratedImage{{Base64: "QUJD", MimeType: "image/png", FilePath: "a.png"}}
	for _, src := range []string{"data:image/png;base64,ZZZ", "https://example.com/a.png"} {
		text := `<img src="` + src + `" />`
		if got := inlineGeneratedImages(text, imgs); got != text {
			t.Fatalf("rewrote an already-addressable src %q -> %q", src, got)
		}
	}
	// No images at all must be a no-op rather than a panic.
	if got := inlineGeneratedImages(`<img src="/x.png" />`, nil); got != `<img src="/x.png" />` {
		t.Fatalf("unexpected rewrite without images: %q", got)
	}
}

func TestStreamImgFilterDropsTagAcrossChunks(t *testing.T) {
	var f streamImgFilter
	// The tag is split across five deltas, mid-attribute.
	chunks := []string{"生成完毕。", `<im`, `g src="/home/cli`, `proxy/a.png" alt="x"`, ` />剩余文本`}
	var got strings.Builder
	for _, c := range chunks {
		got.WriteString(f.Feed(c))
	}
	got.WriteString(f.Flush())

	if want := "生成完毕。剩余文本"; got.String() != want {
		t.Fatalf("filtered text = %q, want %q", got.String(), want)
	}
}

func TestStreamImgFilterKeepsNonLocalTags(t *testing.T) {
	// A model quoting HTML must come back byte-for-byte: only tags pointing at
	// the agent's filesystem are ours to remove.
	for _, tag := range []string{
		`<img>`,
		`<img alt="no src" />`,
		`<img src="data:image/png;base64,QUJD" />`,
		`<img src="https://example.com/a.png" />`,
	} {
		var f streamImgFilter
		got := f.Feed("before "+tag+" after") + f.Flush()
		if want := "before " + tag + " after"; got != want {
			t.Fatalf("filter altered %q -> %q", want, got)
		}
	}
}

func TestStreamImgFilterPassesPlainText(t *testing.T) {
	var f streamImgFilter
	var got strings.Builder
	for _, c := range []string{"a < b", " and ", "c > d"} {
		got.WriteString(f.Feed(c))
	}
	got.WriteString(f.Flush())
	if want := "a < b and c > d"; got.String() != want {
		t.Fatalf("plain text mangled: %q, want %q", got.String(), want)
	}
}

func TestStreamImgFilterDropsUnterminatedTag(t *testing.T) {
	var f streamImgFilter
	out := f.Feed(`done.<img src="/home/cliproxy/a.png"`)
	out += f.Flush()
	if out != "done." {
		t.Fatalf("unterminated tag leaked: %q", out)
	}
}

func TestBuildOpenAIChatCompletionInlinesImageInContent(t *testing.T) {
	result := &cursorlib.ChatResult{
		Text:   `here you go<img src="/home/cliproxy/x/cat.png" alt="Generated image" />`,
		Images: []cursorlib.GeneratedImage{{Base64: "QUJD", MimeType: "image/png", FilePath: "cat.png"}},
	}
	payload := buildOpenAIChatCompletion("cursor-image", result, true)
	content := gjson.GetBytes(payload, "choices.0.message.content").String()
	if strings.Contains(content, "/home/cliproxy") {
		t.Fatalf("content still points at a nonexistent path: %q", content)
	}
	if !strings.Contains(content, "data:image/png;base64,QUJD") {
		t.Fatalf("content missing inline image: %q", content)
	}
	// The images array stays populated for clients that read it instead.
	if gjson.GetBytes(payload, "choices.0.message.images.0.image_url.url").String() != "data:image/png;base64,QUJD" {
		t.Fatalf("images array lost: %s", payload)
	}
}

func TestBuildOpenAIChatCompletionIncludesImages(t *testing.T) {
	result := &cursorlib.ChatResult{
		Text: "here you go",
		Images: []cursorlib.GeneratedImage{
			{Base64: "QUJD", MimeType: "image/png"},
		},
	}
	payload := buildOpenAIChatCompletion("cursor-image", result, true)
	images := gjson.GetBytes(payload, "choices.0.message.images")
	if !images.IsArray() || len(images.Array()) != 1 {
		t.Fatalf("missing images array: %s", payload)
	}
	url := images.Array()[0].Get("image_url.url").String()
	if url != "data:image/png;base64,QUJD" {
		t.Fatalf("unexpected image url: %q", url)
	}
}
