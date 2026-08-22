package cursor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// splitAgentServerRecords splits an envelope payload into one chunk per
// top-level field record. AgentServerMessage is a bare oneof, but upstream
// sometimes packs several records into a single frame (the first text_delta
// piggybacks on the server_metrics frame). A plain proto.Unmarshal keeps only
// the last oneof record on the wire, silently dropping the earlier ones —
// which lost the first streamed token of every reply. Returns nil when the
// payload is not well-formed wire format so the caller can fall back to
// decoding the payload as-is.
func splitAgentServerRecords(payload []byte) [][]byte {
	var records [][]byte
	rest := payload
	for len(rest) > 0 {
		num, typ, n := protowire.ConsumeTag(rest)
		if n < 0 {
			return nil
		}
		m := protowire.ConsumeFieldValue(num, typ, rest[n:])
		if m < 0 {
			return nil
		}
		records = append(records, rest[:n+m])
		rest = rest[n+m:]
	}
	return records
}

// MCPProviderIdentifier is the provider_identifier advertised for OpenAI tools.
const MCPProviderIdentifier = "cliproxyapi"

// ToolDefinition is an OpenAI-style function tool projected into Cursor MCP tools.
type ToolDefinition struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ToolCall is a model-requested function invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}

// ToolResult is a client-provided tool response.
type ToolResult struct {
	ToolCallID string
	Name       string
	Content    string
	IsError    bool
}

// GeneratedImage is one image produced by Cursor's server-side GenerateImage tool.
type GeneratedImage struct {
	// Base64 is the raw base64-encoded image payload (no data-URL prefix).
	Base64 string
	// MimeType is sniffed from the decoded bytes; defaults to image/png.
	MimeType string
	// FilePath is the path the desktop client would have saved the image to.
	FilePath string
}

// ReferenceImage is one caller-supplied input image for image-to-image
// generation. Path is the workspace path advertised to Cursor through
// GenerateImageArgs.reference_image_paths; Data holds the decoded bytes that
// the read exec serves when the server fetches that path.
type ReferenceImage struct {
	Path     string
	Data     []byte
	MimeType string
}

// DataURL renders the image as an inline data URL.
func (g GeneratedImage) DataURL() string {
	mime := g.MimeType
	if mime == "" {
		mime = "image/png"
	}
	return "data:" + mime + ";base64," + g.Base64
}

// StreamEvent is one Cursor→OpenAI segment event.
type StreamEvent struct {
	Type             string
	Text             string
	ToolCall         *ToolCall
	Image            *GeneratedImage
	Reason           string
	Message          string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
}

type pendingExec struct {
	request *agentv1.ExecServerMessage
	call    ToolCall
}

// Session is a live Agent Connect run that can pause for client tools.
type Session struct {
	ID             string
	ConversationID string
	Model          string

	mu           sync.Mutex
	stream       *BidiStream
	blobStore    map[string][]byte
	tools        []ToolDefinition
	toolIndex    map[string]*ToolDefinition
	pending      map[string]*pendingExec
	events       chan StreamEvent
	errCh        chan error
	closed       bool
	finished     bool
	waitingTools bool
	pauseCh      chan struct{}
	cancel       context.CancelFunc
	lastActivity time.Time
	manager      *SessionManager

	// writtenImages holds image bytes delivered by write_args execs, keyed by
	// file path. GenerateImage completions reference these when the completed
	// tool call does not embed image_data itself.
	writtenImages map[string][]byte

	// referenceImages holds caller-supplied input images for image-to-image
	// runs, keyed by the workspace path advertised to Cursor. Read execs are
	// answered from this map because the headless client has no real
	// workspace on disk. Seeded before the read loop starts and never mutated
	// afterwards, so reads may outlive a single tool call.
	referenceImages map[string][]byte

	// allowImages opts this session into server-side image generation. It is
	// off by default so a plain chat can never spend image quota: the Agent
	// asks for approval before GenerateImage runs, and an unopted session
	// rejects that request. Only the image model and the /v1/images endpoints
	// turn it on.
	allowImages bool

	// renameToolsOnWire mirrors the tool-name namespacing applied when the
	// run was built (see claudeWireModel): incoming tool calls resolve the
	// prefixed name back to the client's definition, and tool listings shown
	// to the model use the wire names.
	renameToolsOnWire bool

	// Conversation continuation state (see conversation_cache.go). transcript
	// mirrors the conversation exactly as the client will echo it back on its
	// next request: the request messages, then each assistant segment, tool
	// results, and the closing assistant reply as they stream. checkpoint is
	// the latest ConversationStateStructure the server pushed for this run;
	// replaying it under the same conversation id continues the conversation
	// and keeps Cursor's provider prompt cache warm.
	accountKey      string
	transcript      []ChatMessage
	segText         strings.Builder
	segCalls        []ToolCall
	checkpoint      *agentv1.ConversationStateStructure
	ckptCount       int
	ckptAfterEnd    bool
	outputAfterCkpt bool
	turnEnded       bool
	resumed         bool
	resumeKey       string
	everOutput      bool
	snapshotStored  bool
	// pendingResolve releases the conversation-cache pending marker announced
	// at turn_ended. A follow-up request racing the trailing checkpoint waits
	// on that marker instead of missing the cache.
	pendingResolve func()
}

// ChatResult is the collected text response from one Agent segment / run.
type ChatResult struct {
	Text             string
	Thinking         string
	ToolCalls        []ToolCall
	Images           []GeneratedImage
	ImageError       string
	FinishReason     string
	ConversationID   string
	SessionID        string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
}

// SessionOption customises a session before its read loop starts.
type SessionOption func(*Session)

// WithImageGeneration allows the Agent to run its built-in GenerateImage tool
// on this session. Without it the approval request is rejected, so the model
// falls back to a text answer instead of producing an image.
func WithImageGeneration() SessionOption {
	return func(s *Session) { s.allowImages = true }
}

// WithReferenceImages seeds the input images an image-to-image run serves back
// over the read exec. It must be applied at construction time: Cursor can ask
// for the bytes as soon as the run request is on the wire.
func WithReferenceImages(refs []ReferenceImage) SessionOption {
	return func(s *Session) {
		if len(refs) == 0 {
			return
		}
		s.referenceImages = make(map[string][]byte, len(refs))
		for _, ref := range refs {
			path := strings.TrimSpace(ref.Path)
			if path == "" || len(ref.Data) == 0 {
				continue
			}
			s.referenceImages[path] = ref.Data
			// Best-effort only; the read exec answers from memory regardless.
			writeHeadlessWorkspaceFile(path, ref.Data)
		}
	}
}

