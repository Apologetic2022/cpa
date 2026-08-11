package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var cursorRefreshLead = 5 * time.Minute

// CursorAuthenticator implements PKCE login for Cursor desktop accounts.
type CursorAuthenticator struct{}

// NewCursorAuthenticator constructs a Cursor authenticator.
func NewCursorAuthenticator() Authenticator {
	return &CursorAuthenticator{}
}

// Provider returns the provider key.
func (CursorAuthenticator) Provider() string { return cursor.ProviderType }

// RefreshLead returns how early tokens should be refreshed.
func (CursorAuthenticator) RefreshLead() *time.Duration { return &cursorRefreshLead }

// Login starts Cursor deep-control PKCE login and stores the resulting tokens.
func (a CursorAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	params, err := cursor.GenerateLoginParameters()
	if err != nil {
		return nil, err
	}
	fmt.Printf("\nTo authenticate Cursor, open:\n%s\n\n", params.LoginURL)
	if !opts.NoBrowser && browser.IsAvailable() {
		if errOpen := browser.OpenURL(params.LoginURL); errOpen != nil {
			log.Warnf("Failed to open browser automatically: %v", errOpen)
		} else {
			fmt.Println("Browser opened automatically.")
		}
	}
	fmt.Println("Waiting for Cursor authorization...")

	svc := cursor.NewAuthService()
	access, refresh, err := svc.WaitForLogin(ctx, params, time.Second)
	if err != nil {
		return nil, fmt.Errorf("cursor: %w", err)
	}

	machineID := cursorlib.DesktopMachineID()
	sessionID := time.Now().UTC().Format("20060102150405")
	subject := cursor.TokenUserID(access)
	label := "Cursor User"
	if subject != "" {
		label = "Cursor-" + subject
		if len(label) > 24 {
			label = label[:24]
		}
	}
	expired := cursor.TokenExpiry(access)
	expiredStr := ""
	if !expired.IsZero() {
		expiredStr = expired.UTC().Format(time.RFC3339)
	}

	storage := &cursor.TokenStorage{
		AccessToken:   access,
		RefreshToken:  refresh,
		TokenType:     "Bearer",
		Expired:       expiredStr,
		Type:          cursor.ProviderType,
		BaseURL:       cursor.DefaultBaseURL,
		ClientVersion: cursor.DefaultClientVersion,
		AuthClientID:  cursor.DefaultAuthClientID,
		MachineID:     machineID,
		SessionID:     sessionID,
		ClientOS:      cursorlib.DesktopClientOS(),
		ClientArch:    cursorlib.DesktopClientArch(),
	}

	metadata := map[string]any{
		"type":           cursor.ProviderType,
		"access_token":   access,
		"refresh_token":  refresh,
		"token_type":     "Bearer",
		"base_url":       cursor.DefaultBaseURL,
		"client_version": cursor.DefaultClientVersion,
		"auth_client_id": cursor.DefaultAuthClientID,
		"machine_id":     machineID,
		"session_id":     sessionID,
		"client_os":      cursorlib.DesktopClientOS(),
		"client_arch":    cursorlib.DesktopClientArch(),
		"timestamp":      time.Now().UnixMilli(),
	}
	if expiredStr != "" {
		metadata["expired"] = expiredStr
	}
	if subject != "" {
		metadata["email"] = subject
		storage.Email = subject
	}

	fileName := fmt.Sprintf("cursor-%d.json", time.Now().UnixMilli())
	if subject != "" {
		safe := strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
				return r
			}
			return '-'
		}, subject)
		// Lowercase keeps Windows auth IDs stable (file store lowercases on load).
		fileName = fmt.Sprintf("cursor-%s.json", strings.ToLower(safe))
	}
	fmt.Println("\nCursor authentication successful!")

	return &coreauth.Auth{
		ID:       fileName,
		Provider: a.Provider(),
		FileName: fileName,
		Label:    label,
		Storage:  storage,
		Metadata: metadata,
	}, nil
}
