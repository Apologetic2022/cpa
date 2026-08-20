package cursor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	cursorauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

// ChatMessage is a minimal OpenAI-style chat message.
type ChatMessage struct {
	Role       string
	Content    string
	Name       string
	ToolCallID string
	ToolCalls  []ToolCall
}

// AccountCredentials are the fields required to open an Agent run.
type AccountCredentials struct {
	AccessToken string
	// APIKey is a Cursor user API key (crsr_...) exchangeable for access tokens.
	APIKey        string
	RefreshToken  string
	AuthClientID  string
	BaseURL       string
	ClientVersion string
	MachineID     string
	MacMachineID  string
	SessionID     string
	ClientOS      string
	ClientArch    string
	Timezone      string
	Email         string
	GhostMode     string
	CookieJar     *CookieJar
}

// CredentialsFromMetadata extracts Cursor account fields from auth metadata.
func CredentialsFromMetadata(meta map[string]any) AccountCredentials {
	get := func(keys ...string) string {
		for _, key := range keys {
			if meta == nil {
				continue
			}
			if v, ok := meta[key]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		return ""
	}
	creds := AccountCredentials{
		AccessToken:   get("access_token"),
		APIKey:        get("api_key"),
		RefreshToken:  get("refresh_token"),
		AuthClientID:  get("auth_client_id"),
		BaseURL:       get("base_url"),
		ClientVersion: get("client_version"),
		MachineID:     get("machine_id"),
		MacMachineID:  get("mac_machine_id"),
		SessionID:     get("session_id"),
		ClientOS:      get("client_os"),
		ClientArch:    get("client_arch"),
		Timezone:      get("timezone"),
		Email:         get("email"),
		GhostMode:     get("ghost_mode"),
	}
	if creds.BaseURL == "" {
		creds.BaseURL = cursorauth.DefaultBaseURL
	}
	if creds.ClientVersion == "" {
		creds.ClientVersion = cursorauth.DefaultClientVersion
	}
	// API-key credentials refresh via /auth/exchange_user_api_key, which the
	// auth service selects only when no OAuth client ID is present.
	if creds.AuthClientID == "" && creds.APIKey == "" {
		creds.AuthClientID = cursorauth.DefaultAuthClientID
	}
	if creds.MachineID == "" {
		creds.MachineID = DesktopMachineID()
	}
	if creds.MacMachineID == "" {
		creds.MacMachineID = DesktopMacMachineID()
	}
	if creds.ClientOS == "" {
		creds.ClientOS = DesktopClientOS()
	}
	if creds.ClientArch == "" {
		creds.ClientArch = DesktopClientArch()
	}
	if creds.SessionID == "" {
		creds.SessionID = uuid.NewString()
	}
	if creds.GhostMode == "" {
		creds.GhostMode = "implicit-false"
	}
	jarKey := creds.Email
	if jarKey == "" {
		jarKey = creds.MachineID
	}
	creds.CookieJar = CookieJarForAccount(jarKey)
	return creds
}

func storeBlob(store map[string][]byte, data []byte) []byte {
	sum := sha256.Sum256(data)
	id := sum[:]
	store[hex.EncodeToString(id)] = data
	return id
}

func buildRunRequest(model string, messages []ChatMessage, tools []ToolDefinition) (*agentv1.AgentClientMessage, map[string][]byte, string, error) {
	selection := ResolveRequestedModel(model)
	model = selection.ModelID
	blobStore := map[string][]byte{}
	systemPrompt := "You are a helpful assistant."
	systemBlob := storeBlob(blobStore, mustJSON(map[string]any{
		"role":    "system",
		"content": systemPrompt,
	}))

	var activeUser *ChatMessage
	historyEnd := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && strings.TrimSpace(messages[i].Content) != "" {
			activeUser = &messages[i]
			historyEnd = i
			break
		}
	}
	if activeUser == nil {
		return nil, nil, "", fmt.Errorf("cursor: request has no user message")
	}

	// Track tool names so tool-result parts can include toolName (cursor2api).
	toolNames := map[string]string{}
	rootIDs := [][]byte{systemBlob}
	for _, msg := range messages[:historyEnd] {
		switch msg.Role {
		case "user":
			if strings.TrimSpace(msg.Content) == "" {
				continue
			}
			rootIDs = append(rootIDs, storeBlob(blobStore, mustJSON(map[string]any{
				"role": "user",
				"content": []map[string]string{
					{"type": "text", "text": msg.Content},
				},
			})))
		case "assistant":
			if strings.TrimSpace(msg.Content) == "" && len(msg.ToolCalls) == 0 {
				continue
			}
			content := make([]map[string]any, 0, 1+len(msg.ToolCalls))
			if strings.TrimSpace(msg.Content) != "" {
				content = append(content, map[string]any{
					"type": "text",
					"text": msg.Content,
				})
			}
			for _, tc := range msg.ToolCalls {
				if strings.TrimSpace(tc.ID) != "" && strings.TrimSpace(tc.Name) != "" {
					toolNames[tc.ID] = tc.Name
				}
				args := any(tc.Arguments)
				if args == nil {
					args = map[string]any{}
				}
				content = append(content, map[string]any{
					"type":       "tool-call",
					"toolCallId": tc.ID,
					"toolName":   tc.Name,
					"args":       args,
				})
			}
			if len(content) == 0 {
				continue
			}
			rootIDs = append(rootIDs, storeBlob(blobStore, mustJSON(map[string]any{
				"role":    "assistant",
				"content": content,
			})))
		case "tool":
			if strings.TrimSpace(msg.ToolCallID) == "" {
				continue
			}
			toolName := strings.TrimSpace(msg.Name)
			if toolName == "" {
				toolName = toolNames[msg.ToolCallID]
			}
			resultPart := map[string]any{
				"type":       "tool-result",
				"toolName":   toolName,
				"toolCallId": msg.ToolCallID,
				"result":     msg.Content,
				"toolKind":   "mcp",
			}
			rootIDs = append(rootIDs, storeBlob(blobStore, mustJSON(map[string]any{
				"role": "tool",
				"id":   msg.ToolCallID,
				"content": []map[string]any{
					resultPart,
				},
			})))
		case "system":
			if strings.TrimSpace(msg.Content) == "" {
				continue
			}
			rootIDs = append(rootIDs, storeBlob(blobStore, mustJSON(map[string]any{
				"role":    "system",
				"content": msg.Content,
			})))
		}
	}

	conversationID := uuid.NewString()
	// Desktop / cursor2api do not set exclude_workspace_context by default;
	// forcing it true is rejected for many accounts ("Workspace context
	// exclusion is not allowed…").
	supportsImages := true
	publicID := selection.PublicID
	if publicID == "" {
		publicID = model
	}
	wireID := selection.ModelID
	displayID := publicID
	displayName := publicID
	if entry, ok := catalogEntry(publicID); ok {
		if entry.DisplayModel != "" {
			displayID = entry.DisplayModel
		}
		if entry.DisplayName != "" {
			displayName = entry.DisplayName
		}
		selection.MaxMode = selection.MaxMode || entry.MaxMode
		if !selection.VariantStringRepr && len(selection.Parameters) == 0 && len(entry.Parameters) > 0 {
			selection.Parameters = append([]ModelParameter(nil), entry.Parameters...)
		}
		if selection.VariantStringRepr && strings.TrimSpace(entry.WireID) != "" {
			wireID = entry.WireID
			selection.ModelID = wireID
		}
	}
	details := &agentv1.ModelDetails{ModelId: wireID, DisplayModelId: displayID, DisplayName: displayName}
	if selection.MaxMode {
		maxMode := true
		details.MaxMode = &maxMode
	}
	run := &agentv1.AgentRunRequest{
		ConversationId:             &conversationID,
		ConversationState:          &agentv1.ConversationStateStructure{RootPromptMessagesJson: rootIDs},
		ModelDetails:               details,
		RequestedModel:             toRequestedModelProto(selection),
		ClientSupportsInlineImages: &supportsImages,
		Action: &agentv1.ConversationAction{
			Action: &agentv1.ConversationAction_UserMessageAction{
				UserMessageAction: &agentv1.UserMessageAction{
					UserMessage: &agentv1.UserMessage{
						Text:      activeUser.Content,
						MessageId: uuid.NewString(),
					},
				},
			},
		},
	}
	if defs := buildMcpToolDefinitions(tools); len(defs) > 0 {
		run.McpTools = &agentv1.McpTools{McpTools: defs}
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_RunRequest{RunRequest: run},
	}
	return client, blobStore, conversationID, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

func handleKV(stream *BidiStream, req *agentv1.KvServerMessage, blobStore map[string][]byte) error {
	resp := &agentv1.KvClientMessage{Id: req.Id}
	switch m := req.Message.(type) {
	case *agentv1.KvServerMessage_GetBlobArgs:
		key := hex.EncodeToString(m.GetBlobArgs.GetBlobId())
		if data, ok := blobStore[key]; ok {
			resp.Message = &agentv1.KvClientMessage_GetBlobResult{
				GetBlobResult: &agentv1.GetBlobResult{BlobData: data},
			}
		} else {
			resp.Message = &agentv1.KvClientMessage_GetBlobResult{GetBlobResult: &agentv1.GetBlobResult{}}
		}
	case *agentv1.KvServerMessage_SetBlobArgs:
		key := hex.EncodeToString(m.SetBlobArgs.GetBlobId())
		blobStore[key] = append([]byte(nil), m.SetBlobArgs.GetBlobData()...)
		resp.Message = &agentv1.KvClientMessage_SetBlobResult{SetBlobResult: &agentv1.SetBlobResult{}}
	default:
		return nil
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_KvClientMessage{KvClientMessage: resp},
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	return stream.WriteEnvelope(payload, false)
}

func sendExecStreamClose(stream *BidiStream, id uint32) error {
	control := &agentv1.ExecClientControlMessage{
		Message: &agentv1.ExecClientControlMessage_StreamClose{
			StreamClose: &agentv1.ExecClientStreamClose{Id: id},
		},
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientControlMessage{ExecClientControlMessage: control},
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	return stream.WriteEnvelope(payload, false)
}

// headlessWorkspaceEnv returns a minimal environment describing a real
// workspace/project directory. Server-side tools such as GenerateImage refuse
// to run without one ("needs a workspace/project folder"), so a headless
// client must still advertise concrete paths even though it never writes the
// files itself (the image is returned inline as base64).
func headlessWorkspaceEnv() *agentv1.RequestContextEnv {
	root, project := headlessWorkspaceRoot()
	artifacts := filepath.Join(project, "assets")
	transcripts := filepath.Join(project, "transcripts")
	terminals := filepath.Join(project, "terminals")
	tz := "UTC"
	return &agentv1.RequestContextEnv{
		OsVersion:              DesktopClientOS(),
		WorkspacePaths:         []string{root},
		Shell:                  "/bin/bash",
		TerminalsFolder:        terminals,
		TimeZone:               tz,
		ProjectFolder:          project,
		AgentTranscriptsFolder: transcripts,
		ArtifactsFolder:        &artifacts,
	}
}

// headlessWorkspaceRoot creates and returns a stable per-process workspace root
// plus the ~/.cursor/projects/<slug> project folder Cursor expects. The Agent
// treats these as an open folder so server-side tools (GenerateImage) have a
// valid destination; the image itself is still returned inline as base64.
func headlessWorkspaceRoot() (root, project string) {
	base := os.TempDir()
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		base = home
	}
	root = filepath.Join(base, "cliproxy-cursor-workspace")
	project = filepath.Join(base, ".cursor", "projects", "cliproxy-cursor-workspace")
	for _, dir := range []string{
		root,
		filepath.Join(root, "assets"),
		project,
		filepath.Join(project, "assets"),
		filepath.Join(project, "transcripts"),
		filepath.Join(project, "terminals"),
	} {
		_ = os.MkdirAll(dir, 0o755)
	}
	return root, project
}

// referenceImageDir is the folder inside the headless project that reference
// images are advertised under. Cursor only ever sees these paths through
// GenerateImageArgs.reference_image_paths and reads them back over the read
// exec, so the directory does not need to exist on disk.
func referenceImageDir() string {
	_, project := headlessWorkspaceRoot()
	return filepath.Join(project, "assets", "references")
}

// ReferenceImagePath builds the workspace path advertised for the index-th
// reference image of a run. The extension is derived from the MIME type so the
// server can infer the format from the path alone.
func ReferenceImagePath(index int, mimeType string) string {
	ext := ".png"
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg", "image/jpg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	case "image/gif":
		ext = ".gif"
	}
	return filepath.Join(referenceImageDir(), fmt.Sprintf("reference-%d%s", index+1, ext))
}

// writeHeadlessWorkspaceFile persists server-delivered file bytes (e.g. the
// GenerateImage PNG) when the target lies inside the headless workspace or
// project folder. Failures are ignored: the bytes are returned inline anyway.
func writeHeadlessWorkspaceFile(path string, data []byte) {
	if strings.TrimSpace(path) == "" || len(data) == 0 {
		return
	}
	root, project := headlessWorkspaceRoot()
	cleaned := filepath.Clean(path)
	inside := func(base string) bool {
		rel, err := filepath.Rel(base, cleaned)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if !inside(root) && !inside(project) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(cleaned), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(cleaned, data, 0o644)
}

func headlessRequestContext() *agentv1.RequestContext {
	trueVal := true
	falseVal := false
	ctx := &agentv1.RequestContext{
		Env:                         headlessWorkspaceEnv(),
		EnvInfoComplete:             &trueVal,
		RulesInfoComplete:           &trueVal,
		RepositoryInfoComplete:      &trueVal,
		GitRepoInfoComplete:         &trueVal,
		GitStatusInfoComplete:       &trueVal,
		CustomSubagentsInfoComplete: &trueVal,
		AgentSkillsInfoComplete:     &trueVal,
		McpInfoComplete:             &trueVal,
		McpFileSystemInfoComplete:   &trueVal,
		WebFetchEnabled:             &falseVal,
		WebSearchEnabled:            &falseVal,
		SupportsMcpAuth:             &falseVal,
		ReadLintsEnabled:            &falseVal,
	}
	return ctx
}
