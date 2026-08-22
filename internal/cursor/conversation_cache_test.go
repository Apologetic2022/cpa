package cursor

import (
	"strings"
	"testing"
	"time"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
)

func TestConversationFingerprintMatchesClientEcho(t *testing.T) {
	// What the gateway saw while running the turn.
	stored := []ChatMessage{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "list the files"},
		{Role: "assistant", Content: "Listing now.", ToolCalls: []ToolCall{
			{ID: "call-abc\nfc_1", Name: "ls", Arguments: map[string]any{"path": "/tmp", "depth": float64(2)}},
		}},
		{Role: "tool", Name: "ls", ToolCallID: "call-abc_fc_1", Content: "a.txt\nb.txt"},
		{Role: "assistant", Content: "Two files: a.txt and b.txt."},
	}
	// What the client echoes back on its next request: normalised tool call
	// ids and arguments that round-tripped through JSON.
	echoed := []ChatMessage{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "list the files"},
		{Role: "assistant", Content: "Listing now.", ToolCalls: []ToolCall{
			{ID: "call-abc_fc_1", Name: "ls", Arguments: map[string]any{"depth": float64(2), "path": "/tmp"}},
		}},
		{Role: "tool", ToolCallID: "call-abc_fc_1", Content: "a.txt\nb.txt"},
		{Role: "assistant", Content: "Two files: a.txt and b.txt."},
	}
	if conversationFingerprint(stored) != conversationFingerprint(echoed) {
		t.Fatalf("fingerprint of stored transcript does not match the client echo")
	}

	changed := append(append([]ChatMessage(nil), echoed...), ChatMessage{Role: "user", Content: "next"})
	if conversationFingerprint(echoed) == conversationFingerprint(changed) {
		t.Fatalf("fingerprint must change when the transcript grows")
	}
}

func TestLookupTurnBoundaryResumeFoldsToolTail(t *testing.T) {
	t.Setenv("CPA_CURSOR_CONV_CACHE_DIR", t.TempDir())
	// Checkpoint stored at the end of turn 1.
	stored := []ChatMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
	}
	fp := conversationFingerprint(stored)
	state := &agentv1.ConversationStateStructure{Turns: [][]byte{[]byte("turn")}}
	defaultConversationCache.Store("acct-boundary", fp, &convEntry{conversationID: "conv-b", state: state, model: "claude-sonnet-5"})
	defer defaultConversationCache.Invalidate("acct-boundary", fp)

	// Turn 2 called a tool, the live session was lost, and the continuation
	// request now ends with the tool result plus the synthetic replay note.
	continuation := append(append([]ChatMessage(nil), stored...),
		ChatMessage{Role: "user", Content: "second question"},
		ChatMessage{Role: "assistant", Content: "checking", ToolCalls: []ToolCall{
			{ID: "c1", Name: "Shell", Arguments: map[string]any{"cmd": "ls"}},
		}},
		ChatMessage{Role: "tool", Name: "Shell", ToolCallID: "c1", Content: "a.txt full output"},
		ChatMessage{Role: "user", Content: replayNoteMarker + " Their results are below.\n<tool_result>a.txt</tool_result>"},
	)
	entry, gotFp, folded, ok := lookupTurnBoundaryResume("acct-boundary", "claude-sonnet-5", continuation)
	if !ok || entry == nil || entry.conversationID != "conv-b" || gotFp != fp {
		t.Fatalf("expected boundary resume hit, got ok=%v entry=%+v", ok, entry)
	}
	for _, want := range []string{"second question", "Shell", "a.txt full output", "already run"} {
		if !strings.Contains(folded, want) {
			t.Fatalf("folded tail must carry %q, got %q", want, folded)
		}
	}
	if strings.Contains(folded, replayNoteMarker) {
		t.Fatalf("folded tail must skip the synthetic replay note, got %q", folded)
	}

	// A checkpoint pinned to another model cannot be continued.
	if _, _, _, ok = lookupTurnBoundaryResume("acct-boundary", "grok-4.6", continuation); ok {
		t.Fatalf("boundary resume must refuse a model mismatch")
	}
}

