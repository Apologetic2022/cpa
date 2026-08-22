package cursor

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
	cursorauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
)

var (
	processClientKey     string
	processClientKeyOnce sync.Once
	processSessions      sync.Map
)

// ClientProfile describes the desktop IDE client fingerprint for Cursor RPCs.
type ClientProfile struct {
	Version       string
	ClientType    string
	ClientLayout  string
	GhostMode     string
	MachineID     string
	MacMachineID  string
	Timezone      string
	ClientOS      string
	ClientArch    string
	SessionID     string
	IdentityScope string
	ClientKey     string
	BaseURL       string
	CookieJar     *CookieJar
}

// ProfileFromCredentials builds a ClientProfile from account credentials.
func ProfileFromCredentials(creds AccountCredentials) ClientProfile {
	ghost := strings.TrimSpace(creds.GhostMode)
	if ghost == "" {
		ghost = "implicit-false"
	}
	mac := strings.TrimSpace(creds.MacMachineID)
	if mac == "" {
		mac = DesktopMacMachineID()
	}
	machineID := strings.TrimSpace(creds.MachineID)
	if machineID == "" {
		machineID = DesktopMachineID()
	}
	baseURL := strings.TrimSpace(creds.BaseURL)
	if baseURL == "" {
		baseURL = cursorauth.DefaultBaseURL
	}
	jar := creds.CookieJar
	if jar == nil {
		jarKey := creds.Email
		if jarKey == "" {
			jarKey = machineID
		}
		jar = CookieJarForAccount(jarKey)
	}
	return ClientProfile{
		Version:       firstNonEmpty(creds.ClientVersion, cursorauth.DefaultClientVersion),
		ClientType:    "ide",
		ClientLayout:  "editor",
		GhostMode:     ghost,
		MachineID:     machineID,
		MacMachineID:  mac,
		Timezone:      creds.Timezone,
		ClientOS:      firstNonEmpty(creds.ClientOS, DesktopClientOS()),
		ClientArch:    firstNonEmpty(creds.ClientArch, DesktopClientArch()),
		SessionID:     creds.SessionID,
		IdentityScope: machineID,
		BaseURL:       baseURL,
		CookieJar:     jar,
	}
}

func desktopClientKey() string {
	processClientKeyOnce.Do(func() {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			processClientKey = strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
			return
		}
		processClientKey = hex.EncodeToString(buf)
	})
	return processClientKey
}

func (p ClientProfile) sessionID() string {
	if strings.TrimSpace(p.SessionID) != "" {
		return p.SessionID
	}
	key := p.IdentityScope
	if key == "" {
		key = p.MachineID
	}
	if key == "" {
		key = "cursor-anonymous-machine"
	}
	if v, ok := processSessions.Load(key); ok {
		return v.(string)
	}
	id := uuid.NewString()
	actual, _ := processSessions.LoadOrStore(key, id)
	return actual.(string)
}

// Headers builds the desktop Cursor request headers for one RPC.
func (p ClientProfile) Headers(accessToken, requestID, backupRequestID string) (map[string]string, error) {
	if p.ClientType != "" && p.ClientType != "ide" {
		return nil, fmt.Errorf("cursor: only desktop IDE client profile is supported")
	}
	if strings.TrimSpace(p.MachineID) == "" {
		return nil, fmt.Errorf("cursor: machine_id is required")
	}
	if requestID == "" {
		requestID = uuid.NewString()
	}
	if backupRequestID == "" {
		backupRequestID = uuid.NewString()
	}
	clientKey := p.ClientKey
	if clientKey == "" {
		clientKey = desktopClientKey()
	}
	cookie := transportCursorCookie(accessToken)
	if p.CookieJar != nil {
		if jarCookie := p.CookieJar.Header(p.BaseURL); jarCookie != "" {
			cookie = jarCookie
		}
	}
	headers := map[string]string{
		"authorization":               "Bearer " + accessToken,
		"cookie":                      cookie,
		"traceparent":                 cursorTraceparent(),
		"x-client-key":                clientKey,
		"x-cursor-streaming":          "true",
		"x-ghost-mode":                firstNonEmpty(p.GhostMode, "implicit-false"),
		"x-cursor-client-version":     p.Version,
		"x-cursor-client-type":        "ide",
		"x-cursor-client-layout":      firstNonEmpty(p.ClientLayout, "editor"),
		"x-request-id":                requestID,
		"x-cursor-client-os":          p.ClientOS,
		"x-cursor-client-arch":        p.ClientArch,
		"x-cursor-client-device-type": "desktop",
		"x-new-onboarding-completed":  "false",
		"x-session-id":                p.sessionID(),
		"x-cursor-checksum":           Checksum(p.MachineID, p.MacMachineID, 0),
		"x-amzn-trace-id":             "Root=" + backupRequestID,
		"content-type":                "application/connect+proto",
		"connect-protocol-version":    "1",
		"user-agent":                  "connect-es/1.6.1",
		"connect-accept-encoding":     "gzip, br",
	}
	if tz := strings.TrimSpace(p.Timezone); tz != "" {
		headers["x-cursor-timezone"] = tz
	}
	return headers, nil
}

func accessTokenPrefix(token string) string {
	if len(token) >= 15 {
		return token[:15]
	}
	return token
}

func cursorTraceparent() string {
	traceID := make([]byte, 16)
	spanID := make([]byte, 8)
	_, _ = rand.Read(traceID)
	_, _ = rand.Read(spanID)
	// Ensure non-zero IDs.
	if isAllZero(traceID) {
		traceID[15] = 1
	}
	if isAllZero(spanID) {
		spanID[7] = 1
	}
	return fmt.Sprintf("00-%s-%s-00", hex.EncodeToString(traceID), hex.EncodeToString(spanID))
}

func isAllZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
