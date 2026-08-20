package cursor

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// onePixelPNG is the smallest payload http.DetectContentType still recognises.
var onePixelPNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E,
	0x44, 0xAE, 0x42, 0x60, 0x82,
}

func TestPublishGeneratedImageRoundTrip(t *testing.T) {
	img := GeneratedImage{Base64: base64.StdEncoding.EncodeToString(onePixelPNG), MimeType: "image/png"}
	path := PublishGeneratedImage(img)
	if !strings.HasPrefix(path, PublishedImagePathPrefix) || !strings.HasSuffix(path, ".png") {
		t.Fatalf("unexpected path %q", path)
	}
	name := strings.TrimPrefix(path, PublishedImagePathPrefix)
	data, mime, ok := LookupPublishedImage(name)
	if !ok {
		t.Fatalf("published image %q not found", name)
	}
	if mime != "image/png" || !bytes.Equal(data, onePixelPNG) {
		t.Fatalf("stored image differs: mime=%q len=%d", mime, len(data))
	}
	if _, _, ok = LookupPublishedImage("does-not-exist.png"); ok {
		t.Fatal("unknown name resolved")
	}
}

func TestPublishImageBytesSniffsAndRejects(t *testing.T) {
	// A missing or bogus MIME type is recovered from the bytes themselves.
	path := PublishImageBytes(onePixelPNG, "application/octet-stream")
	if !strings.HasSuffix(path, ".png") {
		t.Fatalf("expected sniffed png, got %q", path)
	}
	if got := PublishImageBytes([]byte("not an image at all"), ""); got != "" {
		t.Fatalf("non-image accepted: %q", got)
	}
	if got := PublishImageBytes(nil, "image/png"); got != "" {
		t.Fatalf("empty payload accepted: %q", got)
	}
	if got := PublishGeneratedImage(GeneratedImage{Base64: "!!!not base64!!!"}); got != "" {
		t.Fatalf("undecodable payload accepted: %q", got)
	}
}

func TestPublishedImageStoreEvictsAndExpires(t *testing.T) {
	store := &publishedImageStore{items: make(map[string]publishedImage)}
	future := time.Now().Add(time.Hour)
	store.put("expired.png", publishedImage{data: []byte("a"), mime: "image/png", expires: time.Now().Add(-time.Minute)})
	store.put("kept.png", publishedImage{data: []byte("b"), mime: "image/png", expires: future})
	if _, _, ok := store.get("expired.png"); ok {
		t.Fatal("expired image still served")
	}
	if _, _, ok := store.get("kept.png"); !ok {
		t.Fatal("live image dropped")
	}

	// The oldest entries go first once the store is over its limits.
	for i := 0; i < publishedImageMaxCount+5; i++ {
		store.put(string(rune('a'+i%26))+time.Now().Format("150405.000000000"), publishedImage{data: []byte("x"), mime: "image/png", expires: future})
	}
	if len(store.items) > publishedImageMaxCount {
		t.Fatalf("store grew past its cap: %d entries", len(store.items))
	}
	if _, _, ok := store.get("kept.png"); ok {
		t.Fatal("oldest entry should have been evicted first")
	}
}