func TestEchoTranscriptStripsReplayNote(t *testing.T) {
	messages := []ChatMessage{
		{Role: "user", Content: "question"},
		{Role: "assistant", Content: "checking", ToolCalls: []ToolCall{{ID: "c1", Name: "Shell"}}},
		{Role: "tool", ToolCallID: "c1", Content: "out"},
		{Role: "user", Content: replayNoteMarker + " Their results are below."},
	}
	mirror := echoTranscript(messages)
	if len(mirror) != 3 || mirror[len(mirror)-1].Role != "tool" {
		t.Fatalf("synthetic replay note must not enter the transcript mirror, got %+v", mirror)
	}
	// A genuine trailing user message stays.
	plain := []ChatMessage{{Role: "user", Content: "hello"}}
	if got := echoTranscript(plain); len(got) != 1 {
		t.Fatalf("real user messages must stay, got %+v", got)
	}
}

func TestConversationCachePersistsAcrossRestart(t *testing.T) {
	t.Setenv("CPA_CURSOR_CONV_CACHE_DIR", t.TempDir())
	state := &agentv1.ConversationStateStructure{Turns: [][]byte{[]byte("turn-a"), []byte("turn-b")}}
	before := &conversationCache{entries: map[string]*convEntry{}, pending: map[string]*pendingMarker{}}
	before.Store("acct", "fp-persist", &convEntry{
		conversationID: "conv-persist",
		state:          state,
		blobs:          map[string][]byte{"blob1": []byte("payload")},
		model:          "claude-sonnet-5",
	})

	// A fresh cache simulates the gateway restarting.
	after := &conversationCache{entries: map[string]*convEntry{}, pending: map[string]*pendingMarker{}}
	entry, ok := after.Lookup("acct", "fp-persist")
	if !ok || entry == nil {
		t.Fatalf("expected the checkpoint to survive a restart")
	}
	if entry.conversationID != "conv-persist" || entry.model != "claude-sonnet-5" {
		t.Fatalf("restored entry mismatch: %+v", entry)
	}
	if len(entry.state.GetTurns()) != 2 || string(entry.blobs["blob1"]) != "payload" {
		t.Fatalf("restored state/blobs mismatch: %+v", entry)
	}

	// Invalidation removes the disk mirror too.
	after.Invalidate("acct", "fp-persist")
	again := &conversationCache{entries: map[string]*convEntry{}, pending: map[string]*pendingMarker{}}
	if _, ok = again.Lookup("acct", "fp-persist"); ok {
		t.Fatalf("invalidated checkpoint must not resurrect from disk")
	}
}

func TestConversationCacheRoundTrip(t *testing.T) {
	cache := &conversationCache{entries: map[string]*convEntry{}}
	state := &agentv1.ConversationStateStructure{Turns: [][]byte{[]byte("turn")}}
	cache.Store("acct", "fp", &convEntry{conversationID: "conv-1", state: state, model: "grok-4.6"})

	entry, ok := cache.Lookup("acct", "fp")
	if !ok || entry.conversationID != "conv-1" {
		t.Fatalf("expected stored entry, got ok=%v entry=%+v", ok, entry)
	}
	if _, ok = cache.Lookup("other", "fp"); ok {
		t.Fatalf("entries must be scoped to the account")
	}
	cache.Invalidate("acct", "fp")
	if _, ok = cache.Lookup("acct", "fp"); ok {
		t.Fatalf("expected entry to be invalidated")
	}
}

func TestLookupWaitsForPendingStore(t *testing.T) {
	cache := &conversationCache{entries: map[string]*convEntry{}, pending: map[string]*pendingMarker{}}
	state := &agentv1.ConversationStateStructure{Turns: [][]byte{[]byte("turn")}}

	resolve := cache.BeginPending("acct", "fp")
	done := make(chan struct{})
	go func() {
		// The store trails the lookup, as when the follow-up request beats
		// the upstream checkpoint frame.
		time.Sleep(50 * time.Millisecond)
		cache.Store("acct", "fp", &convEntry{conversationID: "conv-race", state: state, model: "grok-4.6"})
		resolve()
		close(done)
	}()

	entry, ok := cache.Lookup("acct", "fp")
	if !ok || entry.conversationID != "conv-race" {
		t.Fatalf("lookup should wait out the pending store, got ok=%v entry=%+v", ok, entry)
	}
	<-done

	// An abandoned store must release waiters promptly and report a miss.
	resolve = cache.BeginPending("acct", "fp2")
	start := time.Now()
	go func() {
		time.Sleep(20 * time.Millisecond)
		resolve()
	}()
	if _, ok = cache.Lookup("acct", "fp2"); ok {
		t.Fatalf("abandoned pending store must miss")
	}
	if waited := time.Since(start); waited >= convPendingWait {
		t.Fatalf("resolved pending marker should not block the full wait window (waited %s)", waited)
	}

	// No pending marker: a plain miss returns immediately.
	start = time.Now()
	if _, ok = cache.Lookup("acct", "fp3"); ok {
		t.Fatalf("unexpected hit")
	}
	if waited := time.Since(start); waited > 500*time.Millisecond {
		t.Fatalf("plain miss must not wait (waited %s)", waited)
	}
}

