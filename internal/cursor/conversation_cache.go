package cursor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
)

// Conversation continuation cache.
//
// Cursor's backend keys its provider-side prompt cache to the upstream
// conversation: replaying the same history under a fresh conversation_id
// re-bills the whole prefix (observed cache_read=0 on every follow-up turn).
// The server streams conversation_checkpoint_update frames during a run; a
// follow-up turn that presents that checkpoint under the *same*
// conversation_id continues the conversation and keeps the provider cache
// warm.
//
// The cache maps (account, transcript fingerprint) -> latest checkpoint. The
// fingerprint is derived from the message history a client echoes back, so a
// follow-up request finds the entry by hashing everything before its new user
// message.

const (
	// convCacheTTL must outlive a human pausing mid-conversation: a checkpoint
	// that expires while the user is reading forces the next turn to replay
	// the whole history under a fresh upstream conversation, which re-bills
	// the entire prefix.
	convCacheTTL = 2 * time.Hour
	// convCacheMaxSize allows for the multiple keys one turn now stores (the
	// final transcript, the client's request prefix, and mid-turn tool-pause
	// boundaries).
	convCacheMaxSize = 2048

	// convPendingWait bounds how long a lookup waits for a checkpoint that is
	// still in flight. The server's final checkpoint trails turn_ended by up
	// to checkpointGraceWindow; an agent chaining requests can come back
	// faster than that, and missing the entry would replay the conversation
	// from scratch.
	convPendingWait = checkpointGraceWindow + time.Second
)

type convEntry struct {
	conversationID string
	state          *agentv1.ConversationStateStructure
	blobs          map[string][]byte
	model          string
	expiresAt      time.Time
}

// pendingMarker tracks one in-flight checkpoint store. Two paths race to
// release its waiters: the owning session's resolve func, and a takeover in
// BeginPending when a duplicate turn (same account + transcript) finishes
// while the first store is still trailing in. closed is guarded by the cache
// mutex so whichever path loses the race becomes a no-op instead of a double
// close — an unguarded double close panics and takes the whole gateway down,
// wiping every cached checkpoint with it.
type pendingMarker struct {
	ch     chan struct{}
	closed bool
}

// closeLocked releases waiters exactly once. The caller must hold the cache
// mutex.
func (m *pendingMarker) closeLocked() {
	if !m.closed {
		m.closed = true
		close(m.ch)
	}
}

type conversationCache struct {
	mu      sync.Mutex
	entries map[string]*convEntry
	// pending marks fingerprints whose checkpoint is still trailing in from
	// the upstream stream. Lookup blocks briefly on these instead of treating
	// the gap as a miss.
	pending map[string]*pendingMarker
}

var defaultConversationCache = &conversationCache{
	entries: map[string]*convEntry{},
	pending: map[string]*pendingMarker{},
}

// conversationReuseEnabled gates checkpoint continuation. On by default;
// CPA_CURSOR_CONV_REUSE=0 turns it off (kept for cache A/B testing on the
// gateway host).
func conversationReuseEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPA_CURSOR_CONV_REUSE"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

func convCacheKey(accountKey, fingerprint string) string {
	return accountKey + "\x00" + fingerprint
}

func (c *conversationCache) Lookup(accountKey, fingerprint string) (*convEntry, bool) {
	if accountKey == "" || fingerprint == "" {
		return nil, false
	}
	key := convCacheKey(accountKey, fingerprint)
	entry, ok, wait := c.lookupOnce(key)
	if ok {
		return entry, ok
	}
	if wait != nil {
		// The turn that produced this transcript has ended but its checkpoint
		// is still in flight; wait for the store instead of replaying the
		// whole conversation under a fresh upstream conversation id.
		select {
		case <-wait:
		case <-time.After(convPendingWait):
		}
		if entry, ok, _ = c.lookupOnce(key); ok {
			return entry, ok
		}
	}
	return c.loadPersisted(key)
}

