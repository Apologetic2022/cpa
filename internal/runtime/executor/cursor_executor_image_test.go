package executor

import (
	"bytes"
	"encoding/base64"
	"mime/multipart"
	"strings"
	"testing"

	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
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
	body := []byte(`{"model":"nano-banana-pro","messages":[
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

func TestAttachChatImageNoteKeepsConversation(t *testing.T) {
	messages := []cursorlib.ChatMessage{
		{Role: "system", Content: "be helpful"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
		{Role: "user", Content: "把红色圆形改成蓝色\n"},
	}
	refs := []cursorlib.ReferenceImage{{Path: "/ws/assets/references/reference-1.png"}}

	got := attachChatImageNote(messages, refs)
	if len(got) != 4 {
		t.Fatalf("conversation was reshaped: %#v", got)
	}
	last := got[3].Content
	if !strings.HasPrefix(last, "把红色圆形改成蓝色") {
		t.Fatalf("prompt lost: %q", last)
	}
	if !strings.Contains(last, refs[0].Path) {
		t.Fatalf("note missing reference path: %q", last)
	}
	// Earlier turns must be untouched so history stays faithful.
	if got[1].Content != "hello" || got[0].Content != "be helpful" {
		t.Fatalf("earlier turns were rewritten: %#v", got[:2])
	}
}

func TestAttachChatImageNoteIsNoopWithoutRefs(t *testing.T) {
	messages := []cursorlib.ChatMessage{{Role: "user", Content: "just text"}}
	if got := attachChatImageNote(messages, nil); got[0].Content != "just text" {
		t.Fatalf("note added without references: %q", got[0].Content)
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
	prompt, refs, err := cursorImagesRequest([]byte(`{"model":"nano-banana-pro","prompt":"a red fox"}`))
	if err != nil || prompt != "a red fox" {
		t.Fatalf("prompt = %q err = %v", prompt, err)
	}
	if len(refs) != 0 {
		t.Fatalf("expected no reference images, got %d", len(refs))
	}
	if _, _, err = cursorImagesRequest([]byte(`{"model":"nano-banana-pro"}`)); err == nil {
		t.Fatal("expected error for missing prompt")
	}
	if _, _, err = cursorImagesRequest([]byte(`not json`)); err == nil {
		t.Fatal("expected error for non-JSON body")
	}
}

// A caller can ask for the resolution either the OpenAI way or in words, and
// the images endpoints have to honour both.
func TestCursorImagesInputReadsTheRequestedResolution(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`{"prompt":"a red fox"}`, 0},
		{`{"prompt":"a red fox in 4K"}`, 3840},
		{`{"prompt":"a red fox","size":"2560x1440"}`, 2560},
		// The larger of the two wins: both say what the caller wants, and the
		// bigger one is the one they would notice missing.
		{`{"prompt":"a red fox in 4K","size":"2048x2048"}`, 3840},
		{`{"prompt":"a red fox","size":"1024x1024"}`, 0},
	}
	for _, tc := range cases {
		input, err := cursorImagesInput([]byte(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		if input.longEdge != tc.want {
			t.Errorf("%s: longEdge = %d, want %d", tc.body, input.longEdge, tc.want)
		}
	}
}

func TestCursorImagesRequestJSONEdit(t *testing.T) {
	body := `{"model":"nano-banana-pro","prompt":"make it green","images":[{"image_url":"data:image/png;base64,` + testPNGBase64 + `"}]}`
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

// hostedURLs is the URL list the executor would build for a run's images.
func hostedURLs(t *testing.T, base string, imgs []cursorlib.GeneratedImage) []string {
	t.Helper()
	return cursorImageURLs(base, imgs)
}

func TestRenderGeneratedImagesRewritesLocalPathToHostedURL(t *testing.T) {
	imgs := []cursorlib.GeneratedImage{{Base64: testPNGBase64, MimeType: "image/png", FilePath: "/home/cliproxy/.cursor/projects/cliproxy-cursor-workspace/assets/assets/portrait-woman.png"}}
	urls := hostedURLs(t, "https://gw.example.com", imgs)
	text := `已经生成一张写实肖像。<img src="/home/cliproxy/.cursor/projects/cliproxy-cursor-workspace/assets/assets/portrait-woman.png" alt="Generated image" />后续可继续调整。`

	got := renderGeneratedImages(text, imgs, urls)
	if strings.Contains(got, "/home/cliproxy") {
		t.Fatalf("local path survived rewrite: %q", got)
	}
	// A data URL is what the client's markdown sanitizer blocks, so the
	// rewritten reference must be a fetchable link instead.
	if strings.Contains(got, "data:image/png") {
		t.Fatalf("data URL leaked into content: %q", got)
	}
	if !strings.Contains(got, "!["+"Generated image"+"](https://gw.example.com/media/") {
		t.Fatalf("hosted markdown image missing: %q", got)
	}
	if !strings.Contains(got, "已经生成一张写实肖像。") || !strings.Contains(got, "后续可继续调整。") {
		t.Fatalf("surrounding text damaged: %q", got)
	}
}

func TestRenderGeneratedImagesRewritesMarkdownLocalPath(t *testing.T) {
	imgs := []cursorlib.GeneratedImage{{Base64: testPNGBase64, MimeType: "image/png", FilePath: "portrait.png"}}
	urls := hostedURLs(t, "https://gw.example.com", imgs)
	text := "成品如下：\n\n![美女肖像](/home/cliproxy/.cursor/projects/cliproxy-cursor-workspace/assets/portrait.png)\n"

	got := renderGeneratedImages(text, imgs, urls)
	if strings.Contains(got, "/home/cliproxy") {
		t.Fatalf("local path survived rewrite: %q", got)
	}
	if !strings.Contains(got, "![美女肖像](https://gw.example.com/media/") {
		t.Fatalf("alt text or hosted URL lost: %q", got)
	}
}

func TestRenderGeneratedImagesAppendsUnreferencedImage(t *testing.T) {
	imgs := []cursorlib.GeneratedImage{{Base64: testPNGBase64, MimeType: "image/png", FilePath: "cat.png"}}
	urls := hostedURLs(t, "https://gw.example.com", imgs)

	got := renderGeneratedImages("图片已成功生成。", imgs, urls)
	if !strings.HasPrefix(got, "图片已成功生成。") {
		t.Fatalf("prose damaged: %q", got)
	}
	if !strings.Contains(got, "![Generated image](https://gw.example.com/media/") {
		t.Fatalf("image not appended for a reply that never referenced it: %q", got)
	}
}

func TestRenderGeneratedImagesFallsBackToOrder(t *testing.T) {
	imgs := []cursorlib.GeneratedImage{{Base64: testPNGBase64, MimeType: "image/png", FilePath: "somewhere-else.png"}}
	urls := hostedURLs(t, "https://gw.example.com", imgs)
	got := renderGeneratedImages(`<img src="/home/cliproxy/.cursor/projects/cliproxy-cursor-workspace/assets/assets/unknown-name.png" />`, imgs, urls)
	if got != "!["+"Generated image"+"]("+urls[0]+")" {
		t.Fatalf("expected order fallback, got %q", got)
	}
}

func TestRenderGeneratedImagesLeavesAddressableSources(t *testing.T) {
	imgs := []cursorlib.GeneratedImage{{Base64: testPNGBase64, MimeType: "image/png", FilePath: "a.png"}}
	urls := hostedURLs(t, "https://gw.example.com", imgs)
	for _, src := range []string{"data:image/png;base64,ZZZ", "https://example.com/a.png"} {
		text := `<img src="` + src + `" />`
		got := renderGeneratedImages(text, imgs, urls)
		if !strings.HasPrefix(got, text) {
			t.Fatalf("rewrote an already-addressable src %q -> %q", src, got)
		}
	}
	// No images at all must be a no-op rather than a panic.
	if got := renderGeneratedImages(`<img src="/x.png" />`, nil, nil); got != `<img src="/x.png" />` {
		t.Fatalf("unexpected rewrite without images: %q", got)
	}
}

func TestCursorPublicBaseURLPrefersOperatorOverride(t *testing.T) {
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.RequestBaseURLMetadataKey: "https://15.204.94.214",
	}}
	if got := cursorPublicBaseURL(opts); got != "https://15.204.94.214" {
		t.Fatalf("request origin ignored: %q", got)
	}
	// A gateway whose own certificate the client will not trust has to be able
	// to hand out a different origin for the images it hosts.
	t.Setenv("CPA_PUBLIC_BASE_URL", "https://gw.example.com/")
	if got := cursorPublicBaseURL(opts); got != "https://gw.example.com" {
		t.Fatalf("override ignored: %q", got)
	}
	if got := cursorPublicBaseURL(cliproxyexecutor.Options{}); got != "https://gw.example.com" {
		t.Fatalf("override lost without metadata: %q", got)
	}
}

func TestCursorImageURLsFallsBackToDataURLWithoutOrigin(t *testing.T) {
	imgs := []cursorlib.GeneratedImage{{Base64: "QUJD", MimeType: "image/png"}}
	if got := cursorImageURLs("", imgs); len(got) != 1 || got[0] != "data:image/png;base64,QUJD" {
		t.Fatalf("expected data URL fallback, got %#v", got)
	}
}

func TestCursorImageURLsServesHostedBytes(t *testing.T) {
	imgs := []cursorlib.GeneratedImage{{Base64: testPNGBase64, MimeType: "image/png"}}
	urls := cursorImageURLs("https://gw.example.com", imgs)
	if len(urls) != 1 || !strings.HasPrefix(urls[0], "https://gw.example.com/media/") {
		t.Fatalf("unexpected hosted url: %#v", urls)
	}
	name := strings.TrimPrefix(urls[0], "https://gw.example.com/media/")
	data, mime, ok := cursorlib.LookupPublishedImage(name)
	if !ok {
		t.Fatalf("hosted image %q is not served back", name)
	}
	if mime != "image/png" {
		t.Fatalf("unexpected mime %q", mime)
	}
	want, _ := base64.StdEncoding.DecodeString(testPNGBase64)
	if !bytes.Equal(data, want) {
		t.Fatalf("hosted bytes differ from the generated image")
	}
	if !strings.HasSuffix(name, ".png") {
		t.Fatalf("hosted name should carry the image extension: %q", name)
	}
}

func TestStreamImgFilterDropsTagAcrossChunks(t *testing.T) {
	var f streamImgFilter
	// The tag is split across five deltas, mid-attribute.
	chunks := []string{
		"生成完毕。",
		`<im`,
		`g src="/home/cliproxy/.cursor/pro`,
		`jects/cliproxy-cursor-workspace/assets/a.png" alt="x"`,
		` />剩余文本`,
	}
	var got strings.Builder
	for _, c := range chunks {
		got.WriteString(f.Feed(c))
	}
	got.WriteString(f.Flush())

	if want := "生成完毕。剩余文本"; got.String() != want {
		t.Fatalf("filtered text = %q, want %q", got.String(), want)
	}
}