func TestTrailingUserRunMatchesStoredFingerprint(t *testing.T) {
	// The transcript stored at turn end closes with the assistant reply.
	stored := []ChatMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "<todo_list>old</todo_list>"},
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
	}
	// The follow-up prepends reminders and a todo list to the actual
	// question as separate user messages (Cursor CLI style).
	followUp := append(append([]ChatMessage(nil), stored...),
		ChatMessage{Role: "user", Content: "<system_reminder>mode changed</system_reminder>"},
		ChatMessage{Role: "user", Content: "<todo_list>new</todo_list>"},
		ChatMessage{Role: "user", Content: "second question"},
	)
	prefix, turn := splitTrailingUserRun(followUp)
	if len(turn) != 3 {
		t.Fatalf("expected the 3 trailing user messages to form the new turn, got %d", len(turn))
	}
	if conversationFingerprint(prefix) != conversationFingerprint(stored) {
		t.Fatalf("lookup prefix must reproduce the stored fingerprint")
	}
	joined := joinedUserText(turn)
	for _, want := range []string{"mode changed", "<todo_list>new</todo_list>", "second question"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("joined turn text must carry %q, got %q", want, joined)
		}
	}

	// A single trailing user message behaves exactly like the old split.
	single := append(append([]ChatMessage(nil), stored...),
		ChatMessage{Role: "user", Content: "second question"})
	prefix, turn = splitTrailingUserRun(single)
	if len(turn) != 1 || conversationFingerprint(prefix) != conversationFingerprint(stored) {
		t.Fatalf("single-user turn must keep matching, turn=%d", len(turn))
	}

	// A tool-result continuation is not a user run and must not match.
	toolCont := append(append([]ChatMessage(nil), stored...),
		ChatMessage{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "Shell"}}},
		ChatMessage{Role: "tool", ToolCallID: "c1", Content: "out"},
	)
	prefix, turn = splitTrailingUserRun(toolCont)
	if len(turn) != 0 || len(prefix) != len(toolCont) {
		t.Fatalf("trailing tool messages must stay in the prefix")
	}
}

func TestBeginPendingTakeoverDoesNotDoubleClose(t *testing.T) {
	// Two turns with the same (account, transcript) can overlap: the second
	// BeginPending takes over the pending slot and releases the first
	// marker's waiters. When the first turn's resolve fires afterwards it
	// used to close the already-closed channel and panic, crashing the
	// gateway and wiping the whole conversation cache (seen in production).
	cache := &conversationCache{entries: map[string]*convEntry{}, pending: map[string]*pendingMarker{}}

	resolveA := cache.BeginPending("acct", "fp")
	resolveB := cache.BeginPending("acct", "fp")

	resolveA() // must not panic even though B's takeover already closed A
	resolveB()
	resolveA() // resolve funcs stay idempotent
	resolveB()

	if len(cache.pending) != 0 {
		t.Fatalf("pending map must be empty after all resolves, got %d entries", len(cache.pending))
	}

	// The slot stays usable afterwards.
	resolve := cache.BeginPending("acct", "fp")
	state := &agentv1.ConversationStateStructure{Turns: [][]byte{[]byte("turn")}}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cache.Store("acct", "fp", &convEntry{conversationID: "conv-after", state: state, model: "grok-4.6"})
		resolve()
	}()
	if entry, ok := cache.Lookup("acct", "fp"); !ok || entry.conversationID != "conv-after" {
		t.Fatalf("lookup after takeover must still work, got ok=%v entry=%+v", ok, entry)
	}
}

