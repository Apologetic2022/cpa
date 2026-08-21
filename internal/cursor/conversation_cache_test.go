package cursor

import (
	"testing"

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