func TestRenderGeneratedImagesLeavesUnrelatedLocalPaths(t *testing.T) {
	// A model writing HTML must keep its own relative/absolute paths, even in a
	// turn that also produced an image.
	imgs := []cursorlib.GeneratedImage{{Base64: testPNGBase64, MimeType: "image/png", FilePath: "cat.png"}}
	urls := hostedURLs(t, "https://gw.example.com", imgs)
	for _, src := range []string{"./logo.png", "/var/www/assets/hero.jpg", "images/icon.svg"} {
		text := `<img src="` + src + `" />`
		if got := renderGeneratedImages(text, imgs, urls); !strings.HasPrefix(got, text) {
			t.Fatalf("rewrote an unrelated path %q -> %q", src, got)
		}
	}
}

func TestStreamImgFilterKeepsUnrelatedLocalPaths(t *testing.T) {
	for _, src := range []string{"./logo.png", "/var/www/hero.jpg", "images/icon.svg"} {
		var f streamImgFilter
		text := `<img src="` + src + `" alt="x">`
		if got := f.Feed(text) + f.Flush(); got != text {
			t.Fatalf("filter dropped an unrelated path %q -> %q", src, got)
		}
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

func TestStreamImgFilterDropsMarkdownLocalPathAcrossChunks(t *testing.T) {
	var f streamImgFilter
	chunks := []string{
		"图片如下：",
		"![美女肖",
		"像](/home/cliproxy/.cursor/projects/cliproxy-cursor",
		"-workspace/assets/portrait.png)",
		"需要修改直接说。",
	}
	var got strings.Builder
	for _, c := range chunks {
		got.WriteString(f.Feed(c))
	}
	got.WriteString(f.Flush())
	if want := "图片如下：需要修改直接说。"; got.String() != want {
		t.Fatalf("filtered text = %q, want %q", got.String(), want)
	}
}

func TestStreamImgFilterKeepsUnrelatedMarkdown(t *testing.T) {
	for _, text := range []string{
		"![cat](https://example.com/cat.png)",
		"![local](./cat.png)",
		"see ![](/var/www/a.png) here",
		"警告! [链接](https://example.com) 结束",
		"纯文本 ! 感叹号",
	} {
		var f streamImgFilter
		if got := f.Feed(text) + f.Flush(); got != text {
			t.Fatalf("filter altered %q -> %q", text, got)
		}
	}
}

func TestStreamImgFilterDropsUnterminatedTag(t *testing.T) {
	var f streamImgFilter
	out := f.Feed(`done.<img src="/home/cliproxy/.cursor/projects/cliproxy-cursor-workspace/assets/assets/a.png"`)
	out += f.Flush()
	if out != "done." {
		t.Fatalf("unterminated tag leaked: %q", out)
	}
}

func TestBuildOpenAIChatCompletionLinksImageInContent(t *testing.T) {
	result := &cursorlib.ChatResult{
		Text:   `here you go<img src="/home/cliproxy/.cursor/projects/cliproxy-cursor-workspace/assets/assets/cat.png" alt="Generated image" />`,
		Images: []cursorlib.GeneratedImage{{Base64: testPNGBase64, MimeType: "image/png", FilePath: "cat.png"}},
	}
	urls := hostedURLs(t, "https://gw.example.com", result.Images)
	payload := buildOpenAIChatCompletion("nano-banana-pro", result, urls)
	content := gjson.GetBytes(payload, "choices.0.message.content").String()
	if strings.Contains(content, "/home/cliproxy") {
		t.Fatalf("content still points at a nonexistent path: %q", content)
	}
	if !strings.Contains(content, "![Generated image]("+urls[0]+")") {
		t.Fatalf("content missing hosted image: %q", content)
	}
	// The images array stays populated for clients that read it instead.
	if gjson.GetBytes(payload, "choices.0.message.images.0.image_url.url").String() != urls[0] {
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
	payload := buildOpenAIChatCompletion("nano-banana-pro", result, cursorImageURLs("", result.Images))
	images := gjson.GetBytes(payload, "choices.0.message.images")
	if !images.IsArray() || len(images.Array()) != 1 {
		t.Fatalf("missing images array: %s", payload)
	}
	url := images.Array()[0].Get("image_url.url").String()
	if url != "data:image/png;base64,QUJD" {
		t.Fatalf("unexpected image url: %q", url)
	}
}
