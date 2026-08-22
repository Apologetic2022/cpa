package cursor

import (
	"encoding/hex"
	"testing"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

// Captured from production journal (2026-08-22): upstream packs the first
// text_delta ("X") and server_metrics into one AgentServerMessage frame.
// A plain Unmarshal keeps only server_metrics and drops the "X".
const packedFirstTokenFrameHex = "0a0d0a030a0158c801f2d0f1c1823442240900043928318ca540110000b0c744e75440190000ec302621384021006815fdf934a340"

func TestSplitAgentServerRecordsPreservesFirstToken(t *testing.T) {
	payload, err := hex.DecodeString(packedFirstTokenFrameHex)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	// Baseline: demonstrate the oneof overwrite the split must prevent.
	whole := &agentv1.AgentServerMessage{}
	if err := proto.Unmarshal(payload, whole); err != nil {
		t.Fatalf("unmarshal whole payload: %v", err)
	}
	if whole.GetInteractionUpdate() != nil {
		t.Fatalf("fixture no longer reproduces the oneof overwrite; got %v", whole)
	}

	chunks := splitAgentServerRecords(payload)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 records, got %d", len(chunks))
	}

	first := &agentv1.AgentServerMessage{}
	if err := proto.Unmarshal(chunks[0], first); err != nil {
		t.Fatalf("unmarshal first record: %v", err)
	}
	update := first.GetInteractionUpdate()
	if update == nil {
		t.Fatalf("first record is not an interaction_update: %v", first)
	}
	if got := update.GetTextDelta().GetText(); got != "X" {
		t.Fatalf("first text delta = %q, want %q", got, "X")
	}

	second := &agentv1.AgentServerMessage{}
	if err := proto.Unmarshal(chunks[1], second); err != nil {
		t.Fatalf("unmarshal second record: %v", err)
	}
	if second.GetServerMetrics() == nil {
		t.Fatalf("second record is not server_metrics: %v", second)
	}
}

func TestSplitAgentServerRecordsSingleRecord(t *testing.T) {
	msg := &agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_TextDelta{
					TextDelta: &agentv1.TextDeltaUpdate{Text: "hello"},
				},
			},
		},
	}
	payload, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	chunks := splitAgentServerRecords(payload)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 record, got %d", len(chunks))
	}
	round := &agentv1.AgentServerMessage{}
	if err := proto.Unmarshal(chunks[0], round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := round.GetInteractionUpdate().GetTextDelta().GetText(); got != "hello" {
		t.Fatalf("text = %q, want %q", got, "hello")
	}
}

func TestSplitAgentServerRecordsMalformed(t *testing.T) {
	if chunks := splitAgentServerRecords([]byte{0xff, 0xff, 0xff}); chunks != nil {
		t.Fatalf("expected nil for malformed payload, got %d chunks", len(chunks))
	}
}