func TestFinalTranscriptMatchesFlushedFingerprint(t *testing.T) {
	session := &Session{
		transcript: []ChatMessage{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hello"},
		},
	}
	session.segText.WriteString("partial reply")
	session.segCalls = []ToolCall{{ID: "call-1", Name: "ls", Arguments: map[string]any{"path": "/"}}}

	early := conversationFingerprint(session.finalTranscriptLocked())
	session.flushAssistantSegmentLocked()
	flushed := conversationFingerprint(session.transcript)
	if early != flushed {
		t.Fatalf("turn_ended fingerprint must match the flushed transcript fingerprint")
	}
	if session.segText.Len() != 0 || session.segCalls != nil {
		t.Fatalf("flush must consume the segment buffers")
	}
}

func TestSessionTranscriptSnapshot(t *testing.T) {
	t.Setenv("CPA_CURSOR_CONV_REUSE", "1")
	cacheBefore := len(defaultConversationCache.entries)

	session := &Session{
		ID:             "sess",
		ConversationID: "conv-snap",
		Model:          "grok-4.6",
		accountKey:     "acct-snap",
		transcript: []ChatMessage{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hello"},
		},
		checkpoint:   &agentv1.ConversationStateStructure{Turns: [][]byte{[]byte("turn")}},
		ckptAfterEnd: true,
		blobStore:    map[string][]byte{"k": []byte("v")},
	}
	session.segText.WriteString("hi there")
	session.storeConversationSnapshot()

	if len(defaultConversationCache.entries) != cacheBefore+1 {
		t.Fatalf("expected snapshot to be stored")
	}
	fingerprint := conversationFingerprint([]ChatMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	})
	entry, ok := defaultConversationCache.Lookup("acct-snap", fingerprint)
	if !ok {
		t.Fatalf("snapshot not found under the echoed-transcript fingerprint")
	}
	if entry.conversationID != "conv-snap" || len(entry.state.GetTurns()) != 1 {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	// A second call must be a no-op (already stored).
	session.storeConversationSnapshot()
	if len(defaultConversationCache.entries) != cacheBefore+1 {
		t.Fatalf("snapshot must only be stored once")
	}
	defaultConversationCache.Invalidate("acct-snap", fingerprint)
}

func TestSessionSnapshotCheckpointCompleteness(t *testing.T) {
	t.Setenv("CPA_CURSOR_CONV_REUSE", "1")

	newSession := func(id string) *Session {
		s := &Session{
			ID:             id,
			ConversationID: "conv-" + id,
			Model:          "grok-4.6",
			accountKey:     "acct-" + id,
			transcript:     []ChatMessage{{Role: "user", Content: "hi " + id}},
			checkpoint:     &agentv1.ConversationStateStructure{Turns: [][]byte{[]byte("turn")}},
		}
		s.segText.WriteString("reply " + id)
		return s
	}
	fingerprintFor := func(id string) string {
		return conversationFingerprint([]ChatMessage{
			{Role: "user", Content: "hi " + id},
			{Role: "assistant", Content: "reply " + id},
		})
	}

	// The final checkpoint landed before turn_ended but nothing streamed after
	// it: it reflects the whole reply and must be stored.
	pre := newSession("pre")
	pre.ckptAfterEnd = false
	pre.outputAfterCkpt = false
	pre.storeConversationSnapshot()
	if _, ok := defaultConversationCache.Lookup("acct-pre", fingerprintFor("pre")); !ok {
		t.Fatalf("pre-turn_ended checkpoint with no trailing output must be stored")
	}
	defaultConversationCache.Invalidate("acct-pre", fingerprintFor("pre"))

	// Output streamed after the last checkpoint and no post-end checkpoint
	// arrived: the checkpoint may be missing that output, so skip the store.
	stale := newSession("stale")
	stale.ckptAfterEnd = false
	stale.outputAfterCkpt = true
	stale.storeConversationSnapshot()
	if _, ok := defaultConversationCache.Lookup("acct-stale", fingerprintFor("stale")); ok {
		t.Fatalf("a checkpoint with output after it must not be stored")
	}

	// A post-turn_ended checkpoint always wins, even if output preceded it.
	post := newSession("post")
	post.ckptAfterEnd = true
	post.outputAfterCkpt = true
	post.storeConversationSnapshot()
	if _, ok := defaultConversationCache.Lookup("acct-post", fingerprintFor("post")); !ok {
		t.Fatalf("post-turn_ended checkpoint must be stored")
	}
	defaultConversationCache.Invalidate("acct-post", fingerprintFor("post"))
}
