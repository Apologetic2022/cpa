package cursor

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestRequestedLongEdgeReadsTheUsualShorthands(t *testing.T) {
	cases := map[string]int{
		"画一只猫，4K 超清":                     upscaleLongEdge4K,
		"a red fox, 8k, highly detailed": upscaleLongEdge4K,
		"portrait in 2K":                 upscaleLongEdge2K,
		"landscape 3840x2160":            upscaleLongEdge4K,
		"landscape 2560×1440":            upscaleLongEdge2K,
		"render at 2160p":                upscaleLongEdge4K,
		"1440p wallpaper":                upscaleLongEdge2K,
		"UHD city at night":              upscaleLongEdge4K,
		"a qhd desktop background":       upscaleLongEdge2K,
		// An explicit size is honoured as written, within the cap.
		"exactly 3000x2000 please": 3000,
		"absurd 12000x9000":        upscaleMaxLongEdge,
	}
	for prompt, want := range cases {
		if got := RequestedLongEdge(prompt); got != want {
			t.Errorf("RequestedLongEdge(%q) = %d, want %d", prompt, got, want)
		}
	}
}

// A prompt that never mentions size must not trigger an enlargement, and a
// number that happens to look like one must not either.
func TestRequestedLongEdgeIgnoresProseAndSmallSizes(t *testing.T) {
	for _, prompt := range []string{
		"",
		"一只橘猫坐在窗台上",
		"a crowd of 2k people",
		"logo 512x512",
		"shot on a 24mm lens",
		"make it 4kg heavier",
	} {
		if got := RequestedLongEdge(prompt); got != 0 {
			t.Errorf("RequestedLongEdge(%q) = %d, want 0", prompt, got)
		}
	}
}

// The whole point is reaching the requested size, so check the output really
// is that big and still looks like the input rather than noise.
func TestUpscaleImageBytesReachesTheRequestedEdge(t *testing.T) {
	src := gradientPNG(t, 200, 100)
	out, mime, err := UpscaleImageBytes(src, "image/png", 800)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("image was not enlarged")
	}
	if mime != "image/png" {
		t.Fatalf("mime = %q, want image/png", mime)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 800 || cfg.Height != 400 {
		t.Fatalf("scaled to %dx%d, want 800x400", cfg.Width, cfg.Height)
	}

	// The gradient runs dark to light across x; that has to survive.
	scaled, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	left := luminance(scaled.At(20, 200))
	right := luminance(scaled.At(780, 200))
	if right <= left+50 {
		t.Fatalf("gradient lost: left=%v right=%v", left, right)
	}
}

func TestUpscaleImageBytesLeavesLargeEnoughImagesAlone(t *testing.T) {
	src := gradientPNG(t, 3000, 2000)
	out, _, err := UpscaleImageBytes(src, "image/png", 2560)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Fatal("an image already past the requested edge was re-encoded")
	}
}

// Stretching a thumbnail to 4K would be a lie about detail, so the enlargement
// stops at upscaleMaxFactor.
func TestUpscaleImageBytesCapsTheStretch(t *testing.T) {
	src := gradientPNG(t, 100, 100)
	out, _, err := UpscaleImageBytes(src, "image/png", 3840)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 400 {
		t.Fatalf("width = %d, want the 4x cap at 400", cfg.Width)
	}
}

func TestUpscaleGeneratedImagesRewritesInPlace(t *testing.T) {
	images := []GeneratedImage{{
		Base64:   base64.StdEncoding.EncodeToString(gradientPNG(t, 300, 300)),
		MimeType: "image/png",
	}}
	UpscaleGeneratedImages(images, 900)

	data, err := base64.StdEncoding.DecodeString(images[0].Base64)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 900 || cfg.Height != 900 {
		t.Fatalf("image is %dx%d, want 900x900", cfg.Width, cfg.Height)
	}
	if images[0].MimeType != "image/png" {
		t.Fatalf("mime = %q", images[0].MimeType)
	}
}

func TestUpscaleGeneratedImagesIsANoopWithoutATarget(t *testing.T) {
	original := base64.StdEncoding.EncodeToString(gradientPNG(t, 64, 64))
	images := []GeneratedImage{{Base64: original, MimeType: "image/png"}}
	UpscaleGeneratedImages(images, 0)
	if images[0].Base64 != original {
		t.Fatal("image was rewritten without a requested size")
	}
}

func gradientPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			v := uint8(x * 255 / max(width-1, 1))
			img.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func luminance(c color.Color) int {
	r, g, b, _ := c.RGBA()
	return int((r + g + b) / 3 >> 8)
}