// StartSession opens a new Agent run for the given messages/tools.
func StartSession(ctx context.Context, creds AccountCredentials, model string, messages []ChatMessage, tools []ToolDefinition, opts ...SessionOption) (*Session, error) {
	if strings.TrimSpace(creds.AccessToken) == "" {
		return nil, fmt.Errorf("cursor: access_token is required")
	}
	selection := ResolveRequestedModel(model)
	// Options are applied before the run request is built: whether the run may
	// generate images decides what the Agent is told it can do.
	session := &Session{}
	for _, opt := range opts {
		if opt != nil {
			opt(session)
		}
	}

	// Continue a checkpointed conversation when the request's history matches
	// a turn this gateway already ran on the same account. A fresh
	// conversation id would drop Cursor's provider-side prompt cache and
	// re-bill the entire prefix.
	var clientMsg *agentv1.AgentClientMessage
	var blobStore map[string][]byte
	var conversationID string
	session.accountKey = accountKeyFromCredentials(creds)
	if !session.allowImages && conversationReuseEnabled() && session.accountKey != "" {
		// The stored fingerprint always ends at the previous assistant reply,
		// so the lookup must treat the whole trailing run of user messages as
		// the new turn: agent front-ends (Cursor CLI style) prepend reminders
		// and todo lists to the actual question as separate user messages,
		// and hashing those into the transcript made every follow-up miss.
		prefix, turn := splitTrailingUserRun(messages)
		userText := joinedUserText(turn)
		var entry *convEntry
		var fingerprint string
		modelMismatch := ""
		if len(prefix) > 0 && userText != "" {
			fingerprint = conversationFingerprint(prefix)
			if found, ok := defaultConversationCache.Lookup(session.accountKey, fingerprint); ok {
				if found.model == selection.ModelID {
					entry = found
				} else {
					modelMismatch = found.model
				}
			}
		}
		resumeMode := "turn"
		if entry == nil && modelMismatch == "" {
			// No checkpoint for the trailing-user-run split. Probe older
			// turn-end boundaries: a tool-result continuation whose live
			// session is gone ends in tool messages, and a client may have
			// rewritten its recent history. Folding the un-checkpointed tail
			// into the resumed turn keeps the provider cache for everything
			// before it.
			if found, fp, folded, ok := lookupTurnBoundaryResume(session.accountKey, selection.ModelID, messages); ok {
				entry, fingerprint, userText = found, fp, folded
				resumeMode = "fold"
			}
		}
		switch {
		case entry != nil:
			cm, bs, cid, errResume := buildResumeRunRequest(model, entry, userText, tools)
			if errResume == nil {
				clientMsg, blobStore, conversationID = cm, bs, cid
				session.resumed = true
				session.resumeKey = fingerprint
				log.Infof("cursor: resuming conversation %s from checkpoint (account=%s model=%s mode=%s)", cid, session.accountKey, selection.ModelID, resumeMode)
			} else {
				log.Warnf("cursor: checkpoint found but resume request build failed, replaying full history: %v", errResume)
			}
		case modelMismatch != "":
			// Model switched mid-conversation: the checkpointed upstream
			// conversation is pinned to another model, so it cannot be
			// continued. Info because it explains a full re-bill.
			log.Infof("cursor: checkpoint model mismatch, replaying full history (account=%s have=%s want=%s)", session.accountKey, modelMismatch, selection.ModelID)
		case historyHasAssistant(messages):
			// A follow-up turn with no checkpoint replays the entire
			// history uncached; log it so cache misses are visible in
			// production without debug logging.
			log.Infof("cursor: no checkpoint for follow-up, replaying full history (account=%s model=%s history=%d)", session.accountKey, selection.ModelID, len(prefix))
		}
	}
	if clientMsg == nil {
		var err error
		clientMsg, blobStore, conversationID, err = buildRunRequest(model, messages, tools, session.allowImages)
		if err != nil {
			return nil, err
		}
	}
	first, err := proto.Marshal(clientMsg)
	if err != nil {
		return nil, err
	}

	profile := ProfileFromCredentials(creds)
	requestID := uuid.NewString()
	headers, err := profile.Headers(creds.AccessToken, requestID, "")
	if err != nil {
		return nil, err
	}
	headers["x-original-request-id"] = requestID

	// Detach the Agent H2 stream from the inbound HTTP request context.
	// Tool round-trips span multiple client requests; cancelling with the
	// first response would close the pipe before mcp_result can be written.
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	stream, err := OpenAgentRun(runCtx, creds.BaseURL, headers, first)
	if err != nil && session.resumed {
		// A rejected checkpoint must not wedge the conversation: drop the
		// cached entry and rebuild the run from the full request history.
		defaultConversationCache.Invalidate(session.accountKey, session.resumeKey)
		session.resumed = false
		session.resumeKey = ""
		log.Warnf("cursor: checkpoint resume rejected, rebuilding conversation: %v", err)
		clientMsg, blobStore, conversationID, err = buildRunRequest(model, messages, tools, session.allowImages)
		if err != nil {
			cancel()
			return nil, err
		}
		if first, err = proto.Marshal(clientMsg); err != nil {
			cancel()
			return nil, err
		}
		stream, err = OpenAgentRun(runCtx, creds.BaseURL, headers, first)
	}
	if err != nil {
		cancel()
		return nil, err
	}
	if profile.CookieJar != nil {
		profile.CookieJar.RememberResponse(creds.BaseURL, stream.ResponseHeader())
	}

	session.ID = uuid.NewString()
	session.ConversationID = conversationID
	session.Model = selection.ModelID
	session.renameToolsOnWire = claudeWireModel(selection.ModelID)
	session.stream = stream
	session.blobStore = blobStore
	session.transcript = echoTranscript(messages)
	session.tools = append([]ToolDefinition(nil), tools...)
	session.toolIndex = indexTools(tools)
	session.pending = map[string]*pendingExec{}
	session.events = make(chan StreamEvent, 64)
	session.errCh = make(chan error, 1)
	session.pauseCh = make(chan struct{})
	session.cancel = cancel
	session.lastActivity = time.Now()
	session.manager = DefaultSessionManager()
	session.manager.Register(session)
	go session.heartbeatLoop(runCtx)
	go session.readLoop(runCtx)
	return session, nil
}

// echoTranscript copies the request messages into the transcript mirror,
// dropping the synthetic lost-session replay note. The client never saw that
// message and will not echo it back, so hashing it into the stored checkpoint
// fingerprint would make every later turn of the conversation miss the cache
// — one lost tool session used to un-cache a conversation permanently.
func echoTranscript(messages []ChatMessage) []ChatMessage {
	out := append([]ChatMessage(nil), messages...)
	for len(out) > 0 {
		last := &out[len(out)-1]
		if last.Role == "user" && strings.HasPrefix(strings.TrimSpace(last.Content), replayNoteMarker) {
			out = out[:len(out)-1]
			continue
		}
		break
	}
	return out
}

func indexTools(tools []ToolDefinition) map[string]*ToolDefinition {
	out := make(map[string]*ToolDefinition, len(tools))
	for i := range tools {
		name := strings.TrimSpace(tools[i].Name)
		if name == "" {
			continue
		}
		out[name] = &tools[i]
	}
	return out
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastActivity = time.Now()
	s.mu.Unlock()
}

func (s *Session) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			closed := s.closed
			stream := s.stream
			s.mu.Unlock()
			if closed || stream == nil {
				return
			}
			hb := &agentv1.AgentClientMessage{
				Message: &agentv1.AgentClientMessage_ClientHeartbeat{ClientHeartbeat: &agentv1.ClientHeartbeat{}},
			}
			payload, err := proto.Marshal(hb)
			if err != nil {
				continue
			}
			_ = stream.WriteEnvelope(payload, false)
		}
	}
}

