package cursor

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Cursor's image tool takes a description and nothing else — there is no size
// argument in the protocol — and it renders around one megapixel. A caller who
// asks for 2K or 4K therefore cannot be served upstream, so the gateway
// resamples the result to the requested size itself.

const (
	// upscaleLongEdge2K and upscaleLongEdge4K are the long edges the usual
	// shorthands mean: QHD and UHD, the resolutions those names label on a
	// display. DCI's 2048/4096 are a cinema convention few callers intend.
	upscaleLongEdge2K = 2560
	upscaleLongEdge4K = 3840
	// upscaleMaxLongEdge caps an explicitly requested size. Beyond this the
	// intermediate RGBA buffers cost more memory than the detail is worth:
	// 4096x4096 is already 64 MB per copy.
	upscaleMaxLongEdge = 4096
	// upscaleMinLongEdge is the largest explicit size that is not read as a
	// request for more pixels: 1024 is what an image endpoint defaults to and
	// roughly what the generator renders, so asking for it changes nothing.
	upscaleMinLongEdge = 1024
	// upscaleMaxFactor bounds how far one image is stretched. Past this the
	// result is a soft enlargement rather than a sharper picture, and the
	// caller is better served by the honest smaller image.
	upscaleMaxFactor = 4.0
	// upscaleJPEGQuality is used when the result is re-encoded as a JPEG.
	upscaleJPEGQuality = 92
	// upscalePNGBudget is the size above which a lossless result is re-encoded
	// as a JPEG. A 4K enlargement of a photo runs past 10 MB as a PNG, which
	// the chat client has to download before it can show anything; at this
	// quality the difference is invisible and the file is a third of the size.
	upscalePNGBudget = 6 << 20
)

var (
	// resolutionShorthandPattern matches 2k / 4K / 8 k, capturing whatever word
	// follows so a quantity can be told apart from a resolution.
	resolutionShorthandPattern = regexp.MustCompile(`(?i)(^|[^a-z0-9])([2-9])\s?k\b[\s]*([a-z]+)?`)
	// resolutionScanlinePattern matches 1440p / 2160p style names.
	resolutionScanlinePattern = regexp.MustCompile(`(?i)(^|[^a-z0-9])(\d{3,4})\s?p\b`)
	// resolutionPairPattern matches an explicit 3840x2160 / 2560×1440.
	resolutionPairPattern = regexp.MustCompile(`(?i)(\d{3,5})\s*[x×*]\s*(\d{3,5})`)
	// resolutionWordPattern matches the names used without a number.
	resolutionWordPattern = regexp.MustCompile(`(?i)(^|[^a-z0-9])(uhd|ultra\s?hd|qhd|wqhd)\b`)
)

// countingNouns follow a number-with-k that counts something rather than
// naming a resolution.
var countingNouns = map[string]bool{
	"people": true, "persons": true, "users": true, "followers": true,
	"subscribers": true, "stars": true, "likes": true, "views": true,
	"comments": true, "words": true, "tokens": true, "lines": true,
	"items": true, "steps": true, "miles": true, "dollars": true,
	"euros": true, "years": true, "hours": true, "times": true,
}

// RequestedLongEdge reads the resolution a prompt asks for and returns it as a
// long edge in pixels, or 0 when the prompt says nothing about size. Only sizes
// worth resampling to are returned: anything below upscaleMinLongEdge is what
// the generator already produces.
func RequestedLongEdge(prompt string) int {
	if strings.TrimSpace(prompt) == "" {
		return 0
	}
	best := 0
	note := func(edge int) {
		if edge > best {
			best = edge
		}
	}

	if m := resolutionPairPattern.FindAllStringSubmatch(prompt, -1); m != nil {
		for _, groups := range m {
			w, _ := strconv.Atoi(groups[1])
			h, _ := strconv.Atoi(groups[2])
			note(clampLongEdge(max(w, h)))
		}
	}
	for _, groups := range resolutionShorthandPattern.FindAllStringSubmatch(prompt, -1) {
		if countingNouns[strings.ToLower(groups[3])] {
			// "2k people" is a headcount, not a resolution. In an image prompt
			// this is the only reading of "4k" that is not about size, so the
			// shorthand counts everywhere else.
			continue
		}
		switch groups[2] {
		case "2", "3":
			note(upscaleLongEdge2K)
		default:
			// 4K and anything a caller writes above it (5K, 8K) land on the
			// same ceiling: the source has no detail to justify more.
			note(upscaleLongEdge4K)
		}
	}
	for _, groups := range resolutionScanlinePattern.FindAllStringSubmatch(prompt, -1) {
		switch groups[2] {
		case "1440":
			note(upscaleLongEdge2K)
		case "2160", "4320":
			note(upscaleLongEdge4K)
		}
	}
	for _, groups := range resolutionWordPattern.FindAllStringSubmatch(prompt, -1) {
		if strings.EqualFold(groups[2], "qhd") || strings.EqualFold(groups[2], "wqhd") {
			note(upscaleLongEdge2K)
			continue
		}
		note(upscaleLongEdge4K)
	}
	return best
}

