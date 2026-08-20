package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path"
	"testing"

	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
)

var testGeneratedPNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E,
	0x44, 0xAE, 0x42, 0x60, 0x82,
}

func TestGeneratedImageRouteServesWithoutAPIKey(t *testing.T) {
	server := newTestServer(t)
	path := cursorlib.PublishImageBytes(testGeneratedPNG, "image/png")
	if path == "" {
		t.Fatal("failed to publish test image")
	}

	// No Authorization header: the chat client fetching the image is a
	// markdown renderer and never carries the caller's API key.
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("unexpected content type %q", got)
	}
	if !bytes.Equal(rr.Body.Bytes(), testGeneratedPNG) {
		t.Fatalf("served bytes differ from the published image")
	}
}

// A reply that names no host is fetched by the client against the base URL it
// was configured with, so the same bytes have to answer from inside the API
// namespace — and still without a key, because an <img> sends none.
func TestGeneratedImageServedFromAPINamespace(t *testing.T) {
	server := newTestServer(t)
	name := path.Base(cursorlib.PublishImageBytes(testGeneratedPNG, "image/png"))

	req := httptest.NewRequest(http.MethodGet, cursorlib.PublishedImageAPIPathPrefix+name, nil)
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("unexpected content type %q", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("a cross-origin renderer cannot read the response: %q", got)
	}
	if !bytes.Equal(rr.Body.Bytes(), testGeneratedPNG) {
		t.Fatalf("served bytes differ from the published image")
	}
}

// Some clients join a host-less reference onto a base URL that carries a path
// of its own. The prefix never reaches this gateway as a route, but the name
// still identifies the image.
func TestGeneratedImageServedUnderAClientBasePathPrefix(t *testing.T) {
	server := newTestServer(t)
	name := path.Base(cursorlib.PublishImageBytes(testGeneratedPNG, "image/png"))

	for _, requestPath := range []string{
		"/grok-4.6" + cursorlib.PublishedImageAPIPathPrefix + name,
		"/some/deep/base" + cursorlib.PublishedImagePathPrefix + name,
	} {
		req := httptest.NewRequest(http.MethodGet, requestPath, nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("%s returned %d: %s", requestPath, rr.Code, rr.Body.String())
		}
		if !bytes.Equal(rr.Body.Bytes(), testGeneratedPNG) {
			t.Fatalf("%s served different bytes", requestPath)
		}
	}
}

// Links handed out before the prefix changed live in chat histories, so the
// old shape has to keep resolving for as long as the bytes are cached.
func TestGeneratedImageServedUnderLegacyPrefix(t *testing.T) {
	server := newTestServer(t)
	name := path.Base(cursorlib.PublishImageBytes(testGeneratedPNG, "image/png"))

	req := httptest.NewRequest(http.MethodGet, cursorlib.PublishedImageLegacyPathPrefix+name, nil)
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("legacy path returned %d: %s", rr.Code, rr.Body.String())
	}
	if !bytes.Equal(rr.Body.Bytes(), testGeneratedPNG) {
		t.Fatalf("legacy path served different bytes")
	}
}

func TestUnrelatedPathsStillFourOhFour(t *testing.T) {
	server := newTestServer(t)
	for _, requestPath := range []string{
		"/v1/images/../../etc/passwd",
		cursorlib.PublishedImagePathPrefix,
		"/v1/images/not-a-published-name",
		"/totally/unrelated",
	} {
		req := httptest.NewRequest(http.MethodGet, requestPath, nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code == http.StatusOK {
			t.Fatalf("%s unexpectedly served a body: %s", requestPath, rr.Body.String())
		}
	}
}

func TestGeneratedImageRouteUnknownName(t *testing.T) {
	server := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, cursorlib.PublishedImagePathPrefix+"deadbeef.png", nil)
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unexpected status %d for an unknown image", rr.Code)
	}
}