func (s *Session) readLoop(ctx context.Context) {
	defer close(s.events)
	decoder := NewDecoder()
	if s.stream != nil {
		if enc := strings.TrimSpace(s.stream.ResponseHeader().Get("Connect-Content-Encoding")); enc != "" {
			decoder.SetCompression(enc)
		} else if enc := strings.TrimSpace(s.stream.ResponseHeader().Get("connect-content-encoding")); enc != "" {
			decoder.SetCompression(enc)
		}
	}
	readBuf := make([]byte, 32*1024)
	for {
		s.mu.Lock()
		waiting := s.waitingTools
		closed := s.closed
		stream := s.stream
		pauseCh := s.pauseCh
		s.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-ctx.Done():
			s.mu.Lock()
			alreadyClosed := s.closed
			s.mu.Unlock()
			if !alreadyClosed {
				s.emit(StreamEvent{Type: "error", Message: ctx.Err().Error()})
				s.emit(StreamEvent{Type: "segment_end", Reason: "error"})
			}
			return
		default:
		}
		if waiting {
			select {
			case <-ctx.Done():
				return
			case <-pauseCh:
				continue
			}
		}

		n, errRead := stream.Read(readBuf)
		if n > 0 {
			envelopes, errFeed := decoder.Feed(readBuf[:n])
			if errFeed != nil {
				s.fail(errFeed)
				return
			}
			endForTools := false
			for _, env := range envelopes {
				if env.EndStream() {
					debugDumpFrame(env, nil)
					if len(env.Payload) > 0 {
						var trailer struct {
							Error json.RawMessage `json:"error"`
						}
						_ = json.Unmarshal(env.Payload, &trailer)
						if len(trailer.Error) > 0 && string(trailer.Error) != "null" {
							s.fail(fmt.Errorf("cursor connect end-stream error: %s", string(trailer.Error)))
							return
						}
					}
					s.storeConversationSnapshot()
					s.markFinished()
					s.emit(StreamEvent{Type: "segment_end", Reason: "stop"})
					_ = s.closeWith("upstream_end_stream")
					return
				}
				chunks := splitAgentServerRecords(env.Payload)
				if len(chunks) == 0 {
					chunks = [][]byte{env.Payload}
				}
				for _, chunk := range chunks {
					serverMsg := &agentv1.AgentServerMessage{}
					if err := proto.Unmarshal(chunk, serverMsg); err != nil {
						s.fail(fmt.Errorf("cursor decode server message: %w", err))
						return
					}
					debugDumpFrame(env, serverMsg)
					pause, err := s.handleServerMessage(serverMsg)
					if err != nil {
						s.fail(err)
						return
					}
					if pause {
						endForTools = true
					}
				}
			}
			if endForTools {
				s.flushAssistantSegment()
				s.beginWaitingTools()
				s.emit(StreamEvent{Type: "segment_end", Reason: "tool_calls"})
			}
		}
		if errRead == io.EOF {
			s.storeConversationSnapshot()
			s.markFinished()
			s.emit(StreamEvent{Type: "segment_end", Reason: "stop"})
			_ = s.closeWith("upstream_eof")
			return
		}
		if errRead != nil {
			s.fail(errRead)
			return
		}
		if n == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (s *Session) beginWaitingTools() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waitingTools {
		return
	}
	s.waitingTools = true
	s.pauseCh = make(chan struct{})
}

func (s *Session) resumeReading() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.waitingTools {
		return
	}
	s.waitingTools = false
	close(s.pauseCh)
	s.pauseCh = make(chan struct{})
}

func (s *Session) emit(ev StreamEvent) {
	s.touch()
	select {
	case s.events <- ev:
	default:
		// Drop if consumer is gone; Close will unwind.
		select {
		case s.events <- ev:
		case <-time.After(2 * time.Second):
		}
	}
}

// markFinished records that the turn delivered its terminal event. Tearing the
// Agent stream down afterwards makes the in-flight read fail with "http2:
// response body closed", and reporting that would turn a complete answer into
// a failed one.
func (s *Session) markFinished() {
	s.mu.Lock()
	s.finished = true
	s.mu.Unlock()
}

// flushAssistantSegmentLocked appends the streamed assistant step (text plus
// any client tool calls) to the transcript mirror, matching the assistant
// message the client will echo back on its next request.
func (s *Session) flushAssistantSegmentLocked() {
	text := s.segText.String()
	calls := s.segCalls
	if strings.TrimSpace(text) == "" && len(calls) == 0 {
		return
	}
	s.transcript = append(s.transcript, ChatMessage{Role: "assistant", Content: text, ToolCalls: calls})
	s.segText.Reset()
	s.segCalls = nil
}

// finalTranscriptLocked renders the transcript exactly as flushing would leave
// it, without consuming the live segment buffers. It exists so the transcript
// fingerprint can be computed at turn_ended, before the trailing checkpoint
// has been stored.
func (s *Session) finalTranscriptLocked() []ChatMessage {
	out := append([]ChatMessage(nil), s.transcript...)
	text := s.segText.String()
	calls := s.segCalls
	if strings.TrimSpace(text) != "" || len(calls) > 0 {
		out = append(out, ChatMessage{Role: "assistant", Content: text, ToolCalls: calls})
	}
	return out
}

func (s *Session) flushAssistantSegment() {
	s.mu.Lock()
	s.flushAssistantSegmentLocked()
	s.mu.Unlock()
}

// checkpointGraceWindow is how long a finished run keeps its stream open
// waiting for the server's trailing conversation_checkpoint_update.
const checkpointGraceWindow = 3 * time.Second