// clampLongEdge keeps an explicitly requested size inside what is worth
// rendering, and drops sizes small enough that the generator already meets them.
func clampLongEdge(edge int) int {
	if edge <= upscaleMinLongEdge {
		return 0
	}
	if edge > upscaleMaxLongEdge {
		return upscaleMaxLongEdge
	}
	return edge
}

// UpscaleGeneratedImages resamples every image to the requested long edge,
// in place. Images already at or above it, and images that would have to be
// stretched further than upscaleMaxFactor, are left as they are.
func UpscaleGeneratedImages(images []GeneratedImage, longEdge int) {
	for i := range images {
		images[i] = UpscaleGeneratedImage(images[i], longEdge)
	}
}

// UpscaleGeneratedImage returns the image resampled to the requested long
// edge, or unchanged when there is nothing to do.
func UpscaleGeneratedImage(img GeneratedImage, longEdge int) GeneratedImage {
	if longEdge <= 0 {
		return img
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(img.Base64))
	if err != nil || len(data) == 0 {
		return img
	}
	started := time.Now()
	out, mime, err := UpscaleImageBytes(data, img.MimeType, longEdge)
	if err != nil {
		log.Warnf("cursor upscale to %dpx failed, serving the original: %v", longEdge, err)
		return img
	}
	if out == nil {
		return img
	}
	log.Infof("cursor upscale: %d -> %d bytes for a %dpx long edge in %s", len(data), len(out), longEdge, time.Since(started).Round(time.Millisecond))
	img.Base64 = base64.StdEncoding.EncodeToString(out)
	img.MimeType = mime
	return img
}

// UpscaleImageBytes resamples one image so its longest side reaches longEdge.
// It returns nil bytes when the image is already large enough or is too small
// to enlarge honestly, which callers treat as "keep the original".
func UpscaleImageBytes(data []byte, mimeType string, longEdge int) ([]byte, string, error) {
	if longEdge <= 0 {
		return nil, "", nil
	}
	src, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode: %w", err)
	}
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	longest := max(width, height)
	if longest <= 0 || longest >= longEdge {
		return nil, "", nil
	}
	factor := float64(longEdge) / float64(longest)
	if factor > upscaleMaxFactor {
		factor = upscaleMaxFactor
	}
	dstW := int(math.Round(float64(width) * factor))
	dstH := int(math.Round(float64(height) * factor))
	if dstW <= width && dstH <= height {
		return nil, "", nil
	}

	scaled := catmullRomScale(toRGBA(src), max(dstW, 1), max(dstH, 1))
	// Resampling cannot invent detail, and every interpolation kernel softens
	// edges on the way up. A light unsharp pass puts back the local contrast
	// the enlargement cost, which is what makes the result read as sharp.
	unsharpMask(scaled, 0.55)

	mime := normalizeImageMime(mimeType)
	if mime == "" {
		mime = "image/" + format
	}
	if mime == "image/jpeg" || mime == "image/jpg" {
		return encodeJPEG(scaled)
	}
	var buf bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err = encoder.Encode(&buf, scaled); err != nil {
		return nil, "", fmt.Errorf("encode png: %w", err)
	}
	if buf.Len() <= upscalePNGBudget {
		return buf.Bytes(), "image/png", nil
	}
	compact, compactMime, err := encodeJPEG(scaled)
	if err != nil || len(compact) >= buf.Len() {
		return buf.Bytes(), "image/png", nil
	}
	return compact, compactMime, nil
}

func encodeJPEG(img image.Image) ([]byte, string, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: upscaleJPEGQuality}); err != nil {
		return nil, "", fmt.Errorf("encode jpeg: %w", err)
	}
	return buf.Bytes(), "image/jpeg", nil
}

