package cursor

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
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

// newTestStore returns a store writing to a scratch directory.
func newTestStore(t *testing.T) *publishedImageStore {
	t.Helper()
	return &publishedImageStore{
		items:    make(map[string]publishedImage),
		dir:      t.TempDir(),
		dirReady: true,
	}
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
	if _, _, ok = LookupPublishedImage("doesnotexist.png"); ok {
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

func TestPublishedImageSurvivesMemoryEviction(t *testing.T) {
	// A link sits in the client's transcript long after the bytes leave the
	// heap, so the file has to answer on its own.
	store := newTestStore(t)
	store.put("kept.png", publishedImage{data: onePixelPNG, mime: "image/png", expires: time.Now().Add(time.Hour)})
	store.forgetLocked("kept.png")
	if _, ok := store.items["kept.png"]; ok {
		t.Fatal("entry still in memory")
	}
	data, mime, ok := store.get("kept.png")
	if !ok {
		t.Fatal("image not served from disk after eviction")
	}
	if mime != "image/png" || !bytes.Equal(data, onePixelPNG) {
		t.Fatalf("disk copy differs: mime=%q len=%d", mime, len(data))
	}
}

func TestPublishedImageExpiresOnDisk(t *testing.T) {
	store := newTestStore(t)
	store.put("stale.png", publishedImage{data: onePixelPNG, mime: "image/png", expires: time.Now().Add(-time.Minute)})
	old := time.Now().Add(-publishedImageTTL - time.Hour)
	if err := os.Chtimes(filepath.Join(store.dir, "stale.png"), old, old); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := store.get("stale.png"); ok {
		t.Fatal("expired image still served")
	}
	if _, err := os.Stat(filepath.Join(store.dir, "stale.png")); !os.IsNotExist(err) {
		t.Fatalf("expired file not removed: %v", err)
	}
}

func TestPublishedImagePruneDropsExpiredFiles(t *testing.T) {
	store := newTestStore(t)
	stale := filepath.Join(store.dir, "old.png")
	if err := os.WriteFile(stale, onePixelPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-publishedImageTTL - time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	store.put("fresh.png", publishedImage{data: onePixelPNG, mime: "image/png", expires: time.Now().Add(time.Hour)})
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("prune left an expired file: %v", err)
	}
	if _, _, ok := store.get("fresh.png"); !ok {
		t.Fatal("prune removed the live file")
	}
}

func TestPublishedImageNameValidation(t *testing.T) {
	store := newTestStore(t)
	secret := filepath.Join(filepath.Dir(store.dir), "secret.png")
	if err := os.WriteFile(secret, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../secret.png", "..%2Fsecret.png", "/etc/passwd", "", "a/b.png"} {
		if _, _, ok := store.get(name); ok {
			t.Fatalf("traversal name %q resolved", name)
		}
	}
}
