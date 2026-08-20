package executor

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"strings"
	"testing"

	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
	"github.com/tidwall/gjson"
)

// noisyPNGBase64 returns a PNG too large to inline verbatim. The noise defeats
// PNG's compression, which is what makes the payload big enough to matter.
func noisyPNGBase64(t *testing.T, size int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	rng := rand.New(rand.NewSource(1))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(rng.Intn(256)), G: uint8(rng.Intn(256)), B: uint8(rng.Intn(256)), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestImageDeliveryModeSelection(t *testing.T) {
	cases := map[string]imageDelivery{
		"":         imageDeliveryLink,
		"link":     imageDeliveryLink,
		"nonsense": imageDeliveryLink,
		"base64":   imageDeliveryInline,
		"Inline":   imageDeliveryInline,
		" data ":   imageDeliveryInline,
		"path":     imageDeliveryPath,
		"relative": imageDeliveryPath,
		"block":    imageDeliveryBlock,
		"native":   imageDeliveryBlock,
	}
	for value, want := range cases {
		t.Setenv(imageDeliveryEnv, value)
		if got := cursorImageDelivery(); got != want {
			t.Fatalf("%q selected mode %d, want %d", value, got, want)
		}
	}
}

// Inline delivery is the mode that names no origin at all.
func TestCursorImageURLsInlineHidesOrigin(t *testing.T) {
	t.Setenv(imageDeliveryEnv, "base64")
	imgs := []cursorlib.GeneratedImage{{Base64: testPNGBase64, MimeType: "image/png"}}
	urls := cursorImageURLs("https://gw.example.com", imgs)
	if len(urls) != 1 {
		t.Fatalf("expected one url, got %#v", urls)
	}
	if !strings.HasPrefix(urls[0], "data:image/") {
		t.Fatalf("expected a data URL, got %q", urls[0])
	}
	if strings.Contains(urls[0], "gw.example.com") || strings.Contains(urls[0], "/cursor-images/") {
		t.Fatalf("inline url leaks the origin: %q", urls[0])
	}
	payload := urls[0][strings.Index(urls[0], ",")+1:]
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		t.Fatalf("inline payload is not base64: %v", err)
	}
}

// Relative delivery keeps the hosted copy but names no host.
func TestCursorImageURLsRelativeOmitsHost(t *testing.T) {
	t.Setenv(imageDeliveryEnv, "relative")
	imgs := []cursorlib.GeneratedImage{{Base64: testPNGBase64, MimeType: "image/png"}}
	urls := cursorImageURLs("https://gw.example.com", imgs)
	if len(urls) != 1 || !strings.HasPrefix(urls[0], cursorlib.PublishedImagePathPrefix) {
		t.Fatalf("expected a host-relative path, got %#v", urls)
	}
	if strings.Contains(urls[0], "gw.example.com") {
		t.Fatalf("relative url leaks the origin: %q", urls[0])
	}
	name := strings.TrimPrefix(urls[0], cursorlib.PublishedImagePathPrefix)
	if _, _, ok := cursorlib.LookupPublishedImage(name); !ok {
		t.Fatalf("relative url is not fetchable: %q", urls[0])
	}
}

// The whole reply, not just the URL, has to be free of the origin.
func TestRenderGeneratedImagesInlineLeavesNoOrigin(t *testing.T) {
	t.Setenv(imageDeliveryEnv, "base64")
	imgs := []cursorlib.GeneratedImage{{Base64: testPNGBase64, MimeType: "image/png", FilePath: "cat.png"}}
	urls := cursorImageURLs("https://15-204-94-214.sslip.io", imgs)
	text := renderGeneratedImages(`成品如下：<img src="/home/cliproxy/.cursor/projects/cliproxy-cursor-workspace/assets/cat.png" />`, imgs, urls)
	for _, leak := range []string{"sslip.io", "15.204.94.214", "/cursor-images/"} {
		if strings.Contains(text, leak) {
			t.Fatalf("reply still exposes %q: %s", leak, text)
		}
	}
	if !strings.Contains(text, "![Generated image](data:image/") {
		t.Fatalf("inline image missing from reply: %s", text)
	}
}