// LookupNoWait returns a stored entry without blocking on an in-flight store.
// Fallback boundary probes use it: they hash several candidate prefixes, and
// only the exact turn-end transcript could ever be pending.
func (c *conversationCache) LookupNoWait(accountKey, fingerprint string) (*convEntry, bool) {
	if accountKey == "" || fingerprint == "" {
		return nil, false
	}
	key := convCacheKey(accountKey, fingerprint)
	if entry, ok, _ := c.lookupOnce(key); ok {
		return entry, ok
	}
	return c.loadPersisted(key)
}

// lookupOnce returns the live entry for key, or the pending-store channel a
// caller may wait on when the entry is not there yet.
func (c *conversationCache) lookupOnce(key string) (*convEntry, bool, chan struct{}) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if ok {
		if now.After(entry.expiresAt) {
			delete(c.entries, key)
			return nil, false, nil
		}
		// Refresh on hit: an active conversation keeps extending its own life.
		entry.expiresAt = now.Add(convCacheTTL)
		return entry, true, nil
	}
	if marker := c.pending[key]; marker != nil {
		return nil, false, marker.ch
	}
	return nil, false, nil
}

// BeginPending announces that the checkpoint for (accountKey, fingerprint) is
// about to be stored, so a concurrent lookup for the same transcript waits
// for it instead of missing. The returned resolve func must be called exactly
// when the store happened or was abandoned; it is safe to call more than once.
func (c *conversationCache) BeginPending(accountKey, fingerprint string) func() {
	if accountKey == "" || fingerprint == "" {
		return func() {}
	}
	key := convCacheKey(accountKey, fingerprint)
	marker := &pendingMarker{ch: make(chan struct{})}
	c.mu.Lock()
	// A stale marker for the same key (e.g. a retried or duplicate turn) is
	// released so its waiters re-check instead of hanging on an orphaned
	// channel. Its owner's resolve func may still fire later; closeLocked
	// keeps that from double-closing.
	if prev, ok := c.pending[key]; ok {
		prev.closeLocked()
	}
	c.pending[key] = marker
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		if c.pending[key] == marker {
			delete(c.pending, key)
		}
		marker.closeLocked()
		c.mu.Unlock()
	}
}

func (c *conversationCache) Store(accountKey, fingerprint string, entry *convEntry) {
	if accountKey == "" || fingerprint == "" || entry == nil || entry.state == nil {
		return
	}
	entry.expiresAt = time.Now().Add(convCacheTTL)
	key := convCacheKey(accountKey, fingerprint)
	c.mu.Lock()
	if len(c.entries) >= convCacheMaxSize {
		c.pruneLocked()
	}
	c.entries[key] = entry
	c.mu.Unlock()
	persistConvEntry(key, entry)
}

func (c *conversationCache) Invalidate(accountKey, fingerprint string) {
	if accountKey == "" || fingerprint == "" {
		return
	}
	key := convCacheKey(accountKey, fingerprint)
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
	removePersistedConvEntry(key)
}

// pruneLocked drops expired entries first; if the cache is still full it
// evicts the entries closest to expiry.
func (c *conversationCache) pruneLocked() {
	now := time.Now()
	for key, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
	for len(c.entries) >= convCacheMaxSize {
		var oldestKey string
		var oldest time.Time
		for key, entry := range c.entries {
			if oldestKey == "" || entry.expiresAt.Before(oldest) {
				oldestKey = key
				oldest = entry.expiresAt
			}
		}
		if oldestKey == "" {
			return
		}
		delete(c.entries, oldestKey)
	}
}

// Checkpoint persistence.
//
// The conversation cache is what keeps Cursor's provider-side prompt cache
// warm, and it used to live only in memory: every gateway restart (deploys
// most of all) re-billed the full history of every active conversation on its
// next turn. Entries are therefore mirrored to disk — one file per
// (account, fingerprint) — and loaded back lazily on a memory miss. The files
// contain conversation history blobs in the clear, like the request logs on
// the same host; CPA_CURSOR_CONV_PERSIST=0 disables the mirror.