// finishAfterCheckpoint delays stream teardown after turn_ended until the
// server's conversation checkpoint (which trails the turn) has been read, then
// snapshots and closes. The client-visible segment already ended, so the wait
// adds no latency to the response.
func (s *Session) finishAfterCheckpoint() {
	deadline := time.Now().Add(checkpointGraceWindow)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		done := s.ckptAfterEnd || s.closed
		s.mu.Unlock()
		if done {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.storeConversationSnapshot()
	_ = s.closeWith("turn_ended")
}

// storeConversationSnapshot publishes the finished turn's checkpoint to the
// conversation cache, keyed by the transcript the client will echo back
// before its next user message. Only a checkpoint that already reflects the
// whole reply is trusted: one that postdates turn_ended, or the latest one as
// long as no output streamed after it (the frames race, so the final
// checkpoint sometimes lands just before turn_ended). A checkpoint with
// output after it may be missing that output, and resuming from it would
// silently drop part of the reply from the conversation.
func (s *Session) storeConversationSnapshot() {
	if s.allowImages || !conversationReuseEnabled() {
		return
	}
	s.mu.Lock()
	complete := s.ckptAfterEnd || !s.outputAfterCkpt
	if s.snapshotStored || s.accountKey == "" || s.checkpoint == nil || !complete {
		s.mu.Unlock()
		return
	}
	s.snapshotStored = true
	s.flushAssistantSegmentLocked()
	transcript := append([]ChatMessage(nil), s.transcript...)
	state := s.checkpoint
	blobs := make(map[string][]byte, len(s.blobStore))
	for k, v := range s.blobStore {
		blobs[k] = v
	}
	convID := s.ConversationID
	model := s.Model
	accountKey := s.accountKey
	s.mu.Unlock()
	fingerprint := conversationFingerprint(transcript)
	defaultConversationCache.Store(accountKey, fingerprint, &convEntry{
		conversationID: convID,
		state:          state,
		blobs:          blobs,
		model:          model,
	})
	s.resolvePending()
	log.Infof("cursor: stored conversation checkpoint conv=%s account=%s turns=%d", convID, accountKey, len(state.GetTurns()))
}

// resolvePending releases the pending-store marker, waking lookups that were
// waiting for this turn's checkpoint. Safe to call at any point after
// turn_ended, including when the store was abandoned.
func (s *Session) resolvePending() {
	s.mu.Lock()
	resolve := s.pendingResolve
	s.mu.Unlock()
	if resolve != nil {
		resolve()
	}
}

func (s *Session) fail(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	finished := s.finished
	resumedNoOutput := s.resumed && !s.everOutput
	s.mu.Unlock()
	// A resume that dies before producing anything points at a checkpoint the
	// server would not restore; dropping it makes the client's retry rebuild
	// the conversation from scratch instead of looping on the same failure.
	if resumedNoOutput {
		defaultConversationCache.Invalidate(s.accountKey, s.resumeKey)
	}
	if finished {
		log.Debugf("cursor session torn down after a finished turn: id=%s err=%v", s.ID, err)
		_ = s.closeWith("teardown")
		return
	}
	select {
	case s.errCh <- err:
	default:
	}
	log.Debugf("cursor session failed: id=%s err=%v", s.ID, err)
	s.emit(StreamEvent{Type: "error", Message: err.Error()})
	s.emit(StreamEvent{Type: "segment_end", Reason: "error"})
	_ = s.closeWith("stream_error")
}

// CollectSegment drains events until segment_end.
func (s *Session) CollectSegment(ctx context.Context) (*ChatResult, error) {
	result := &ChatResult{
		ConversationID: s.ConversationID,
		SessionID:      s.ID,
		FinishReason:   "stop",
	}
	var text strings.Builder
	var thinking strings.Builder
	for {
		select {
		case <-ctx.Done():
			_ = s.closeWith("collect_ctx_done")
			return nil, ctx.Err()
		case err := <-s.errCh:
			if err != nil {
				return nil, err
			}
		case ev, ok := <-s.events:
			if !ok {
				result.Text = text.String()
				result.Thinking = thinking.String()
				return result, nil
			}
			switch ev.Type {
			case "text_delta":
				text.WriteString(ev.Text)
			case "thinking_delta":
				thinking.WriteString(ev.Text)
			case "tool_call":
				if ev.ToolCall != nil {
					result.ToolCalls = append(result.ToolCalls, *ev.ToolCall)
				}
			case "image":
				if ev.Image != nil {
					result.Images = append(result.Images, *ev.Image)
				}
				if ev.Message != "" {
					result.ImageError = ev.Message
				}
			case "usage_final":
				result.InputTokens = ev.InputTokens
				result.OutputTokens = ev.OutputTokens
				result.CacheReadTokens = ev.CacheReadTokens
				result.CacheWriteTokens = ev.CacheWriteTokens
				result.ReasoningTokens = ev.ReasoningTokens
			case "error":
				return nil, fmt.Errorf("cursor: %s", ev.Message)
			case "segment_end":
				result.Text = text.String()
				result.Thinking = thinking.String()
				if ev.Reason != "" {
					result.FinishReason = ev.Reason
				}
				if result.FinishReason == "tool_calls" && len(result.ToolCalls) == 0 {
					result.FinishReason = "stop"
				}
				return result, nil
			}
		}
	}
}

// IterSegment invokes fn for each event until segment_end (inclusive).
func (s *Session) IterSegment(ctx context.Context, fn func(StreamEvent) error) error {
	for {
		select {
		case <-ctx.Done():
			_ = s.closeWith("iter_ctx_done")
			return ctx.Err()
		case err := <-s.errCh:
			if err != nil {
				return err
			}
		case ev, ok := <-s.events:
			if !ok {
				return nil
			}
			if err := fn(ev); err != nil {
				return err
			}
			if ev.Type == "segment_end" {
				return nil
			}
			if ev.Type == "error" {
				return fmt.Errorf("cursor: %s", ev.Message)
			}
		}
	}
}

// SubmitToolResults replies on the live exec stream and resumes reading.
func (s *Session) SubmitToolResults(results []ToolResult) error {
	if s == nil {
		return fmt.Errorf("cursor: nil session")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("%w (session closed)", ErrToolSessionLost)
	}
	pending := make([]*pendingExec, 0, len(results))
	for _, result := range results {
		item, ok := s.pending[NormalizeToolCallID(result.ToolCallID)]
		if !ok {
			s.mu.Unlock()
			return fmt.Errorf("%w (no pending tool call %s)", ErrToolSessionLost, result.ToolCallID)
		}
		pending = append(pending, item)
	}
	s.mu.Unlock()

	for i, result := range results {
		item := pending[i]
		if err := s.sendMcpResult(item.request, result); err != nil {
			return err
		}
		key := NormalizeToolCallID(result.ToolCallID)
		s.mu.Lock()
		delete(s.pending, key)
		s.transcript = append(s.transcript, ChatMessage{
			Role:       "tool",
			Name:       result.Name,
			ToolCallID: result.ToolCallID,
			Content:    result.Content,
		})
		s.mu.Unlock()
		if s.manager != nil {
			s.manager.UnbindPending(key)
		}
	}
	s.resumeReading()
	return nil
}

func (s *Session) sendMcpResult(req *agentv1.ExecServerMessage, result ToolResult) error {
	mcp := &agentv1.McpResult{
		Result: &agentv1.McpResult_Success{
			Success: &agentv1.McpSuccess{
				Content: []*agentv1.McpToolResultContentItem{
					{
						Content: &agentv1.McpToolResultContentItem_Text{
							Text: &agentv1.McpTextContent{Text: result.Content},
						},
					},
				},
				IsError: result.IsError,
			},
		},
	}
	execClient := &agentv1.ExecClientMessage{
		Id:     req.Id,
		ExecId: req.ExecId,
		Message: &agentv1.ExecClientMessage_McpResult{
			McpResult: mcp,
		},
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: execClient},
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	if err = s.stream.WriteEnvelope(payload, false); err != nil {
		return err
	}
	return sendExecStreamClose(s.stream, req.Id)
}

// Close tears down the Agent stream. After a cleanly finished turn the
// teardown is deferred to finishAfterCheckpoint, which is still waiting for
// the trailing conversation checkpoint on the open stream.
func (s *Session) Close() error {
	s.mu.Lock()
	graceful := s.turnEnded && !s.closed
	s.mu.Unlock()
	if graceful {
		return nil
	}
	return s.closeWith("explicit")
}

// closeWith records why a session ended. Tool round-trips span several client
// requests, so a premature close only shows up later as an unknown
// tool_call_id; the reason is the only way to tell those cases apart.
func (s *Session) closeWith(reason string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	pending := len(s.pending)
	stream := s.stream
	cancel := s.cancel
	resolve := s.pendingResolve
	s.mu.Unlock()
	// A session that ends without storing its snapshot must still release the
	// pending marker, or a racing lookup would block for the full wait.
	if resolve != nil {
		resolve()
	}
	log.Debugf("cursor session close: id=%s reason=%s pending_tools=%d", s.ID, reason, pending)
	if cancel != nil {
		cancel()
	}
	if s.manager != nil {
		s.manager.Remove(s)
	}
	if stream != nil {
		return stream.Close()
	}
	return nil
}

func (s *Session) handleServerMessage(msg *agentv1.AgentServerMessage) (pauseForTools bool, err error) {
	switch m := msg.Message.(type) {
	case *agentv1.AgentServerMessage_InteractionUpdate:
		switch u := m.InteractionUpdate.Message.(type) {
		case *agentv1.InteractionUpdate_TextDelta:
			s.mu.Lock()
			s.everOutput = true
			s.outputAfterCkpt = true
			s.segText.WriteString(u.TextDelta.GetText())
			s.mu.Unlock()
			s.emit(StreamEvent{Type: "text_delta", Text: u.TextDelta.GetText()})
		case *agentv1.InteractionUpdate_ThinkingDelta:
			s.mu.Lock()
			s.everOutput = true
			s.outputAfterCkpt = true
			s.mu.Unlock()
			s.emit(StreamEvent{Type: "thinking_delta", Text: u.ThinkingDelta.GetText()})
		case *agentv1.InteractionUpdate_TurnEnded:
			// cursor2api: some transports emit turn_ended for the pre-tool
			// segment before mcp_result is returned. Keep the bidi run open
			// while client tools are still pending.
			s.mu.Lock()
			pendingCount := len(s.pending)
			waiting := s.waitingTools
			s.mu.Unlock()
			if pendingCount > 0 || waiting {
				return false, nil
			}
			ev := StreamEvent{Type: "usage_final"}
			if u.TurnEnded.InputTokens != nil {
				ev.InputTokens = *u.TurnEnded.InputTokens
			}
			if u.TurnEnded.OutputTokens != nil {
				ev.OutputTokens = *u.TurnEnded.OutputTokens
			}
			if u.TurnEnded.CacheReadTokens != nil {
				ev.CacheReadTokens = *u.TurnEnded.CacheReadTokens
			}
			if u.TurnEnded.CacheWriteTokens != nil {
				ev.CacheWriteTokens = *u.TurnEnded.CacheWriteTokens
			}
			if u.TurnEnded.ReasoningTokens != nil {
				ev.ReasoningTokens = *u.TurnEnded.ReasoningTokens
			}
			s.mu.Lock()
			s.turnEnded = true
			// Announce the upcoming checkpoint store before the reply is
			// released to the client: a follow-up request that echoes this
			// transcript can arrive faster than the trailing checkpoint, and
			// the pending marker makes its lookup wait instead of miss.
			if !s.allowImages && conversationReuseEnabled() && s.accountKey != "" && !s.snapshotStored && s.pendingResolve == nil {
				fingerprint := conversationFingerprint(s.finalTranscriptLocked())
				s.pendingResolve = defaultConversationCache.BeginPending(s.accountKey, fingerprint)
			}
			s.mu.Unlock()
			s.emit(ev)
			s.markFinished()
			s.emit(StreamEvent{Type: "segment_end", Reason: "stop"})
			// The conversation checkpoint trails turn_ended; keep reading
			// briefly so it can be captured before the stream closes.
			go s.finishAfterCheckpoint()
		case *agentv1.InteractionUpdate_ToolCallCompleted:
			if gen := u.ToolCallCompleted.GetToolCall().GetGenerateImageToolCall(); gen != nil {
				log.Debugf("cursor genimage completed: result=%T err=%q", gen.GetResult().GetResult(), gen.GetResult().GetError().GetError())
			}
			s.handleToolCallCompleted(u.ToolCallCompleted)
		case *agentv1.InteractionUpdate_ToolCallStarted:
			if gen := u.ToolCallStarted.GetToolCall().GetGenerateImageToolCall(); gen != nil {
				log.Debugf("cursor genimage started: desc=%q path=%q refs=%q", gen.GetArgs().GetDescription(), gen.GetArgs().GetFilePath(), gen.GetArgs().GetReferenceImagePaths())
			}
		case *agentv1.InteractionUpdate_PartialToolCall:
			// Client-visible tool calls are driven by Exec mcp_args for declared tools.
		}
	case *agentv1.AgentServerMessage_KvServerMessage:
		return false, handleKV(s.stream, m.KvServerMessage, s.blobStore)
	case *agentv1.AgentServerMessage_ExecServerMessage:
		return s.handleExec(m.ExecServerMessage)
	case *agentv1.AgentServerMessage_InteractionQuery:
		return false, s.handleInteractionQuery(m.InteractionQuery)
	case *agentv1.AgentServerMessage_ConversationCheckpointUpdate:
		// The checkpoint is the server's own serialization of the
		// conversation so far. Replaying the latest one under the same
		// conversation id is what lets the next gateway turn continue this
		// conversation — and keep its provider prompt cache — instead of
		// starting over.
		if m.ConversationCheckpointUpdate != nil {
			cloned, okClone := proto.Clone(m.ConversationCheckpointUpdate).(*agentv1.ConversationStateStructure)
			if okClone {
				s.mu.Lock()
				s.checkpoint = cloned
				s.ckptCount++
				s.outputAfterCkpt = false
				if s.turnEnded {
					s.ckptAfterEnd = true
				}
				count := s.ckptCount
				afterEnd := s.ckptAfterEnd
				s.mu.Unlock()
				log.Debugf("cursor: conversation checkpoint #%d turns=%d rootMsgs=%d afterTurnEnd=%v conv=%s",
					count, len(cloned.GetTurns()), len(cloned.GetRootPromptMessagesJson()), afterEnd, s.ConversationID)
			}
		}
	case *agentv1.AgentServerMessage_ServerMetrics:
		// ignore
	}
	return false, nil
}

// handleToolCallCompleted surfaces server-side tool results the proxy cares
// about. Today that is only GenerateImage: Cursor's backend renders the image
// and either embeds it as base64 on the completed tool call or delivers the
// bytes beforehand through a write_args exec (stashed in writtenImages).
func (s *Session) handleToolCallCompleted(update *agentv1.ToolCallCompletedUpdate) {
	if update == nil {
		return
	}
	gen := update.GetToolCall().GetGenerateImageToolCall()
	if gen == nil {
		return
	}
	if !s.allowImages {
		// Cursor's server runs the tool without waiting for approval in some
		// flows, so the image still has to be dropped here. Report the
		// attempt: the model tends to announce an image it never delivered,
		// and silence would leave the caller waiting for it.
		s.emit(StreamEvent{Type: "image", Message: ImageGenerationRejectedReason})
		return
	}
	result := gen.GetResult()
	if result == nil {
		return
	}
	requestedPath := strings.TrimSpace(gen.GetArgs().GetFilePath())
	if genErr := result.GetError(); genErr != nil {
		// The PNG may already have arrived via write_args even when the
		// completion reports an error; prefer delivering the image.
		if raw := s.takeWrittenImage(requestedPath); len(raw) > 0 {
			s.emitRawImage(raw, requestedPath)
			return
		}
		s.emit(StreamEvent{Type: "image", Message: genErr.GetError()})
		return
	}
	success := result.GetSuccess()
	if success == nil {
		return
	}
	filePath := strings.TrimSpace(success.GetFilePath())
	data := strings.TrimSpace(success.GetImageData())
	if data == "" {
		raw := s.takeWrittenImage(filePath)
		if len(raw) == 0 {
			return
		}
		s.emitRawImage(raw, filePath)
		return
	}
	s.emit(StreamEvent{Type: "image", Image: &GeneratedImage{
		Base64:   data,
		MimeType: sniffImageMimeType(data),
		FilePath: success.GetFilePath(),
	}})
}

// emitRawImage emits an image event from raw (non-base64) bytes.
func (s *Session) emitRawImage(raw []byte, filePath string) {
	mime := http.DetectContentType(raw)
	if !strings.HasPrefix(mime, "image/") {
		mime = "image/png"
	}
	s.emit(StreamEvent{Type: "image", Image: &GeneratedImage{
		Base64:   base64.StdEncoding.EncodeToString(raw),
		MimeType: mime,
		FilePath: filePath,
	}})
}

// handleInteractionQuery auto-approves GenerateImage requests so headless
// clients never stall on the desktop approval dialog. Sessions that were not
// opened for image generation reject the request instead. Other query variants
// are not decoded by the MVP proto and stay unanswered.
func (s *Session) handleInteractionQuery(query *agentv1.InteractionQuery) error {
	client := buildGenerateImageDecision(query, s.allowImages)
	if client == nil {
		return nil
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	return s.stream.WriteEnvelope(payload, false)
}

// ImageGenerationRejectedReason is handed back to the Agent when a session
// that is not an image session asks to run GenerateImage.
const ImageGenerationRejectedReason = "Image generation is not available in this conversation. Use the dedicated image model instead."

// buildGenerateImageDecision returns the approval or rejection reply for a
// GenerateImage interaction query, or nil when the query is not an image
// generation request.
func buildGenerateImageDecision(query *agentv1.InteractionQuery, allow bool) *agentv1.AgentClientMessage {
	if query == nil {
		return nil
	}
	gen := query.GetGenerateImageRequestQuery()
	if gen == nil {
		return nil
	}
	decision := &agentv1.GenerateImageRequestResponse{}
	if allow {
		decision.Result = &agentv1.GenerateImageRequestResponse_Approved_{
			Approved: &agentv1.GenerateImageRequestResponse_Approved{
				Description: gen.GetArgs().GetDescription(),
			},
		}
	} else {
		decision.Result = &agentv1.GenerateImageRequestResponse_Rejected_{
			Rejected: &agentv1.GenerateImageRequestResponse_Rejected{
				Reason: ImageGenerationRejectedReason,
			},
		}
	}
	resp := &agentv1.InteractionResponse{
		Id: query.GetId(),
		Result: &agentv1.InteractionResponse_GenerateImageRequestResponse{
			GenerateImageRequestResponse: decision,
		},
	}
	return &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_InteractionResponse{InteractionResponse: resp},
	}
}

// sniffImageMimeType inspects base64 image bytes; Cursor does not send an
// explicit content type with GenerateImage results.
func sniffImageMimeType(b64 string) string {
	const fallback = "image/png"
	sample := b64
	if len(sample) > 512 {
		sample = sample[:512]
	}
	sample = sample[:len(sample)-len(sample)%4]
	decoded, err := base64.StdEncoding.DecodeString(sample)
	if err != nil || len(decoded) == 0 {
		return fallback
	}
	mime := http.DetectContentType(decoded)
	if !strings.HasPrefix(mime, "image/") {
		return fallback
	}
	return mime
}

func (s *Session) handleExec(req *agentv1.ExecServerMessage) (bool, error) {
	switch m := req.Message.(type) {
	case nil:
		if unknown := req.ProtoReflect().GetUnknown(); len(unknown) > 0 {
			log.Warnf("cursor exec: undecoded variant, unknown field bytes (first 64): %x", truncateBytes(unknown, 64))
		}
		return false, sendExecStreamClose(s.stream, req.Id)
	case *agentv1.ExecServerMessage_WriteArgs:
		return false, s.handleWriteArgs(req, m.WriteArgs)
	case *agentv1.ExecServerMessage_ReadArgs:
		return false, s.handleReadArgs(req, m.ReadArgs)
	case *agentv1.ExecServerMessage_RequestContextArgs:
		result := &agentv1.RequestContextResult{
			Result: &agentv1.RequestContextResult_Success{
				Success: &agentv1.RequestContextSuccess{
					RequestContext: s.requestContext(),
				},
			},
		}
		execClient := &agentv1.ExecClientMessage{
			Id:     req.Id,
			ExecId: req.ExecId,
			Message: &agentv1.ExecClientMessage_RequestContextResult{
				RequestContextResult: result,
			},
		}
		client := &agentv1.AgentClientMessage{
			Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: execClient},
		}
		payload, err := proto.Marshal(client)
		if err != nil {
			return false, err
		}
		if err = s.stream.WriteEnvelope(payload, false); err != nil {
			return false, err
		}
		return false, sendExecStreamClose(s.stream, req.Id)
	case *agentv1.ExecServerMessage_McpArgs:
		return s.handleMcpArgs(req, m.McpArgs)
	}
	if unknown := req.ProtoReflect().GetUnknown(); len(unknown) > 0 {
		log.Warnf("cursor exec: unhandled variant, unknown field bytes (first 64): %x", truncateBytes(unknown, 64))
	}
	return false, rejectUnsupportedExec(s.stream, req)
}

func truncateBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}

// handleWriteArgs services the write exec Cursor uses to deliver server-side
// tool output (GenerateImage sends the rendered PNG as file_bytes). The bytes
// are kept in memory for the tool-call completion and also written to disk
// when the target stays inside the headless workspace.
func (s *Session) handleWriteArgs(req *agentv1.ExecServerMessage, args *agentv1.WriteArgs) error {
	path := strings.TrimSpace(args.GetPath())
	data := args.GetFileBytes()
	if len(data) == 0 && args.GetFileText() != "" {
		data = []byte(args.GetFileText())
	}
	if path != "" && len(data) > 0 && strings.HasPrefix(http.DetectContentType(data), "image/") {
		s.mu.Lock()
		if s.writtenImages == nil {
			s.writtenImages = map[string][]byte{}
		}
		s.writtenImages[path] = data
		s.mu.Unlock()
	}
	writeHeadlessWorkspaceFile(path, data)

	result := &agentv1.WriteResult{
		Result: &agentv1.WriteResult_Success{
			Success: &agentv1.WriteSuccess{
				Path:     path,
				FileSize: int32(len(data)),
			},
		},
	}
	execClient := &agentv1.ExecClientMessage{
		Id:      req.Id,
		ExecId:  req.ExecId,
		Message: &agentv1.ExecClientMessage_WriteResult{WriteResult: result},
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: execClient},
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	if err = s.stream.WriteEnvelope(payload, false); err != nil {
		return err
	}
	return sendExecStreamClose(s.stream, req.Id)
}

