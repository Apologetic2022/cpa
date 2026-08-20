package cliproxy

import (
	"context"
	"strings"
	"time"

	cursorauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func (s *Service) fetchCursorModelsForAuth(ctx context.Context, auth *coreauth.Auth) []*registry.ModelInfo {
	if auth == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	creds := cursorlib.CredentialsFromMetadata(auth.Metadata)
	if strings.TrimSpace(creds.AccessToken) == "" {
		// Config cursor-api-key auths start without an access token; exchange
		// the API key so the catalog fetch (and later requests) can proceed.
		if strings.TrimSpace(creds.APIKey) == "" {
			return nil
		}
		refreshed, err := cursorauth.NewAuthService().RefreshToken(fetchCtx, creds.APIKey, "", creds.BaseURL)
		if err != nil {
			log.WithError(err).Warn("cursor: API key token exchange failed; using builtin model list")
			return nil
		}
		creds.AccessToken = refreshed.AccessToken
		if auth.Metadata == nil {
			auth.Metadata = map[string]any{}
		}
		auth.Metadata["access_token"] = refreshed.AccessToken
		if refreshed.RefreshToken != "" {
			auth.Metadata["refresh_token"] = refreshed.RefreshToken
		}
		if !refreshed.ExpiresAt.IsZero() {
			auth.Metadata["expired"] = refreshed.ExpiresAt.UTC().Format(time.RFC3339)
		}
	}

	models, err := cursorlib.FetchAvailableModels(fetchCtx, creds)
	if err != nil {
		log.WithError(err).Warn("cursor: AvailableModels fetch failed; using builtin model list")
		return nil
	}
	if len(models) == 0 {
		log.Warn("cursor: AvailableModels returned empty catalog; using builtin model list")
		return nil
	}
	log.Infof("cursor: AvailableModels loaded %d models for auth %s", len(models), auth.ID)
	return cursorlib.CatalogToModelInfos(models)
}
