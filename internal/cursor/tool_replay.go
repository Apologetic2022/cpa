package cursor

import (
	"fmt"
	"strings"
)

// replayToolResultLimit caps how much of each tool output is repeated in the
// synthesized turn. The full text still travels in the rebuilt history, so the
// copy only has to be long enough for the model to act on.
const replayToolResultLimit = 4000

// replayNoteMarker opens the synthetic user message a lost-session replay
// appends. It identifies that message later: the client never saw it, so the
// transcript mirror (and therefore the checkpoint fingerprint) must not
// contain it, and a checkpoint-fold resume must not duplicate the results it
// restates.
const replayNoteMarker = "The tool calls you requested have already run."

// ReplayMessagesForLostSession rewrites a tool-result resume request into a
// self-contained prompt.
//
// Resuming a tool round-trip normally writes the results into the live Agent
// run, but that run is gone after a gateway restart or an idle timeout. The
// request itself still carries the whole conversation, so a fresh run can pick
// the task back up: the trailing tool results are restated as a closing user
// turn, which is what buildRunRequest needs to treat everything before it as
// history.
func ReplayMessagesForLostSession(messages []ChatMessage, results []ToolResult) []ChatMessage {
	if len(results) == 0 {
		return messages
	}
	var b strings.Builder
	b.WriteString(replayNoteMarker + " Their results are below; continue the task with them and do not repeat the same calls.\n")
	for _, result := range results {
		name := strings.TrimSpace(result.Name)
		if name == "" {
			name = "tool"
		}
		b.WriteString(fmt.Sprintf("\n<tool_result name=%q>\n", name))
		b.WriteString(truncateToolResult(result.Content))
		b.WriteString("\n</tool_result>\n")
	}
	replayed := make([]ChatMessage, 0, len(messages)+1)
	replayed = append(replayed, messages...)
	return append(replayed, ChatMessage{Role: "user", Content: b.String()})
}

func truncateToolResult(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return "(empty result)"
	}
	if len(content) <= replayToolResultLimit {
		return content
	}
	return content[:replayToolResultLimit] + "\n… (truncated)"
}
