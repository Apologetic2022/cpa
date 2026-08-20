package cursor

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	"google.golang.org/protobuf/proto"
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
	client := buildGenerateImageDecision(query, true)
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

	if got := buildGenerateImageDecision(&agentv1.InteractionQuery{Id: 3}, true); got != nil {
		t.Fatalf("expected nil for non-image query, got %#v", got)
	}
}

// A session that was not opened for image generation must turn the request
// down: that is what keeps a plain chat from spending image quota.
func TestBuildGenerateImageDecisionRejectsWhenNotAllowed(t *testing.T) {
	query := &agentv1.InteractionQuery{
		Id: 9,
		Query: &agentv1.InteractionQuery_GenerateImageRequestQuery{
			GenerateImageRequestQuery: &agentv1.GenerateImageRequestQuery{
				Args: &agentv1.GenerateImageArgs{Description: "a red fox"},
			},
		},
	}
	resp := buildGenerateImageDecision(query, false).GetInteractionResponse()
	if resp.GetGenerateImageRequestResponse().GetApproved() != nil {
		t.Fatal("plain chat approved image generation")
	}
	rejected := resp.GetGenerateImageRequestResponse().GetRejected()
	if rejected == nil || rejected.GetReason() == "" {
		t.Fatalf("expected a rejection with a reason, got %#v", resp)
	}
}

// Even if an image still arrives, a session without the opt-in must not
// surface it.
func TestHandleToolCallCompletedDropsImageWhenNotAllowed(t *testing.T) {
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
		t.Fatalf("image leaked into a plain chat: %#v", ev)
	default:
	}
}

