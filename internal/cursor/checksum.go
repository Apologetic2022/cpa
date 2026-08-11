package cursor

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Checksum builds the x-cursor-checksum header using Cursor's JS-style packing.
func Checksum(machineID, macMachineID string, nowMS int64) string {
	if nowMS <= 0 {
		nowMS = time.Now().UnixMilli()
	}
	value := nowMS / 1_000_000
	// Intentionally mirrors JS signed 32-bit shift packing used by Cursor desktop.
	data := []byte{
		byte((value >> 8) & 0xFF),
		byte(value & 0xFF),
		byte((value >> 24) & 0xFF),
		byte((value >> 16) & 0xFF),
		byte((value >> 8) & 0xFF),
		byte(value & 0xFF),
	}
	rolling := 165
	for i, old := range data {
		next := ((int(old) ^ rolling) + (i & 0xFF)) & 0xFF
		data[i] = byte(next)
		rolling = next
	}
	encoded := strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
	if strings.TrimSpace(macMachineID) == "" {
		return encoded + machineID
	}
	return encoded + machineID + "/" + macMachineID
}

// StableMachineID hashes a seed into a desktop-style machine identity.
func StableMachineID(seed string) string {
	seed = strings.TrimSpace(strings.ToLower(seed))
	if seed == "" {
		seed = uuid.NewString()
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// DesktopMachineID returns a stable host machine identity.
func DesktopMachineID() string {
	seed := fmt.Sprintf("%s:%s", runtime.GOOS, hostname())
	if runtime.GOOS == "windows" {
		if guid := windowsMachineGUID(); guid != "" {
			seed = guid
		}
	}
	return StableMachineID(seed)
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "unknown"
	}
	return name
}

// DesktopClientOS maps GOOS to Cursor desktop OS labels.
func DesktopClientOS() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	default:
		return "linux"
	}
}

// DesktopClientArch maps GOARCH to Cursor desktop arch labels.
func DesktopClientArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "ia32"
	default:
		return runtime.GOARCH
	}
}