// handleReadArgs services the read exec Cursor issues to pull a client-side
// file. Only the reference images seeded for this run are readable: the
// headless client has no workspace on disk, and answering arbitrary paths from
// the proxy host's filesystem would expose it to the upstream.
func (s *Session) handleReadArgs(req *agentv1.ExecServerMessage, args *agentv1.ReadArgs) error {
	path := strings.TrimSpace(args.GetPath())
	data, ok := s.referenceImage(path)
	log.Debugf("cursor read exec: path=%q served=%t bytes=%d", path, ok, len(data))
	var result *agentv1.ReadResult
	if !ok {
		result = &agentv1.ReadResult{
			Result: &agentv1.ReadResult_FileNotFound{
				FileNotFound: &agentv1.ReadFileNotFound{Path: path},
			},
		}
	} else {
		result = &agentv1.ReadResult{
			Result: &agentv1.ReadResult_Success{
				Success: &agentv1.ReadSuccess{
					Path:     path,
					FileSize: int64(len(data)),
					Output:   &agentv1.ReadSuccess_Data{Data: data},
				},
			},
		}
	}
	execClient := &agentv1.ExecClientMessage{
		Id:      req.Id,
		ExecId:  req.ExecId,
		Message: &agentv1.ExecClientMessage_ReadResult{ReadResult: result},
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: execClient},
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	if err = s.stream.WriteEnvelope(payload, false); err != nil {
		return err
	}
	return sendExecStreamClose(s.stream, req.Id)
}

