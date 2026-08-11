package cliproxy

import (
	"context"
	"strings"
	"time"

	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func (s *Service) fetchCursorModelsForAuth(ctx context.Context, auth *coreauth.Auth) []*registry.ModelInfo {
	if auth == nil {
		return nil
	}
	creds := cursorlib.CredentialsFromMetadata(auth.Metadata)
	if strings.TrimSpace(creds.AccessToken) == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

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