// An inline image is replayed with every following turn, so it has to stay
// small even when the generated PNG is not.
func TestInlineDataURLShrinksLargeImages(t *testing.T) {
	raw := noisyPNGBase64(t, 900)
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) < 512<<10 {
		t.Fatalf("test image is only %d bytes, expected an oversized one", len(decoded))
	}

	url := cursorlib.GeneratedImage{Base64: raw, MimeType: "image/png"}.InlineDataURL()
	if !strings.HasPrefix(url, "data:image/jpeg;base64,") {
		t.Fatalf("oversized image was not recompressed: %.40s", url)
	}
	payload, err := base64.StdEncoding.DecodeString(url[strings.Index(url, ",")+1:])
	if err != nil {
		t.Fatalf("recompressed payload is not base64: %v", err)
	}
	if len(payload) >= len(decoded) {
		t.Fatalf("recompressed to %d bytes, original was %d", len(payload), len(decoded))
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("recompressed payload is not an image: %v", err)
	}
	if cfg.Width > 1280 || cfg.Height > 1280 {
		t.Fatalf("recompressed image is %dx%d, expected the long side capped", cfg.Width, cfg.Height)
	}
}

// Block delivery keeps the reply text free of both the origin and the payload:
// the bytes leave through the protocol's own image channel instead.
func TestBlockDeliveryKeepsTextClean(t *testing.T) {
	t.Setenv(imageDeliveryEnv, "block")
	result := &cursorlib.ChatResult{
		Text:   `成品如下：<img src="/home/cliproxy/.cursor/projects/cliproxy-cursor-workspace/assets/cat.png" alt="Generated image" />`,
		Images: []cursorlib.GeneratedImage{{Base64: testPNGBase64, MimeType: "image/png", FilePath: "cat.png"}},
	}
	urls := cursorImageURLs("https://15-204-94-214.sslip.io", result.Images)
	payload := buildOpenAIChatCompletion("cursor-image", result, urls)

	content := gjson.GetBytes(payload, "choices.0.message.content").String()
	for _, leak := range []string{"sslip.io", "15.204.94.214", "/cursor-images/", "data:image/", "/home/cliproxy/"} {
		if strings.Contains(content, leak) {
			t.Fatalf("reply text still contains %q: %q", leak, content)
		}
	}
	if !strings.Contains(content, "成品如下：") {
		t.Fatalf("model prose was lost: %q", content)
	}

	images := gjson.GetBytes(payload, "choices.0.message.images")
	if !images.IsArray() || len(images.Array()) != 1 {
		t.Fatalf("image did not travel in the images channel: %s", payload)
	}
	if url := images.Array()[0].Get("image_url.url").String(); !strings.HasPrefix(url, "data:image/") {
		t.Fatalf("images channel carries %q, want inline bytes", url)
	}
}

func TestBlockDeliveryDropsUnreferencedImageFromText(t *testing.T) {
	t.Setenv(imageDeliveryEnv, "block")
	imgs := []cursorlib.GeneratedImage{{Base64: testPNGBase64, MimeType: "image/png", FilePath: "cat.png"}}
	urls := cursorImageURLs("https://gw.example.com", imgs)
	got := renderGeneratedImages("图片已生成。", imgs, textImageURLs(urls))
	if got != "图片已生成。" {
		t.Fatalf("text was rewritten: %q", got)
	}
}

func TestInlineDataURLKeepsSmallImagesIntact(t *testing.T) {
	url := cursorlib.GeneratedImage{Base64: testPNGBase64, MimeType: "image/png"}.InlineDataURL()
	if url != "data:image/png;base64,"+testPNGBase64 {
		t.Fatalf("small image was rewritten: %q", url)
	}
}
