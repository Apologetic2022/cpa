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
		// An image content block aborts the turn in clients that validate the
		// Anthropic union, so "block" is no longer a mode and falls back to a
		// link rather than silently dropping the image.
		"block": imageDeliveryLink,
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
	if strings.Contains(urls[0], "gw.example.com") || strings.Contains(urls[0], cursorlib.PublishedImagePathPrefix) {
		t.Fatalf("inline url leaks the origin: %q", urls[0])
	}
	payload := urls[0][strings.Index(urls[0], ",")+1:]
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		t.Fatalf("inline payload is not base64: %v", err)
	}
}

// Relative delivery keeps the hosted copy but names no host, and points at the
// API namespace so the client resolves it against the base URL it already has.
func TestCursorImageURLsRelativeOmitsHost(t *testing.T) {
	t.Setenv(imageDeliveryEnv, "relative")
	imgs := []cursorlib.GeneratedImage{{Base64: testPNGBase64, MimeType: "image/png"}}
	urls := cursorImageURLs("https://gw.example.com", imgs)
	if len(urls) != 1 || !strings.HasPrefix(urls[0], cursorlib.PublishedImageAPIPathPrefix) {
		t.Fatalf("expected a host-relative API path, got %#v", urls)
	}
	if strings.Contains(urls[0], "gw.example.com") {
		t.Fatalf("relative url leaks the origin: %q", urls[0])
	}
	name := strings.TrimPrefix(urls[0], cursorlib.PublishedImageAPIPathPrefix)
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
	for _, leak := range []string{"sslip.io", "15.204.94.214", cursorlib.PublishedImagePathPrefix} {
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

// A link is the only delivery a hardened markdown renderer will display, but
// the address must stay inside the image markup: what the reader sees is the
// picture, not the host it came from.
func TestLinkDeliveryHidesAddressBehindTheImage(t *testing.T) {
	t.Setenv(imageDeliveryEnv, "link")
	result := &cursorlib.ChatResult{
		Text:   `成品如下：<img src="/home/cliproxy/.cursor/projects/cliproxy-cursor-workspace/assets/cat.png" alt="Generated image" />`,
		Images: []cursorlib.GeneratedImage{{Base64: testPNGBase64, MimeType: "image/png", FilePath: "cat.png"}},
	}
	urls := cursorImageURLs("https://gw.example.com", result.Images)
	content := gjson.GetBytes(buildOpenAIChatCompletion("image", result, urls), "choices.0.message.content").String()

	if !strings.Contains(content, "![Generated image](https://gw.example.com"+cursorlib.PublishedImagePathPrefix) {
		t.Fatalf("image is not rendered as a link: %q", content)
	}
	if strings.Contains(content, "/home/cliproxy/") {
		t.Fatalf("unrenderable local path survived: %q", content)
	}
	prose := markdownImagePattern.ReplaceAllString(content, "")
	if strings.Contains(prose, "gw.example.com") || strings.Contains(prose, cursorlib.PublishedImagePathPrefix) {
		t.Fatalf("address shows up as visible text: %q", prose)
	}
}

// Image generation is a property of the model, not of the prompt: a chat on
// any other model must not reach the image tool.
func TestOnlyTheImageModelMayGenerate(t *testing.T) {
	if !isImageModel("image") || !isImageModel("Image") {
		t.Fatal("the image model cannot generate images")
	}
	for _, model := range []string{"grok-4.6", "claude-4.6-sonnet", "default", "gpt-5.4", "cursor-image", "image-2"} {
		if isImageModel(model) {
			t.Fatalf("%q is allowed to generate images", model)
		}
	}
}

// The link is visible in every reply that carries an image, so no served
// prefix may name the provider sitting behind this gateway.
func TestPublishedImagePathNamesNoProvider(t *testing.T) {
	t.Setenv(imageDeliveryEnv, "link")
	imgs := []cursorlib.GeneratedImage{{Base64: testPNGBase64, MimeType: "image/png"}}
	urls := cursorImageURLs("https://gw.example.com", imgs)
	if len(urls) != 1 {
		t.Fatalf("expected one url, got %#v", urls)
	}
	if strings.Contains(strings.ToLower(urls[0]), "cursor") {
		t.Fatalf("published url names the provider: %q", urls[0])
	}
	for _, prefix := range []string{cursorlib.PublishedImagePathPrefix, cursorlib.PublishedImageAPIPathPrefix} {
		if strings.Contains(strings.ToLower(prefix), "cursor") {
			t.Fatalf("served prefix names the provider: %q", prefix)
		}
	}
}

func TestInlineDataURLKeepsSmallImagesIntact(t *testing.T) {
	url := cursorlib.GeneratedImage{Base64: testPNGBase64, MimeType: "image/png"}.InlineDataURL()
	if url != "data:image/png;base64,"+testPNGBase64 {
		t.Fatalf("small image was rewritten: %q", url)
	}
}