// referenceImage looks up seeded reference bytes. Cursor sometimes normalises
// the advertised path, so an exact miss falls back to a basename match.
func (s *Session) referenceImage(path string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.referenceImages) == 0 {
		return nil, false
	}
	if data, ok := s.referenceImages[path]; ok {
		return data, true
	}
	base := filepath.Base(path)
	for p, data := range s.referenceImages {
		if filepath.Base(p) == base {
			return data, true
		}
	}
	return nil, false
}

// takeWrittenImage removes and returns stashed write_args image bytes. An
// empty path returns any stashed image (single-image runs do not always echo
// the exact path back on completion).
func (s *Session) takeWrittenImage(path string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.writtenImages) == 0 {
		return nil
	}
	if path != "" {
		if data, ok := s.writtenImages[path]; ok {
			delete(s.writtenImages, path)
			return data
		}
	}
	for p, data := range s.writtenImages {
		delete(s.writtenImages, p)
		return data
	}
	return nil
}

func (s *Session) handleMcpArgs(req *agentv1.ExecServerMessage, args *agentv1.McpArgs) (bool, error) {
	toolName := strings.TrimSpace(args.GetToolName())
	if toolName == "" {
		toolName = strings.TrimSpace(args.GetName())
	}
	provider := strings.TrimSpace(args.GetProviderIdentifier())
	if provider == "" {
		provider = strings.TrimSpace(args.GetServerIdentifier())
	}
	def := s.lookupTool(toolName, provider)
	if def == nil {
		result := &agentv1.McpResult{
			Result: &agentv1.McpResult_ToolNotFound{
				ToolNotFound: &agentv1.McpToolNotFound{
					Name:           toolName,
					AvailableTools: s.availableToolNames(),
				},
			},
		}
		return false, s.replyMcp(req, result)
	}
	callID := NormalizeToolCallID(args.GetToolCallId())
	if callID == "" {
		callID = uuid.NewString()
	}
	call := ToolCall{
		ID:        callID,
		Name:      def.Name,
		Arguments: decodeMcpArguments(args),
	}
	copyReq := proto.Clone(req).(*agentv1.ExecServerMessage)
	s.mu.Lock()
	s.pending[callID] = &pendingExec{request: copyReq, call: call}
	s.everOutput = true
	s.outputAfterCkpt = true
	s.segCalls = append(s.segCalls, call)
	s.mu.Unlock()
	if s.manager != nil {
		s.manager.BindPending(callID, s)
	}
	log.Debugf("cursor client tool call: session=%s tool=%s call_id=%s", s.ID, def.Name, callID)
	s.emit(StreamEvent{Type: "tool_call", ToolCall: &call})
	return true, nil
}

func (s *Session) replyMcp(req *agentv1.ExecServerMessage, result *agentv1.McpResult) error {
	execClient := &agentv1.ExecClientMessage{
		Id:      req.Id,
		ExecId:  req.ExecId,
		Message: &agentv1.ExecClientMessage_McpResult{McpResult: result},
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: execClient},
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	if err = s.stream.WriteEnvelope(payload, false); err != nil {
		return err
	}
	return sendExecStreamClose(s.stream, req.Id)
}

func (s *Session) lookupTool(name, provider string) *ToolDefinition {
	if name == "" {
		return nil
	}
	def := s.toolIndex[name]
	if def == nil {
		// Tools registered under a wire-namespaced name (claude upstream)
		// come back with that name; resolve them to the client's definition.
		if trimmed, ok := strings.CutPrefix(name, mcpToolWirePrefix); ok {
			def = s.toolIndex[trimmed]
		}
	}
	if def == nil {
		return nil
	}
	if provider == "" || provider == MCPProviderIdentifier {
		return def
	}
	return nil
}

// availableToolNames lists tool names the way the model knows them: it feeds
// the ToolNotFound reply, so it must match what was registered on the wire.
func (s *Session) availableToolNames() []string {
	names := make([]string, 0, len(s.tools))
	for _, t := range s.tools {
		if t.Name == "" {
			continue
		}
		name := t.Name
		if s.renameToolsOnWire {
			name = mcpToolWirePrefix + name
		}
		names = append(names, name)
	}
	return names
}

func (s *Session) requestContext() *agentv1.RequestContext {
	ctx := headlessRequestContext()
	if len(s.tools) > 0 {
		ctx.Tools = buildMcpToolDefinitions(s.tools, s.renameToolsOnWire)
	}
	return ctx
}

