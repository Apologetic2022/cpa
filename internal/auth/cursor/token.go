package cursor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
)

const (
	ProviderType            = "cursor"
	DefaultBaseURL          = "https://api2.cursor.sh"
	DefaultClientVersion    = "3.7.12"
	DefaultAuthClientID     = "KbZUR41cY7W6zRSdpSUJ7I7mLYBKOCmB"
	AgentRunPath            = "/agent.v1.AgentService/Run"
	refreshThresholdSeconds = 300
)

// TokenStorage stores Cursor OAuth credentials for multi-account pooling.
type TokenStorage struct {
	AccessToken   string         `json:"access_token"`
	RefreshToken  string         `json:"refresh_token"`
	TokenType     string         `json:"token_type"`
	Expired       string         `json:"expired,omitempty"`
	Email         string         `json:"email,omitempty"`
	Type          string         `json:"type"`
	BaseURL       string         `json:"base_url,omitempty"`
	ClientVersion string         `json:"client_version,omitempty"`
	AuthClientID  string         `json:"auth_client_id,omitempty"`
	MachineID     string         `json:"machine_id,omitempty"`
	MacMachineID  string         `json:"mac_machine_id,omitempty"`
	SessionID     string         `json:"session_id,omitempty"`
	ClientOS      string         `json:"client_os,omitempty"`
	ClientArch    string         `json:"client_arch,omitempty"`
	Timezone      string         `json:"timezone,omitempty"`
	Metadata      map[string]any `json:"-"`
}

// SetMetadata injects flattened metadata before saving.
func (ts *TokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile serializes the token storage to a JSON auth file.
func (ts *TokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = ProviderType
	if ts.TokenType == "" {
		ts.TokenType = "Bearer"
	}
	if ts.BaseURL == "" {
		ts.BaseURL = DefaultBaseURL
	}
	if ts.ClientVersion == "" {
		ts.ClientVersion = DefaultClientVersion
	}
	if ts.AuthClientID == "" {
		ts.AuthClientID = DefaultAuthClientID
	}

	if err := os.MkdirAll(filepath.Dir(authFilePath), 0700); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	f, err := os.Create(authFilePath)
	if err != nil {
		return fmt.Errorf("failed to create token file: %w", err)
	}
	defer func() {
		_ = f.Close()
	}()

	data, errMerge := misc.MergeMetadata(ts, ts.Metadata)
	if errMerge != nil {
		return fmt.Errorf("failed to merge metadata: %w", errMerge)
	}

	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(data); err != nil {
		return fmt.Errorf("failed to write token to file: %w", err)
	}
	return nil
}

// IsExpired reports whether the access token should be treated as expired.
func (ts *TokenStorage) IsExpired() bool {
	if ts.Expired == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, ts.Expired)
	if err != nil {
		return true
	}
	return time.Now().Add(time.Duration(refreshThresholdSeconds) * time.Second).After(t)
}

// NeedsRefresh reports whether a refresh should be attempted.
func (ts *TokenStorage) NeedsRefresh() bool {
	if ts.RefreshToken == "" {
		return false
	}
	return ts.IsExpired()
}