// toRGBA returns src as an *image.RGBA, reusing it when it already is one.
func toRGBA(src image.Image) *image.RGBA {
	if rgba, ok := src.(*image.RGBA); ok {
		return rgba
	}
	bounds := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			dst.Set(x, y, src.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return dst
}

// catmullRomScale resamples with a Catmull-Rom cubic kernel, separably: rows
// first, then columns. The kernel interpolates its samples and has mildly
// negative lobes, so enlargements keep edges crisp instead of turning into the
// blurred porridge bilinear gives.
func catmullRomScale(src *image.RGBA, width, height int) *image.RGBA {
	srcW, srcH := src.Bounds().Dx(), src.Bounds().Dy()
	if srcW == width && srcH == height {
		return src
	}
	horizontal := image.NewRGBA(image.Rect(0, 0, width, srcH))
	resampleAxis(src, horizontal, width, srcH, srcW, true)
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	resampleAxis(horizontal, dst, width, height, srcH, false)
	return dst
}

// resampleAxis rescales one axis. When horizontal is true it maps srcLen source
// columns onto dst's width; otherwise srcLen source rows onto dst's height.
func resampleAxis(src, dst *image.RGBA, width, height, srcLen int, horizontal bool) {
	dstLen := height
	if horizontal {
		dstLen = width
	}
	scale := float64(srcLen) / float64(dstLen)
	for d := 0; d < dstLen; d++ {
		// Sample at the centre of the destination pixel's source footprint.
		center := (float64(d)+0.5)*scale - 0.5
		base := int(math.Floor(center))
		var weights [4]float64
		var total float64
		for k := 0; k < 4; k++ {
			w := catmullRom(center - float64(base-1+k))
			weights[k] = w
			total += w
		}
		if total == 0 {
			total = 1
		}
		for k := range weights {
			weights[k] /= total
		}
		for other := 0; other < otherAxisLen(width, height, horizontal); other++ {
			var r, g, b, a float64
			for k := 0; k < 4; k++ {
				idx := clampIndex(base-1+k, srcLen)
				var c [4]uint8
				if horizontal {
					c = pixelAt(src, idx, other)
				} else {
					c = pixelAt(src, other, idx)
				}
				w := weights[k]
				r += w * float64(c[0])
				g += w * float64(c[1])
				b += w * float64(c[2])
				a += w * float64(c[3])
			}
			var x, y int
			if horizontal {
				x, y = d, other
			} else {
				x, y = other, d
			}
			off := dst.PixOffset(x, y)
			dst.Pix[off+0] = clamp8(r)
			dst.Pix[off+1] = clamp8(g)
			dst.Pix[off+2] = clamp8(b)
			dst.Pix[off+3] = clamp8(a)
		}
	}
}

func otherAxisLen(width, height int, horizontal bool) int {
	if horizontal {
		return height
	}
	return width
}

func pixelAt(img *image.RGBA, x, y int) [4]uint8 {
	off := img.PixOffset(x, y)
	return [4]uint8{img.Pix[off], img.Pix[off+1], img.Pix[off+2], img.Pix[off+3]}
}

// catmullRom is the cubic kernel with B=0, C=0.5.
func catmullRom(x float64) float64 {
	x = math.Abs(x)
	switch {
	case x < 1:
		return 1.5*x*x*x - 2.5*x*x + 1
	case x < 2:
		return -0.5*x*x*x + 2.5*x*x - 4*x + 2
	default:
		return 0
	}
}

func clampIndex(i, n int) int {
	if i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
}

func clamp8(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}

// unsharpMask adds back amount times the difference between the image and a
// blurred copy of itself, in place. Alpha is left alone: sharpening it would
// fringe the edges of a cut-out.
func unsharpMask(img *image.RGBA, amount float64) {
	if amount <= 0 {
		return
	}
	width, height := img.Bounds().Dx(), img.Bounds().Dy()
	if width < 3 || height < 3 {
		return
	}
	blurred := boxBlur3(img)
	for i := 0; i+3 < len(img.Pix); i += 4 {
		for c := 0; c < 3; c++ {
			orig := float64(img.Pix[i+c])
			img.Pix[i+c] = clamp8(orig + amount*(orig-float64(blurred[i+c])))
		}
	}
}

// boxBlur3 returns a 3x3 box-blurred copy of the pixel buffer. A box blur is a
// coarse Gaussian, which is all an unsharp mask at this radius needs.
func boxBlur3(img *image.RGBA) []uint8 {
	width, height := img.Bounds().Dx(), img.Bounds().Dy()
	out := make([]uint8, len(img.Pix))
	copy(out, img.Pix)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			var sum [3]int
			var n int
			for dy := -1; dy <= 1; dy++ {
				sy := clampIndex(y+dy, height)
				for dx := -1; dx <= 1; dx++ {
					sx := clampIndex(x+dx, width)
					off := img.PixOffset(sx, sy)
					sum[0] += int(img.Pix[off])
					sum[1] += int(img.Pix[off+1])
					sum[2] += int(img.Pix[off+2])
					n++
				}
			}
			off := img.PixOffset(x, y)
			out[off] = uint8(sum[0] / n)
			out[off+1] = uint8(sum[1] / n)
			out[off+2] = uint8(sum[2] / n)
		}
	}
	return out
}