func rejectUnsupportedExec(stream *BidiStream, req *agentv1.ExecServerMessage) error {
	code := "exec_variant_unsupported"
	control := &agentv1.ExecClientControlMessage{
		Message: &agentv1.ExecClientControlMessage_Throw{
			Throw: &agentv1.ExecClientThrow{
				Id:        req.Id,
				Error:     "No handler for Cursor exec message",
				ErrorCode: &code,
			},
		},
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientControlMessage{ExecClientControlMessage: control},
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	if err = stream.WriteEnvelope(payload, false); err != nil {
		return err
	}
	return sendExecStreamClose(stream, req.Id)
}

// mcpToolWirePrefix namespaces client tool names on the wire when the model's
// upstream rejects duplicates of Cursor's built-in agent tool names (see
// claudeWireModel). The prefix is stripped again when the model calls the
// tool, so clients always see their own names.
const mcpToolWirePrefix = "mcp_"

func buildMcpToolDefinitions(tools []ToolDefinition, renameOnWire bool) []*agentv1.McpToolDefinition {
	out := make([]*agentv1.McpToolDefinition, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}
		if renameOnWire {
			name = mcpToolWirePrefix + name
		}
		schema := tool.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		value, err := toProtobufValue(schema)
		if err != nil {
			value, _ = toProtobufValue(map[string]any{"type": "object"})
		}
		out = append(out, &agentv1.McpToolDefinition{
			Name:               name,
			Description:        tool.Description,
			InputSchema:        value,
			ProviderIdentifier: MCPProviderIdentifier,
			ToolName:           name,
		})
	}
	return out
}

// RunChat performs a text/tool Cursor Agent segment and returns the collected result.
func RunChat(ctx context.Context, creds AccountCredentials, model string, messages []ChatMessage, tools []ToolDefinition, opts ...SessionOption) (*ChatResult, error) {
	results := extractToolResults(messages)
	if len(results) > 0 {
		session, err := DefaultSessionManager().ResolveForToolResults(results)
		if err == nil {
			err = session.SubmitToolResults(results)
		}
		switch {
		case err == nil:
			return session.CollectSegment(ctx)
		case errors.Is(err, ErrToolSessionLost):
			log.Debugf("cursor: replaying conversation after lost tool session: %v", err)
			messages = ReplayMessagesForLostSession(messages, results)
		default:
			return nil, err
		}
	}
	session, err := StartSession(ctx, creds, model, messages, tools, opts...)
	if err != nil {
		return nil, err
	}
	result, err := session.CollectSegment(ctx)
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	if result.FinishReason != "tool_calls" {
		_ = session.Close()
	}
	return result, nil
}

// ImageGenerationAgentModel is the Agent model used to drive the built-in
// GenerateImage tool when a caller only supplies an image prompt.
const ImageGenerationAgentModel = "default"

// ImageGenerationInstruction wraps a raw image prompt with an instruction
// that steers the Agent model into calling the GenerateImage tool.
func ImageGenerationInstruction(prompt string) string {
	return "Use your image generation tool to generate exactly one image that matches the description below, then stop. " +
		"Do not ask clarifying questions and do not write code.\n\nDescription: " + strings.TrimSpace(prompt)
}

// ImageEditInstruction steers the Agent model into an image-to-image call. The
// reference paths must be named explicitly: the model decides what to put in
// GenerateImageArgs.reference_image_paths, and the server only reads paths it
// was given there.
func ImageEditInstruction(prompt string, refs []ReferenceImage) string {
	paths := make([]string, 0, len(refs))
	for _, ref := range refs {
		if path := strings.TrimSpace(ref.Path); path != "" {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return ImageGenerationInstruction(prompt)
	}
	noun := "image"
	if len(paths) > 1 {
		noun = "images"
	}
	return "Use your image generation tool to edit the existing " + noun + " listed below and produce exactly one new image, then stop. " +
		"Pass every listed path as a reference image to the tool. " +
		"Do not ask clarifying questions and do not write code.\n\nReference " + noun + ":\n" +
		"- " + strings.Join(paths, "\n- ") +
		"\n\nRequested change: " + strings.TrimSpace(prompt)
}

// AttachedImageNote names the caller's inline images for a general chat turn.
// The Agent run request carries text only, so an attachment cannot ride along
// with the message: it is materialised at a workspace path whose bytes the read
// exec serves, and the model is told where to find it. Without the paths spelled
// out the model reports there is no image in the workspace and gives up.
func AttachedImageNote(refs []ReferenceImage) string {
	paths := make([]string, 0, len(refs))
	for _, ref := range refs {
		if path := strings.TrimSpace(ref.Path); path != "" {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return ""
	}
	noun := "image"
	if len(paths) > 1 {
		noun = "images"
	}
	return "\n\n[Attached " + noun + "]\nThe user attached the following " + noun +
		" to this message; they are saved in the workspace at:\n- " + strings.Join(paths, "\n- ") +
		"\nRead those paths when the request concerns them. Image generation is unavailable in this conversation, so answer with text."
}

// RunImageGeneration drives one Cursor Agent run whose sole purpose is to
// produce images via the built-in GenerateImage tool. Cursor's server renders
// the image after the client auto-approves the interaction query. When refs is
// non-empty the run is an image-to-image edit: the paths are advertised to the
// model and their bytes are served back over the read exec.
func RunImageGeneration(ctx context.Context, creds AccountCredentials, model string, prompt string, refs ...ReferenceImage) (*ChatResult, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, fmt.Errorf("cursor: image prompt is required")
	}
	if strings.TrimSpace(model) == "" {
		model = ImageGenerationAgentModel
	}
	instruction := ImageGenerationInstruction(prompt)
	if len(refs) > 0 {
		instruction = ImageEditInstruction(prompt, refs)
		paths := make([]string, 0, len(refs))
		for _, ref := range refs {
			paths = append(paths, ref.Path)
		}
		log.Debugf("cursor image edit: advertising %d reference image(s): %q", len(refs), paths)
	}
	result, err := RunChat(ctx, creds, model, []ChatMessage{{Role: "user", Content: instruction}}, nil, WithImageGeneration(), WithReferenceImages(refs))
	if err != nil {
		return nil, err
	}
	if len(result.Images) == 0 {
		if strings.TrimSpace(result.ImageError) != "" {
			return nil, fmt.Errorf("cursor: image generation failed: %s", result.ImageError)
		}
		detail := strings.TrimSpace(result.Text)
		if len(detail) > 300 {
			detail = detail[:300] + "…"
		}
		if detail == "" {
			detail = "the model did not invoke the image generation tool"
		}
		return nil, fmt.Errorf("cursor: no image returned: %s", detail)
	}
	return result, nil
}

func extractToolResults(messages []ChatMessage) []ToolResult {
	// Only trailing tool messages after the latest assistant tool_calls are
	// treated as a live round-trip resume; older history tool rows are ignored.
	start := -1
	for i := len(messages) - 1; i >= 0; i-- {
		switch messages[i].Role {
		case "tool":
			start = i
			continue
		case "assistant":
			if len(messages[i].ToolCalls) > 0 && start >= 0 {
				out := make([]ToolResult, 0, len(messages)-start)
				for _, msg := range messages[start:] {
					if msg.Role != "tool" || strings.TrimSpace(msg.ToolCallID) == "" {
						continue
					}
					out = append(out, ToolResult{
						ToolCallID: msg.ToolCallID,
						Name:       msg.Name,
						Content:    msg.Content,
					})
				}
				return out
			}
			return nil
		default:
			return nil
		}
	}
	return nil
}
