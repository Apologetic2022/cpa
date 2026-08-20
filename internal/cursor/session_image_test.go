package cursor

import (
	"encoding/base64"
	"strings"
	"testing"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
)

// Smallest valid 1x1 PNG.
var testPNGBase64 = base64.StdEncoding.EncodeToString([]byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E,
	0x44, 0xAE, 0x42, 0x60, 0x82,
})

func TestSniffImageMimeType(t *testing.T) {
	if got := sniffImageMimeType(testPNGBase64); got != "image/png" {
		t.Fatalf("png sniff = %q, want image/png", got)
	}
	if got := sniffImageMimeType("not base64!!"); got != "image/png" {
		t.Fatalf("invalid data fallback = %q, want image/png", got)
	}
}

func TestGeneratedImageDataURL(t *testing.T) {
	img := GeneratedImage{Base64: "QUJD", MimeType: "image/webp"}
	if got := img.DataURL(); got != "data:image/webp;base64,QUJD" {
		t.Fatalf("DataURL = %q", got)
	}
	img.MimeType = ""
	if got := img.DataURL(); !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("DataURL fallback = %q", got)
	}
}

func TestBuildGenerateImageApproval(t *testing.T) {
	query := &agentv1.InteractionQuery{
		Id: 7,
		Query: &agentv1.InteractionQuery_GenerateImageRequestQuery{
			GenerateImageRequestQuery: &agentv1.GenerateImageRequestQuery{
				Args:       &agentv1.GenerateImageArgs{Description: "a red fox"},
				ToolCallId: "call-1",
			},
		},
	}
	client := buildGenerateImageApproval(query)
	if client == nil {
		t.Fatal("expected approval message")
	}
	resp := client.GetInteractionResponse()
	if resp.GetId() != 7 {
		t.Fatalf("response id = %d, want 7", resp.GetId())
	}
	approved := resp.GetGenerateImageRequestResponse().GetApproved()
	if approved == nil || approved.GetDescription() != "a red fox" {
		t.Fatalf("unexpected approval payload: %#v", resp)
	}

	if got := buildGenerateImageApproval(&agentv1.InteractionQuery{Id: 3}); got != nil {
		t.Fatalf("expected nil for non-image query, got %#v", got)
	}
}

func TestHandleToolCallCompletedEmitsImage(t *testing.T) {
	s := &Session{events: make(chan StreamEvent, 4)}
	s.handleToolCallCompleted(&agentv1.ToolCallCompletedUpdate{
		CallId: "call-1",
		ToolCall: &agentv1.ToolCall{
			Tool: &agentv1.ToolCall_GenerateImageToolCall{
				GenerateImageToolCall: &agentv1.GenerateImageToolCall{
					Args: &agentv1.GenerateImageArgs{Description: "a red fox"},
					Result: &agentv1.GenerateImageResult{
						Result: &agentv1.GenerateImageResult_Success{
							Success: &agentv1.GenerateImageSuccess{
								FilePath:  "assets/fox.png",
								ImageData: testPNGBase64,
							},
						},
					},
				},
			},
		},
	})
	select {
	case ev := <-s.events:
		if ev.Type != "image" || ev.Image == nil {
			t.Fatalf("unexpected event: %#v", ev)
		}
		if ev.Image.Base64 != testPNGBase64 || ev.Image.MimeType != "image/png" || ev.Image.FilePath != "assets/fox.png" {
			t.Fatalf("unexpected image: %#v", ev.Image)
		}
	default:
		t.Fatal("expected image event")
	}
}

func TestHandleToolCallCompletedEmitsImageError(t *testing.T) {
	s := &Session{events: make(chan StreamEvent, 4)}
	s.handleToolCallCompleted(&agentv1.ToolCallCompletedUpdate{
		ToolCall: &agentv1.ToolCall{
			Tool: &agentv1.ToolCall_GenerateImageToolCall{
				GenerateImageToolCall: &agentv1.GenerateImageToolCall{
					Result: &agentv1.GenerateImageResult{
						Result: &agentv1.GenerateImageResult_Error{
							Error: &agentv1.GenerateImageError{Error: "quota exceeded"},
						},
					},
				},
			},
		},
	})
	select {
	case ev := <-s.events:
		if ev.Type != "image" || ev.Image != nil || ev.Message != "quota exceeded" {
			t.Fatalf("unexpected event: %#v", ev)
		}
	default:
		t.Fatal("expected image error event")
	}
}

func TestHandleToolCallCompletedIgnoresOtherTools(t *testing.T) {
	s := &Session{events: make(chan StreamEvent, 4)}
	s.handleToolCallCompleted(&agentv1.ToolCallCompletedUpdate{
		ToolCall: &agentv1.ToolCall{
			Tool: &agentv1.ToolCall_McpToolCall{McpToolCall: &agentv1.McpToolCall{}},
		},
	})
	select {
	case ev := <-s.events:
		t.Fatalf("unexpected event for mcp tool: %#v", ev)
	default:
	}
}