// persistedConvEntry is the on-disk form of a convEntry. State holds the
// marshalled ConversationStateStructure proto.
type persistedConvEntry struct {
	ConversationID string            `json:"conversation_id"`
	Model          string            `json:"model"`
	ExpiresAtUnix  int64             `json:"expires_at_unix"`
	State          []byte            `json:"state"`
	Blobs          map[string][]byte `json:"blobs,omitempty"`
}

// convPersistEnabled gates the disk mirror; on by default.
func convPersistEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPA_CURSOR_CONV_PERSIST"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// convCacheDir resolves (and creates) the directory checkpoint files live in.
// Empty means persistence is unavailable.
func convCacheDir() string {
	if !convPersistEnabled() {
		return ""
	}
	dir := strings.TrimSpace(os.Getenv("CPA_CURSOR_CONV_CACHE_DIR"))
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil || strings.TrimSpace(base) == "" {
			base = os.TempDir()
		}
		dir = filepath.Join(base, "cliproxy", "cursor-conversations")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return dir
}

// convEntryPath names the file for one cache key. The key embeds the account
// and the transcript fingerprint, so its hash is collision-safe and reveals
// neither.
func convEntryPath(dir, key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".ckpt")
}

// persistConvEntry mirrors a stored checkpoint to disk. Best effort: a failed
// write only costs cache warmth after the next restart.
func persistConvEntry(key string, entry *convEntry) {
	dir := convCacheDir()
	if dir == "" || entry == nil || entry.state == nil {
		return
	}
	state, err := proto.Marshal(entry.state)
	if err != nil {
		return
	}
	payload, err := json.Marshal(persistedConvEntry{
		ConversationID: entry.conversationID,
		Model:          entry.model,
		ExpiresAtUnix:  entry.expiresAt.Unix(),
		State:          state,
		Blobs:          entry.blobs,
	})
	if err != nil {
		return
	}
	path := convEntryPath(dir, key)
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, payload, 0o600); err != nil {
		return
	}
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// removePersistedConvEntry drops the disk mirror of one cache key.
func removePersistedConvEntry(key string) {
	if dir := convCacheDir(); dir != "" {
		_ = os.Remove(convEntryPath(dir, key))
	}
}

// loadPersisted restores a checkpoint from disk after a memory miss (usually
// a restart) and re-inserts it into the in-memory cache. Expired files are
// removed on sight.
func (c *conversationCache) loadPersisted(key string) (*convEntry, bool) {
	dir := convCacheDir()
	if dir == "" {
		return nil, false
	}
	path := convEntryPath(dir, key)
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var stored persistedConvEntry
	if err = json.Unmarshal(payload, &stored); err != nil {
		_ = os.Remove(path)
		return nil, false
	}
	expiresAt := time.Unix(stored.ExpiresAtUnix, 0)
	if time.Now().After(expiresAt) || strings.TrimSpace(stored.ConversationID) == "" {
		_ = os.Remove(path)
		return nil, false
	}
	state := &agentv1.ConversationStateStructure{}
	if err = proto.Unmarshal(stored.State, state); err != nil {
		_ = os.Remove(path)
		return nil, false
	}
	entry := &convEntry{
		conversationID: stored.ConversationID,
		state:          state,
		blobs:          stored.Blobs,
		model:          stored.Model,
		expiresAt:      expiresAt,
	}
	c.mu.Lock()
	if existing, ok := c.entries[key]; ok {
		// A concurrent store won the race; prefer the fresher entry.
		c.mu.Unlock()
		return existing, true
	}
	if len(c.entries) >= convCacheMaxSize {
		c.pruneLocked()
	}
	c.entries[key] = entry
	c.mu.Unlock()
	log.Debugf("cursor: restored conversation checkpoint from disk conv=%s model=%s", entry.conversationID, entry.model)
	return entry, true
}

