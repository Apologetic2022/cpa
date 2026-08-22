package cursor

import (
	"regexp"
	"strings"
)

// Cursor mints client tool call ids that embed a newline
// ("call-<uuid>-0\nfc_<uuid>_0"). Anthropic requires tool_use.id to match
// ^[a-zA-Z0-9_-]+$, so the Claude response translator rewrites the newline to
// "_" before the id reaches the client. The client then echoes the rewritten
// id back and the raw upstream id no longer matches anything the session
// manager knows about. Handing out an already-normalised id keeps the round
// trip stable across every wire protocol; replies to Cursor are addressed by
// the stored exec request, never by this id.
var toolCallIDUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// NormalizeToolCallID renders a tool call id in the form every supported
// client protocol accepts unchanged.
func NormalizeToolCallID(id string) string {
	return toolCallIDUnsafe.ReplaceAllString(strings.TrimSpace(id), "_")
}
