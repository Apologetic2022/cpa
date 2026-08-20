package cursor

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"strings"
)

const (
	// inlineImageMaxBytes is the payload above which an inline image is
	// recompressed. An inline image lives in the assistant message itself, so
	// the client replays it with every following turn: a few megabytes of
	// base64 would be re-uploaded, re-tokenised and re-billed each time.
	inlineImageMaxBytes = 512 << 10
	// inlineImageMaxDim bounds the longest side of a recompressed image.
	inlineImageMaxDim = 1280
	// inlineImageQuality is the JPEG quality used when recompressing.
	inlineImageQuality = 85
)

// InlineDataURL renders the image as a data: URL small enough to sit in a
// conversation transcript.
func (g GeneratedImage) InlineDataURL() string {
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(g.Base64))
	if err != nil || len(data) == 0 {
		return g.DataURL()
	}
	return InlineImageDataURL(data, g.MimeType)
}

// InlineImageDataURL encodes raw image bytes as a data: URL, shrinking
// oversized payloads first. Delivering an image inline avoids publishing a
// fetchable URL, which is the point: nothing in the reply then names the host
// that generated it.
func InlineImageDataURL(data []byte, mimeType string) string {
	if len(data) == 0 {
		return ""
	}
	mime := normalizeImageMime(mimeType)
	if mime == "" {
		mime = "image/png"
	}
	if len(data) > inlineImageMaxBytes {
		if small, ok := shrinkImage(data); ok {
			data, mime = small, "image/jpeg"
		}
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// shrinkImage re-encodes an image as a bounded JPEG. It reports false when the
// payload cannot be decoded or the result would not actually be smaller.
func shrinkImage(data []byte) ([]byte, bool) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	flat := flatten(src)
	bounds := flat.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if longest := max(width, height); longest > inlineImageMaxDim {
		width = width * inlineImageMaxDim / longest
		height = height * inlineImageMaxDim / longest
		flat = boxScale(flat, max(width, 1), max(height, 1))
	}
	var buf bytes.Buffer
	if err = jpeg.Encode(&buf, flat, &jpeg.Options{Quality: inlineImageQuality}); err != nil {
		return nil, false
	}
	if buf.Len() >= len(data) {
		return nil, false
	}
	return buf.Bytes(), true
}

// flatten composites the image onto white so transparency does not turn black
// once the alpha channel is dropped by the JPEG encoder.
func flatten(src image.Image) *image.RGBA {
	bounds := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, bounds.Min, draw.Over)
	return dst
}

// boxScale downsamples by averaging each destination pixel's source area,
// which keeps generated art readable without pulling in a resampling library.
func boxScale(src *image.RGBA, width, height int) *image.RGBA {
	bounds := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		y0 := bounds.Min.Y + y*bounds.Dy()/height
		y1 := bounds.Min.Y + (y+1)*bounds.Dy()/height
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < width; x++ {
			x0 := bounds.Min.X + x*bounds.Dx()/width
			x1 := bounds.Min.X + (x+1)*bounds.Dx()/width
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, b, n uint32
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					c := src.RGBAAt(sx, sy)
					r += uint32(c.R)
					g += uint32(c.G)
					b += uint32(c.B)
					n++
				}
			}
			dst.SetRGBA(x, y, color.RGBA{R: uint8(r / n), G: uint8(g / n), B: uint8(b / n), A: 255})
		}
	}
	return dst
}
