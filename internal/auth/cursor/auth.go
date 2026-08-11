package cursor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	loginURL = "https://cursor.com/loginDeepControl"
	pollURL  = "https://api2.cursor.sh/auth/poll"
)

var refreshGroup singleflight.Group

// LoginParameters holds PKCE login state for Cursor deep-control login.
type LoginParameters struct {
	AttemptID string
	Verifier  string
	Challenge string
	UUID      string
	LoginURL  string
}

// AuthService provides Cursor login and token refresh helpers.
type AuthService struct {
	httpClient *http.Client
	mu         sync.Mutex
}

// NewAuthService constructs an AuthService with a timeout-bound HTTP client.
func NewAuthService() *AuthService {
	return &AuthService{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func b64URL(value []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(value), "=")
}

// GenerateLoginParameters creates a PKCE login challenge for Cursor.
func GenerateLoginParameters() (*LoginParameters, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("cursor: generate verifier: %w", err)
	}
	verifier := b64URL(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge := b64URL(sum[:])
	pollUUID := uuid.NewString()
	query := url.Values{
		"challenge":      {challenge},
		"uuid":           {pollUUID},
		"mode":           {"login"},
		"redirectTarget": {"cli"},
	}
	return &LoginParameters{
		AttemptID: uuid.NewString(),
		Verifier:  verifier,
		Challenge: challenge,
		UUID:      pollUUID,
		LoginURL:  loginURL + "?" + query.Encode(),
	}, nil
}

// PollLoginOnce checks whether the PKCE login completed.
func (s *AuthService) PollLoginOnce(ctx context.Context, pollUUID, verifier string) (accessToken, refreshToken string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return "", "", err
	}
	q := req.URL.Query()
	q.Set("uuid", pollUUID)
	q.Set("verifier", verifier)
	req.URL.RawQuery = q.Encode()

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("cursor poll login: close body error: %v", errClose)
		}
	}()
	if resp.StatusCode == http.StatusNotFound {
		return "", "", nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("cursor poll failed: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var payload struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	if err = json.Unmarshal(body, &payload); err != nil {
		return "", "", fmt.Errorf("cursor poll decode: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return "", "", fmt.Errorf("cursor poll response missing accessToken")
	}
	refresh := strings.TrimSpace(payload.RefreshToken)
	if refresh == "" {
		refresh = payload.AccessToken
	}
	return payload.AccessToken, refresh, nil
}

// WaitForLogin polls until tokens are available or the context is cancelled.
func (s *AuthService) WaitForLogin(ctx context.Context, params *LoginParameters, interval time.Duration) (accessToken, refreshToken string, err error) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		accessToken, refreshToken, err = s.PollLoginOnce(ctx, params.UUID, params.Verifier)
		if err != nil {
			return "", "", err
		}
		if accessToken != "" {
			return accessToken, refreshToken, nil
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-ticker.C:
		}
	}
}

// RefreshResult is the outcome of a Cursor token refresh.
type RefreshResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// RefreshToken refreshes Cursor credentials using desktop OAuth or CLI exchange.
func (s *AuthService) RefreshToken(ctx context.Context, refreshToken, authClientID, baseURL string) (*RefreshResult, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, fmt.Errorf("cursor: refresh token is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	authClientID = strings.TrimSpace(authClientID)
	key := baseURL + "|" + authClientID + "|" + refreshToken
	result, err, _ := refreshGroup.Do(key, func() (any, error) {
		return s.refreshOnce(context.WithoutCancel(ctx), refreshToken, authClientID, baseURL)
	})
	if err != nil {
		return nil, err
	}
	out, ok := result.(*RefreshResult)
	if !ok || out == nil {
		return nil, fmt.Errorf("cursor: refresh failed: invalid single-flight result")
	}
	return out, nil
}

func (s *AuthService) refreshOnce(ctx context.Context, refreshToken, authClientID, baseURL string) (*RefreshResult, error) {
	if authClientID != "" {
		payload := map[string]string{
			"grant_type":    "refresh_token",
			"client_id":     authClientID,
			"refresh_token": refreshToken,
		}
		body, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/oauth/token", strings.NewReader(string(body)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("cursor desktop refresh: %w", err)
		}
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Errorf("cursor desktop refresh: close body error: %v", errClose)
			}
		}()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("cursor desktop refresh failed: HTTP %d: %s", resp.StatusCode, string(raw))
		}
		var parsed struct {
			AccessToken  string `json:"access_token"`
			ShouldLogout bool   `json:"shouldLogout"`
		}
		if err = json.Unmarshal(raw, &parsed); err != nil {
			return nil, err
		}
		if parsed.ShouldLogout {
			return nil, fmt.Errorf("cursor service requires re-login")
		}
		if strings.TrimSpace(parsed.AccessToken) == "" {
			return nil, fmt.Errorf("cursor desktop refresh missing access_token")
		}
		return &RefreshResult{
			AccessToken:  parsed.AccessToken,
			RefreshToken: parsed.AccessToken,
			ExpiresAt:    TokenExpiry(parsed.AccessToken),
		}, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/auth/exchange_user_api_key", strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+refreshToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cursor exchange refresh: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("cursor exchange refresh: close body error: %v", errClose)
		}
	}()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cursor exchange refresh failed: HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var parsed struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	if err = json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	if strings.TrimSpace(parsed.AccessToken) == "" {
		return nil, fmt.Errorf("cursor exchange refresh missing accessToken")
	}
	nextRefresh := strings.TrimSpace(parsed.RefreshToken)
	if nextRefresh == "" {
		nextRefresh = refreshToken
	}
	return &RefreshResult{
		AccessToken:  parsed.AccessToken,
		RefreshToken: nextRefresh,
		ExpiresAt:    TokenExpiry(parsed.AccessToken),
	}, nil
}

// TokenExpiry extracts JWT exp with a 5-minute skew. Zero means unknown.
func TokenExpiry(accessToken string) time.Time {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		padded := parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4)
		payload, err = base64.URLEncoding.DecodeString(padded)
		if err != nil {
			return time.Time{}
		}
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if err = json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(claims.Exp), 0).UTC().Add(-time.Duration(refreshThresholdSeconds) * time.Second)
}

// TokenUserID extracts a short user identifier from the JWT subject.
func TokenUserID(accessToken string) string {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		padded := parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4)
		payload, err = base64.URLEncoding.DecodeString(padded)
		if err != nil {
			return ""
		}
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err = json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	sub := strings.TrimSpace(claims.Sub)
	if sub == "" {
		return ""
	}
	if idx := strings.LastIndex(sub, "|"); idx >= 0 {
		sub = strings.TrimSpace(sub[idx+1:])
	}
	return sub
}
