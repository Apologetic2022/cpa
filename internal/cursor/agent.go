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

// splitActiveUser separates the trailing user message that drives the next
// turn from the history preceding it. The history slice is exactly what a
// client echoes back before its *next* user message, which makes it the
// conversation-cache lookup key.
func splitActiveUser(messages []ChatMessage) ([]ChatMessage, *ChatMessage, error) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && strings.TrimSpace(messages[i].Content) != "" {
			return messages[:i], &messages[i], nil
		}
	}
	return nil, nil, fmt.Errorf("cursor: request has no user message")
}

// historyHasAssistant reports whether the history already contains a model
// reply, i.e. the request is a follow-up turn rather than the start of a new
// conversation.
func historyHasAssistant(messages []ChatMessage) bool {
	for i := range messages {
		if messages[i].Role == "assistant" {
			return true
		}
	}
	return false
}

// resolveRunModelDetails applies catalog overrides to a model selection and
// renders the ModelDetails proto the Agent run needs.
func resolveRunModelDetails(selection *ModelSelection) *agentv1.ModelDetails {
	publicID := selection.PublicID
	if publicID == "" {
		publicID = selection.ModelID
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
	return details
}

// buildResumeRunRequest continues a previously checkpointed conversation: the
// server restores the turns it emitted through conversation_checkpoint_update
// and only the new user message is appended. Keeping the conversation_id is
// what preserves Cursor's provider-side prompt cache across gateway turns.
func buildResumeRunRequest(model string, entry *convEntry, userText string, tools []ToolDefinition) (*agentv1.AgentClientMessage, map[string][]byte, string, error) {
	if entry == nil || entry.state == nil || strings.TrimSpace(entry.conversationID) == "" {
		return nil, nil, "", fmt.Errorf("cursor: no conversation checkpoint to resume")
	}
	selection := ResolveRequestedModel(model)
	details := resolveRunModelDetails(&selection)
	// The original run's blobs stay resolvable: the server may still KV-fetch
	// root prompt messages referenced by the checkpointed turns.
	blobStore := make(map[string][]byte, len(entry.blobs))
	for k, v := range entry.blobs {
		blobStore[k] = v
	}
	conversationID := entry.conversationID
	supportsImages := true
	run := &agentv1.AgentRunRequest{
		ConversationId:             &conversationID,
		ConversationState:          entry.state,
		ModelDetails:               details,
		RequestedModel:             toRequestedModelProto(selection),
		ClientSupportsInlineImages: &supportsImages,
		Action: &agentv1.ConversationAction{
			Action: &agentv1.ConversationAction_UserMessageAction{
				UserMessageAction: &agentv1.UserMessageAction{
					UserMessage: &agentv1.UserMessage{
						Text:      userText,
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

// replayToolsAsText reports whether a replayed history must fold its tool
// calls and tool results into plain text for this model. Cursor serves every
// claude model as a thinking variant, and its Anthropic upstream rejects a
// replayed history that carries structured tool-call/tool-result parts
// without their original signed thinking blocks: the run dies immediately
// with ERROR_PROVIDER_ERROR (provider 400, not retryable), so a claude
// conversation could never be rebuilt once its checkpoint or live session was
// gone. Text-folded history sidesteps the constraint; grok / composer / gpt
// accept the structured form and keep it.
func replayToolsAsText(wireModelID string) bool {
	return strings.Contains(strings.ToLower(wireModelID), "claude")
}

// foldToolCallText renders one historical tool call as plain text.
func foldToolCallText(tc *ToolCall) string {
	args := "{}"
	if len(tc.Arguments) > 0 {
		if b, err := json.Marshal(tc.Arguments); err == nil {
			args = string(b)
		}
	}
	return fmt.Sprintf("[called tool %s id=%s arguments=%s]", strings.TrimSpace(tc.Name), NormalizeToolCallID(tc.ID), args)
}

func buildRunRequest(model string, messages []ChatMessage, tools []ToolDefinition, allowImages bool) (*agentv1.AgentClientMessage, map[string][]byte, string, error) {
	selection := ResolveRequestedModel(model)
	blobStore := map[string][]byte{}
	systemPrompt := "You are a helpful assistant."
	if !allowImages {
		// Cursor's server runs GenerateImage without waiting for the client
		// in some flows, and its result is discarded on a conversation that
		// may not generate. Saying so up front is what keeps the model from
		// spending half a minute on an image nobody will receive and then
		// reporting it as delivered.
		systemPrompt += " You cannot generate images in this conversation: your image generation tool is unavailable. If the user asks for an image, say so plainly instead of calling the tool or claiming an image was produced."
	}
	systemBlob := storeBlob(blobStore, mustJSON(map[string]any{
		"role":    "system",
		"content": systemPrompt,
	}))

	history, activeUser, err := splitActiveUser(messages)
	if err != nil {
		return nil, nil, "", err
	}

	// Track tool names so tool-result parts can include toolName (cursor2api).
	toolNames := map[string]string{}
	foldTools := replayToolsAsText(selection.ModelID)
	rootIDs := [][]byte{systemBlob}
	for _, msg := range history {
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
			if foldTools {
				var b strings.Builder
				if strings.TrimSpace(msg.Content) != "" {
					b.WriteString(msg.Content)
				}
				for i := range msg.ToolCalls {
					tc := &msg.ToolCalls[i]
					if strings.TrimSpace(tc.ID) != "" && strings.TrimSpace(tc.Name) != "" {
						toolNames[tc.ID] = tc.Name
					}
					if b.Len() > 0 {
						b.WriteString("\n\n")
					}
					b.WriteString(foldToolCallText(tc))
				}
				rootIDs = append(rootIDs, storeBlob(blobStore, mustJSON(map[string]any{
					"role": "assistant",
					"content": []map[string]string{
						{"type": "text", "text": b.String()},
					},
				})))
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
			if foldTools {
				text := fmt.Sprintf("[tool result %s id=%s]\n%s", toolName, NormalizeToolCallID(msg.ToolCallID), msg.Content)
				rootIDs = append(rootIDs, storeBlob(blobStore, mustJSON(map[string]any{
					"role": "user",
					"content": []map[string]string{
						{"type": "text", "text": text},
					},
				})))
				continue
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
	details := resolveRunModelDetails(&selection)
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
	return filepath.Join(referenceImageDir(), fmt.Sprintf("reference-%d%s", index+1, imageExtension(mimeType)))
}

// headlessWorkspaceDirName is the folder the headless workspace and its Cursor
// project are always named after. Paths carrying this segment came from this
// client's own advertised environment and never exist on disk.
const headlessWorkspaceDirName = "cliproxy-cursor-workspace"

// IsHeadlessWorkspacePath reports whether a path points inside the workspace
// this client advertises to Cursor. Such paths are what server-side tools echo
// back (GenerateImage names its output there), and they never resolve on the
// relay host, so callers must not surface them as if they were fetchable.
func IsHeadlessWorkspacePath(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	cleaned := filepath.Clean(p)
	root, project := headlessWorkspaceRoot()
	for _, base := range []string{root, project} {
		if rel, err := filepath.Rel(base, cleaned); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	// The service account's home differs between deployments, so also accept
	// the workspace folder name appearing anywhere in the path.
	for _, segment := range strings.Split(filepath.ToSlash(cleaned), "/") {
		if segment == headlessWorkspaceDirName {
			return true
		}
	}
	return false
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
