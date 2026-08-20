package cursor

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
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
	// publishedImageTTL bounds how long a hosted image stays fetchable.
	// Generated images run to several megabytes each, so the cache is kept to
	// a single day: long enough for the conversation the link belongs to,
	// short enough that the directory does not grow without bound.
	publishedImageTTL = 24 * time.Hour
	// publishedImageSweepEvery is how often expired images are deleted. New
	// images also trigger a prune, but a gateway that stops generating them
	// would otherwise keep the last day's files forever.
	publishedImageSweepEvery = time.Hour
	// publishedImageMaxBytes caps the cache directory.
	publishedImageMaxBytes = 2 << 30
	// publishedImageMemoryBytes caps what is kept in the heap on top of that;
	// the rest is served from disk.
	publishedImageMemoryBytes = 128 << 20
)

// publishedImageDirEnv overrides where hosted images are cached. The default
// lands under the user cache directory, which survives a restart — /tmp does
// not when the unit runs with PrivateTmp.
const publishedImageDirEnv = "CPA_IMAGE_CACHE_DIR"

type publishedImage struct {
	data    []byte
	mime    string
	expires time.Time
}

type publishedImageStore struct {
	mu sync.Mutex
	// dir is the on-disk cache. Empty means memory-only, which is what the
	// store falls back to when no directory can be created.
	dir         string
	dirReady    bool
	items       map[string]publishedImage
	order       []string
	bytes       int
	janitorOnce sync.Once
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

// StartPublishedImageJanitor deletes hosted images older than the retention
// window, both at startup and on a timer, for the life of the process. It is
// safe to call more than once.
func StartPublishedImageJanitor() {
	publishedImages.startJanitor()
}

func (s *publishedImageStore) startJanitor() {
	s.janitorOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(publishedImageSweepEvery)
			defer ticker.Stop()
			for {
				// Sweep first: a restart is the one moment a cache that
				// stopped receiving images gets cleaned up at all.
				if removed, freed := s.sweep(time.Now()); removed > 0 {
					log.Debugf("cursor image cache: deleted %d image(s) older than %s, freed %d bytes", removed, publishedImageTTL, freed)
				}
				<-ticker.C
			}
		}()
	})
}

// sweep drops every image past the retention window, on disk and in memory.
func (s *publishedImageStore) sweep(now time.Time) (removed int, freed int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, img := range s.items {
		if !img.expires.After(now) {
			s.forgetLocked(name)
		}
	}
	dir := s.cacheDirLocked()
	if dir == "" {
		return 0, 0
	}
	return s.pruneDirLocked(dir, now)
}

func (s *publishedImageStore) put(name string, img publishedImage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeFileLocked(name, img.data)
	if _, exists := s.items[name]; exists {
		return
	}
	s.items[name] = img
	s.order = append(s.order, name)
	s.bytes += len(img.data)
	// Only the bytes are evicted from memory; the file stays fetchable.
	for len(s.order) > 1 && s.bytes > publishedImageMemoryBytes {
		s.forgetLocked(s.order[0])
	}
}

func (s *publishedImageStore) get(name string) ([]byte, string, bool) {
	if !validPublishedImageName(name) {
		return nil, "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if img, ok := s.items[name]; ok {
		if img.expires.After(now) {
			if len(img.data) > 0 {
				return img.data, img.mime, true
			}
		} else {
			s.forgetLocked(name)
			s.removeFileLocked(name)
			return nil, "", false
		}
	}
	data, ok := s.readFileLocked(name, now)
	if !ok {
		return nil, "", false
	}
	mime := normalizeImageMime(http.DetectContentType(data))
	if mime == "" {
		mime = "application/octet-stream"
	}
	return data, mime, true
}

// forgetLocked drops one entry's bytes from the heap, leaving the file behind.
func (s *publishedImageStore) forgetLocked(name string) {
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

// cacheDirLocked resolves the on-disk cache directory once per process.
func (s *publishedImageStore) cacheDirLocked() string {
	if s.dirReady {
		return s.dir
	}
	s.dirReady = true
	dir := strings.TrimSpace(os.Getenv(publishedImageDirEnv))
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil || strings.TrimSpace(base) == "" {
			base = os.TempDir()
		}
		dir = filepath.Join(base, "cliproxy-api", "generated-images")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.dir = ""
		return ""
	}
	s.dir = dir
	return dir
}

func (s *publishedImageStore) writeFileLocked(name string, data []byte) {
	dir := s.cacheDirLocked()
	if dir == "" || !validPublishedImageName(name) {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		return
	}
	_, _ = s.pruneDirLocked(dir, time.Now())
}

func (s *publishedImageStore) readFileLocked(name string, now time.Time) ([]byte, bool) {
	dir := s.cacheDirLocked()
	if dir == "" {
		return nil, false
	}
	path := filepath.Join(dir, name)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil, false
	}
	if now.Sub(info.ModTime()) > publishedImageTTL {
		_ = os.Remove(path)
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return data, true
}

func (s *publishedImageStore) removeFileLocked(name string) {
	dir := s.cacheDirLocked()
	if dir == "" || !validPublishedImageName(name) {
		return
	}
	_ = os.Remove(filepath.Join(dir, name))
}

// pruneDirLocked drops expired files and, once the directory is over its size
// cap, the oldest ones until it fits again. It reports what it deleted.
func (s *publishedImageStore) pruneDirLocked(dir string, now time.Time) (removed int, freed int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return removed, freed
	}
	type cached struct {
		name    string
		size    int64
		modTime time.Time
	}
	files := make([]cached, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			continue
		}
		if now.Sub(info.ModTime()) > publishedImageTTL {
			if os.Remove(filepath.Join(dir, entry.Name())) == nil {
				removed++
				freed += info.Size()
			}
			continue
		}
		files = append(files, cached{name: entry.Name(), size: info.Size(), modTime: info.ModTime()})
		total += info.Size()
	}
	if total <= publishedImageMaxBytes {
		return removed, freed
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	for _, file := range files {
		if total <= publishedImageMaxBytes {
			return removed, freed
		}
		if err = os.Remove(filepath.Join(dir, file.name)); err == nil {
			total -= file.size
			removed++
			freed += file.size
		}
	}
	return removed, freed
}

// validPublishedImageName rejects anything that is not a name this store
// generated, which is also what keeps a request from escaping the cache
// directory.
func validPublishedImageName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '.', c == '-', c == '_':
		default:
			return false
		}
	}
	return !strings.Contains(name, "..")
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
