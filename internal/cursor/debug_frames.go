package cursor

import (
	"encoding/hex"
	"fmt"
	"os"
	"sync"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	"google.golang.org/protobuf/encoding/prototext"
)

var (
	debugFramesOnce    sync.Once
	debugFramesEnabled bool
)

// debugFrames reports whether raw Agent frame dumping is enabled via
// CURSOR_DEBUG_FRAMES. Used to diagnose upstream protocol drift.
func debugFrames() bool {
	debugFramesOnce.Do(func() {
		debugFramesEnabled = os.Getenv("CURSOR_DEBUG_FRAMES") != ""
	})
	return debugFramesEnabled
}

func debugDumpFrame(env Envelope, msg *agentv1.AgentServerMessage) {
	if !debugFrames() {
		return
	}
	text := "<unparsed>"
	if msg != nil {
		text = prototext.MarshalOptions{Multiline: false}.Format(msg)
	}
	fmt.Fprintf(os.Stderr, "[cursor-frame] flags=%d len=%d msg=%s hex=%s\n",
		env.Flags, len(env.Payload), text, hex.EncodeToString(env.Payload))
}
