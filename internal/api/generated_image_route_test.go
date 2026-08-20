package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
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

func TestGeneratedImageRouteUnknownName(t *testing.T) {
	server := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, cursorlib.PublishedImagePathPrefix+"deadbeef.png", nil)
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unexpected status %d for an unknown image", rr.Code)
	}
}