func TestHandleToolCallCompletedEmitsImage(t *testing.T) {
	s := &Session{events: make(chan StreamEvent, 4), allowImages: true}
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
	s := &Session{events: make(chan StreamEvent, 4), allowImages: true}
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

// readClientFrames drains n Connect envelopes from r and decodes each as an
// AgentClientMessage.
func readClientFrames(t *testing.T, r io.Reader, n int) []*agentv1.AgentClientMessage {
	t.Helper()
	out := make([]*agentv1.AgentClientMessage, 0, n)
	header := make([]byte, 5)
	for i := 0; i < n; i++ {
		if _, err := io.ReadFull(r, header); err != nil {
			t.Fatalf("read envelope header %d: %v", i, err)
		}
		payload := make([]byte, binary.BigEndian.Uint32(header[1:5]))
		if _, err := io.ReadFull(r, payload); err != nil {
			t.Fatalf("read envelope payload %d: %v", i, err)
		}
		msg := &agentv1.AgentClientMessage{}
		if err := proto.Unmarshal(payload, msg); err != nil {
			t.Fatalf("unmarshal client message %d: %v", i, err)
		}
		out = append(out, msg)
	}
	return out
}

// runReadArgs drives one read exec against a session and returns the frames the
// client wrote back (the read result plus the stream close).
func runReadArgs(t *testing.T, s *Session, args *agentv1.ReadArgs) []*agentv1.AgentClientMessage {
	t.Helper()
	pr, pw := io.Pipe()
	s.stream = &BidiStream{writer: pw}

	framesCh := make(chan []*agentv1.AgentClientMessage, 1)
	go func() { framesCh <- readClientFrames(t, pr, 2) }()

	if err := s.handleReadArgs(&agentv1.ExecServerMessage{Id: 9, ExecId: "exec-1"}, args); err != nil {
		t.Fatalf("handleReadArgs: %v", err)
	}
	select {
	case frames := <-framesCh:
		return frames
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for read exec frames")
		return nil
	}
}

func TestHandleReadArgsServesReferenceImage(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(testPNGBase64)
	if err != nil {
		t.Fatal(err)
	}
	const path = "/ws/.cursor/projects/p/assets/references/reference-1.png"
	s := &Session{referenceImages: map[string][]byte{path: raw}}

	frames := runReadArgs(t, s, &agentv1.ReadArgs{Path: path, ToolCallId: "call-1"})

	exec := frames[0].GetExecClientMessage()
	if exec.GetId() != 9 || exec.GetExecId() != "exec-1" {
		t.Fatalf("exec envelope mismatch: id=%d exec_id=%q", exec.GetId(), exec.GetExecId())
	}
	success := exec.GetReadResult().GetSuccess()
	if success == nil {
		t.Fatalf("expected read success, got %#v", exec.GetReadResult().GetResult())
	}
	if success.GetPath() != path {
		t.Fatalf("path = %q", success.GetPath())
	}
	if !bytes.Equal(success.GetData(), raw) {
		t.Fatal("reference image bytes did not round-trip")
	}
	if success.GetFileSize() != int64(len(raw)) {
		t.Fatalf("file_size = %d, want %d", success.GetFileSize(), len(raw))
	}
	if frames[1].GetExecClientControlMessage().GetStreamClose().GetId() != 9 {
		t.Fatalf("expected stream close for id 9, got %#v", frames[1])
	}
}

func TestHandleReadArgsMatchesOnBasename(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(testPNGBase64)
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{referenceImages: map[string][]byte{"/original/dir/reference-1.png": raw}}

	frames := runReadArgs(t, s, &agentv1.ReadArgs{Path: "/somewhere/else/reference-1.png"})

	if success := frames[0].GetExecClientMessage().GetReadResult().GetSuccess(); success == nil {
		t.Fatal("expected basename fallback to serve the image")
	} else if !bytes.Equal(success.GetData(), raw) {
		t.Fatal("basename fallback returned wrong bytes")
	}
}

func TestHandleReadArgsRejectsUnknownPath(t *testing.T) {
	// A session with no reference images must not serve host files.
	s := &Session{}
	frames := runReadArgs(t, s, &agentv1.ReadArgs{Path: "/etc/passwd"})

	result := frames[0].GetExecClientMessage().GetReadResult()
	if result.GetFileNotFound() == nil {
		t.Fatalf("expected file_not_found, got %#v", result.GetResult())
	}
	if result.GetSuccess() != nil {
		t.Fatal("unknown path must not return content")
	}
}

func TestHandleReadArgsDoesNotServeUnrelatedReference(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(testPNGBase64)
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{referenceImages: map[string][]byte{"/ws/assets/references/reference-1.png": raw}}

	frames := runReadArgs(t, s, &agentv1.ReadArgs{Path: "/etc/shadow"})

	if frames[0].GetExecClientMessage().GetReadResult().GetFileNotFound() == nil {
		t.Fatal("expected file_not_found for a path outside the reference set")
	}
}

func TestWithReferenceImagesSeedsSession(t *testing.T) {
	s := &Session{}
	WithReferenceImages([]ReferenceImage{
		{Path: "/a/reference-1.png", Data: []byte{1, 2, 3}},
		{Path: "  ", Data: []byte{4}},
		{Path: "/a/reference-2.png"},
	})(s)

	if len(s.referenceImages) != 1 {
		t.Fatalf("expected only the valid entry to be seeded, got %d", len(s.referenceImages))
	}
	if _, ok := s.referenceImages["/a/reference-1.png"]; !ok {
		t.Fatalf("valid reference missing: %#v", s.referenceImages)
	}
}

func TestImageEditInstructionListsReferencePaths(t *testing.T) {
	refs := []ReferenceImage{
		{Path: "/ws/assets/references/reference-1.png"},
		{Path: "/ws/assets/references/reference-2.png"},
	}
	got := ImageEditInstruction("make it green", refs)
	for _, ref := range refs {
		if !strings.Contains(got, ref.Path) {
			t.Fatalf("instruction missing %q: %s", ref.Path, got)
		}
	}
	if !strings.Contains(got, "make it green") {
		t.Fatalf("instruction missing prompt: %s", got)
	}

	// With no references the edit wording must not leak into a plain generation.
	if got = ImageEditInstruction("a fox", nil); got != ImageGenerationInstruction("a fox") {
		t.Fatalf("expected plain generation instruction, got %q", got)
	}
}

func TestAttachedImageNoteListsPaths(t *testing.T) {
	refs := []ReferenceImage{
		{Path: "/ws/assets/references/reference-1.png"},
		{Path: "/ws/assets/references/reference-2.png"},
	}
	got := AttachedImageNote(refs)
	for _, ref := range refs {
		if !strings.Contains(got, ref.Path) {
			t.Fatalf("note missing %q: %s", ref.Path, got)
		}
	}
	if !strings.HasPrefix(got, "\n\n") {
		t.Fatalf("note must append to an existing turn: %q", got)
	}
	if AttachedImageNote(nil) != "" {
		t.Fatal("expected no note without references")
	}
	if AttachedImageNote([]ReferenceImage{{Path: "  "}}) != "" {
		t.Fatal("expected no note for a blank path")
	}
}

func TestReferenceImagePathExtension(t *testing.T) {
	for mime, suffix := range map[string]string{
		"image/png":  "reference-1.png",
		"image/jpeg": "reference-1.jpg",
		"image/webp": "reference-1.webp",
		"image/gif":  "reference-1.gif",
		"":           "reference-1.png",
	} {
		if got := ReferenceImagePath(0, mime); !strings.HasSuffix(got, suffix) {
			t.Fatalf("ReferenceImagePath(0, %q) = %q, want suffix %q", mime, got, suffix)
		}
	}
	if got := ReferenceImagePath(2, "image/png"); !strings.HasSuffix(got, "reference-3.png") {
		t.Fatalf("index not 1-based: %q", got)
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
