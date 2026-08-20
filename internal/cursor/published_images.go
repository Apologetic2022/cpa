package cursor

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

// PublishedImagePathPrefix is the URL prefix the gateway serves generated
// images under. Chat clients sanitise assistant markdown before rendering it
// and the common sanitisers (harden-react-markdown / streamdown) always reject
// data: URLs, so an image is only visible when it arrives as an ordinary
// http(s) link the client can fetch back from this proxy.
const PublishedImagePathPrefix = "/cursor-images/"

// PublishedImageRoute is the router pattern matching PublishedImagePathPrefix.
const PublishedImageRoute = PublishedImagePathPrefix + ":name"

const (
	// publishedImageTTL bounds how long a hosted image stays fetchable. It has
	// to outlive the turn that produced it, because a client re-renders the
	// whole transcript whenever the conversation is reopened.
	publishedImageTTL = 12 * time.Hour
	// publishedImageMaxBytes and publishedImageMaxCount bound the store: it
	// lives in the gateway's heap, so a long-running relay must not accumulate
	// every image it has ever produced.
	publishedImageMaxBytes = 192 << 20
	publishedImageMaxCount = 256
)

type publishedImage struct {
	data    []byte
	mime    string
	expires time.Time
}

type publishedImageStore struct {
	mu    sync.Mutex
	items map[string]publishedImage
	order []string
	bytes int
}

var publishedImages = &publishedImageStore{items: make(map[string]publishedImage)}

// PublishGeneratedImage hosts one image produced by the GenerateImage tool and
// returns the absolute path it is served under, or an empty string when the
// payload cannot be decoded.
func PublishGeneratedImage(img GeneratedImage) string {
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(img.Base64))
	if err != nil {
		return ""
	}
	return PublishImageBytes(data, img.MimeType)
}

// PublishImageBytes hosts raw image bytes and returns the path they are served
// under. The name carries 128 bits of entropy because the route is
// unauthenticated: the URL is the capability.
func PublishImageBytes(data []byte, mimeType string) string {
	if len(data) == 0 {
		return ""
	}
	mime := normalizeImageMime(mimeType)
	if mime == "" {
		mime = normalizeImageMime(http.DetectContentType(data))
	}
	if mime == "" {
		return ""
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ""
	}
	name := hex.EncodeToString(raw[:]) + imageExtension(mime)
	publishedImages.put(name, publishedImage{
		data:    data,
		mime:    mime,
		expires: time.Now().Add(publishedImageTTL),
	})
	return PublishedImagePathPrefix + name
}

// LookupPublishedImage returns the bytes and MIME type hosted under name.
func LookupPublishedImage(name string) ([]byte, string, bool) {
	return publishedImages.get(strings.TrimSpace(name))
}

func (s *publishedImageStore) put(name string, img publishedImage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(time.Now())
	if _, exists := s.items[name]; exists {
		return
	}
	s.items[name] = img
	s.order = append(s.order, name)
	s.bytes += len(img.data)
	for len(s.order) > 0 && (len(s.order) > publishedImageMaxCount || s.bytes > publishedImageMaxBytes) {
		s.dropLocked(s.order[0])
	}
}

func (s *publishedImageStore) get(name string) ([]byte, string, bool) {
	if name == "" {
		return nil, "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.purgeExpiredLocked(now)
	img, ok := s.items[name]
	if !ok {
		return nil, "", false
	}
	return img.data, img.mime, true
}

func (s *publishedImageStore) purgeExpiredLocked(now time.Time) {
	for _, name := range s.order {
		img, ok := s.items[name]
		if !ok {
			continue
		}
		if img.expires.After(now) {
			break
		}
		s.dropLocked(name)
	}
}

func (s *publishedImageStore) dropLocked(name string) {
	img, ok := s.items[name]
	if !ok {
		return
	}
	delete(s.items, name)
	s.bytes -= len(img.data)
	for i := range s.order {
		if s.order[i] == name {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// normalizeImageMime strips any parameters and rejects non-image types.
func normalizeImageMime(mimeType string) string {
	mime := strings.ToLower(strings.TrimSpace(mimeType))
	if idx := strings.IndexByte(mime, ';'); idx >= 0 {
		mime = strings.TrimSpace(mime[:idx])
	}
	if !strings.HasPrefix(mime, "image/") {
		return ""
	}
	return mime
}

// imageExtension maps an image MIME type to the file extension callers should
// advertise, so a client can infer the format from the URL alone.
func imageExtension(mimeType string) string {
	switch normalizeImageMime(mimeType) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".png"
	}
}