// sweepPersistedConvEntries deletes expired checkpoint files. Called once at
// startup so an abandoned gateway does not accumulate stale conversations.
func sweepPersistedConvEntries() {
	dir := convCacheDir()
	if dir == "" {
		return
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	now := time.Now()
	for _, file := range files {
		name := file.Name()
		if !strings.HasSuffix(name, ".ckpt") {
			continue
		}
		path := filepath.Join(dir, name)
		payload, errRead := os.ReadFile(path)
		if errRead != nil {
			continue
		}
		var stored persistedConvEntry
		if json.Unmarshal(payload, &stored) != nil || now.After(time.Unix(stored.ExpiresAtUnix, 0)) {
			_ = os.Remove(path)
		}
	}
}

func init() {
	go sweepPersistedConvEntries()
}

// accountKeyFromCredentials scopes conversation continuations to one Cursor
// account: a checkpoint replayed on a different account would neither restore
// nor cache anything.
func accountKeyFromCredentials(creds AccountCredentials) string {
	if v := strings.TrimSpace(creds.Email); v != "" {
		return "email:" + v
	}
	if v := strings.TrimSpace(creds.APIKey); v != "" {
		sum := sha256.Sum256([]byte(v))
		return "apikey:" + hex.EncodeToString(sum[:8])
	}
	if v := strings.TrimSpace(creds.MachineID); v != "" {
		return "machine:" + v
	}
	return ""
}

// conversationFingerprint hashes the parts of a transcript a client echoes
// back verbatim on its next turn: roles, text content, and tool call ids /
// names / canonical arguments. Reasoning text is excluded (clients do not
// resend it), and tool call ids are normalised the same way they were handed
// out.
func conversationFingerprint(messages []ChatMessage) string {
	h := sha256.New()
	for i := range messages {
		fingerprintMessage(h, &messages[i])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// conversationPrefixFingerprints returns the fingerprint of every
// message-boundary prefix: element i equals
// conversationFingerprint(messages[:i]). Computed in one pass (Sum does not
// disturb the running hash state), it lets a lookup probe all boundaries of a
// request for the longest prefix a stored checkpoint covers.
func conversationPrefixFingerprints(messages []ChatMessage) []string {
	h := sha256.New()
	out := make([]string, len(messages)+1)
	out[0] = hex.EncodeToString(h.Sum(nil))
	for i := range messages {
		fingerprintMessage(h, &messages[i])
		out[i+1] = hex.EncodeToString(h.Sum(nil))
	}
	return out
}

// fingerprintMessage feeds one transcript message into a fingerprint hash.
// Messages with no role or no echoable payload contribute nothing, so a
// client dropping them does not change the fingerprint.
func fingerprintMessage(h hash.Hash, msg *ChatMessage) {
	role := strings.TrimSpace(msg.Role)
	content := strings.TrimSpace(msg.Content)
	if role == "" {
		return
	}
	if content == "" && len(msg.ToolCalls) == 0 && strings.TrimSpace(msg.ToolCallID) == "" {
		return
	}
	h.Write([]byte{0x1e})
	h.Write([]byte(role))
	h.Write([]byte{0x1f})
	h.Write([]byte(content))
	h.Write([]byte{0x1f})
	if role == "tool" {
		h.Write([]byte(NormalizeToolCallID(msg.ToolCallID)))
		h.Write([]byte{0x1f})
	}
	for _, tc := range msg.ToolCalls {
		h.Write([]byte(NormalizeToolCallID(tc.ID)))
		h.Write([]byte{0x1f})
		h.Write([]byte(strings.TrimSpace(tc.Name)))
		h.Write([]byte{0x1f})
		if len(tc.Arguments) > 0 {
			if b, err := json.Marshal(tc.Arguments); err == nil {
				h.Write(b)
			}
		}
		h.Write([]byte{0x1f})
	}
}
