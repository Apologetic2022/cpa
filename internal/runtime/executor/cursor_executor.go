package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	cursorauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// cursorImagesSourceFormat marks requests arriving from the OpenAI
	// /v1/images endpoints (same convention as the XAI executor).
	cursorImagesSourceFormat = "openai-image"
	// cursorImageModelID is the routed model id for Cursor image generation.
	cursorImageModelID = registry.CursorImageModelID
)

// CursorExecutor executes chat completions through Cursor's Agent Connect protocol.
type CursorExecutor struct {
	cfg *config.Config
	svc *cursorauth.AuthService
}

// NewCursorExecutor creates a Cursor executor.
func NewCursorExecutor(cfg *config.Config) *CursorExecutor {
	return &CursorExecutor{cfg: cfg, svc: cursorauth.NewAuthService()}
}

// Identifier returns the executor identifier.
func (e *CursorExecutor) Identifier() string { return "cursor" }

// Execute performs a non-streaming Cursor Agent chat completion.
func (e *CursorExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.SourceFormat.String() == cursorImagesSourceFormat {
		return e.executeImages(ctx, auth, req, opts)
	}
	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), false)
	_ = originalPayloadSource

	// Keep hyphen variant suffixes (-thinking/-xhigh/…) for Cursor resolution.
	// Public wire model id is the base name; parameters ride on RequestedModel.
	upstreamModel := strings.TrimPrefix(baseModel, "cursor-")
	if upstreamModel == "" {
		upstreamModel = "default"
	}
	imageChat := strings.EqualFold(baseModel, cursorImageModelID)
	if imageChat {
		upstreamModel = cursorlib.ImageGenerationAgentModel
	}
	resolved := cursorlib.ResolveRequestedModel(upstreamModel)
	body, _ = sjson.SetBytes(body, "model", resolved.ModelID)

	messages, err := extractChatMessages(body)
	if err != nil {
		return resp, err
	}
	tools := extractTools(body)
	var sessionOpts []cursorlib.SessionOption
	if imageChat {
		var refs []cursorlib.ReferenceImage
		if refs, err = extractChatImageInputs(body); err != nil {
			return resp, err
		}
		if messages, err = rewriteImageChatMessages(messages, refs); err != nil {
			return resp, err
		}
		tools = nil
		if len(refs) > 0 {
			sessionOpts = append(sessionOpts, cursorlib.WithReferenceImages(refs))
		}
	}

	creds, err := e.ensureCredentials(ctx, auth)
	if err != nil {
		return resp, err
	}

	result, err := cursorlib.RunChat(ctx, creds, upstreamModel, messages, tools, sessionOpts...)
	if err != nil {
		return resp, err
	}

	outPayload := buildOpenAIChatCompletion(req.Model, result)
	reporter.Publish(ctx, usage.Detail{
		InputTokens:     result.InputTokens,
		OutputTokens:    result.OutputTokens,
		CachedTokens:    result.CacheReadTokens,
		CacheReadTokens: result.CacheReadTokens,
		ReasoningTokens: result.ReasoningTokens,
		TotalTokens:     result.InputTokens + result.OutputTokens,
	})

	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, outPayload, &param)
	resp = cliproxyexecutor.Response{Payload: out}
	return resp, nil
}

// ExecuteStream streams OpenAI-compatible SSE chunks from Cursor Agent events.
func (e *CursorExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.SourceFormat.String() == cursorImagesSourceFormat {
		return e.executeImagesStream(ctx, auth, req, opts)
	}
	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	to := sdktranslator.FromString("openai")
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), true)

	upstreamModel := strings.TrimPrefix(baseModel, "cursor-")
	if upstreamModel == "" {
		upstreamModel = "default"
	}
	imageChat := strings.EqualFold(baseModel, cursorImageModelID)
	if imageChat {
		upstreamModel = cursorlib.ImageGenerationAgentModel
	}
	resolved := cursorlib.ResolveRequestedModel(upstreamModel)
	body, _ = sjson.SetBytes(body, "model", resolved.ModelID)

	messages, err := extractChatMessages(body)
	if err != nil {
		return nil, err
	}
	tools := extractTools(body)
	var sessionOpts []cursorlib.SessionOption
	if imageChat {
		var refs []cursorlib.ReferenceImage
		if refs, err = extractChatImageInputs(body); err != nil {
			return nil, err
		}
		if messages, err = rewriteImageChatMessages(messages, refs); err != nil {
			return nil, err
		}
		tools = nil
		if len(refs) > 0 {
			sessionOpts = append(sessionOpts, cursorlib.WithReferenceImages(refs))
		}
	}

	creds, err := e.ensureCredentials(ctx, auth)
	if err != nil {
		return nil, err
	}

	session, err := openCursorSession(ctx, creds, upstreamModel, messages, tools, sessionOpts...)
	if err != nil {
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		completionID := "chatcmpl-" + uuid.NewString()
		created := time.Now().Unix()
		var param any
		toolIndex := 0
		imageIndex := 0
		finishReason := "stop"
		var usageFinal cursorlib.StreamEvent

		emitLine := func(line []byte) bool {
			chunks := sdktranslator.TranslateStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, line, &param)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					_ = session.Close()
					return false
				}
			}
			return true
		}

		roleChunk, _ := json.Marshal(map[string]any{
			"id":      completionID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   req.Model,
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil},
			},
		})
		if !emitLine([]byte("data: " + string(roleChunk))) {
			return
		}

		errIter := session.IterSegment(ctx, func(ev cursorlib.StreamEvent) error {
			var delta map[string]any
			switch ev.Type {
			case "text_delta":
				delta = map[string]any{"content": ev.Text}
			case "thinking_delta":
				delta = map[string]any{"reasoning_content": ev.Text}
			case "image":
				if ev.Image == nil {
					return nil
				}
				delta = map[string]any{
					"images": []map[string]any{
						{
							"index":     imageIndex,
							"type":      "image_url",
							"image_url": map[string]any{"url": ev.Image.DataURL()},
						},
					},
				}
				imageIndex++
			case "tool_call":
				if ev.ToolCall == nil {
					return nil
				}
				args, _ := json.Marshal(ev.ToolCall.Arguments)
				if args == nil {
					args = []byte("{}")
				}
				delta = map[string]any{
					"tool_calls": []map[string]any{
						{
							"index": toolIndex,
							"id":    ev.ToolCall.ID,
							"type":  "function",
							"function": map[string]any{
								"name":      ev.ToolCall.Name,
								"arguments": string(args),
							},
						},
					},
				}
				toolIndex++
			case "usage_final":
				usageFinal = ev
				reporter.Publish(ctx, usage.Detail{
					InputTokens:     ev.InputTokens,
					OutputTokens:    ev.OutputTokens,
					CachedTokens:    ev.CacheReadTokens,
					CacheReadTokens: ev.CacheReadTokens,
					ReasoningTokens: ev.ReasoningTokens,
					TotalTokens:     ev.InputTokens + ev.OutputTokens,
				})
				return nil
			case "error":
				return fmt.Errorf("%s", ev.Message)
			case "segment_end":
				if ev.Reason != "" {
					finishReason = ev.Reason
				}
				return nil
			default:
				return nil
			}
			if delta == nil {
				return nil
			}
			chunk, _ := json.Marshal(map[string]any{
				"id":      completionID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   req.Model,
				"choices": []map[string]any{
					{"index": 0, "delta": delta, "finish_reason": nil},
				},
			})
			if !emitLine([]byte("data: " + string(chunk))) {
				return context.Canceled
			}
			return nil
		})
		if errIter != nil {
			reporter.PublishFailure(ctx, errIter)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errIter}:
			case <-ctx.Done():
			}
			_ = session.Close()
			return
		}

		if finishReason == "tool_calls" && toolIndex == 0 {
			finishReason = "stop"
		}
		if finishReason != "tool_calls" {
			_ = session.Close()
		}

		endChunk, _ := json.Marshal(map[string]any{
			"id":      completionID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   req.Model,
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason},
			},
		})
		if !emitLine([]byte("data: " + string(endChunk))) {
			return
		}
		_ = usageFinal
		doneChunks := sdktranslator.TranslateStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, []byte("[DONE]"), &param)
		for i := range doneChunks {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: doneChunks[i]}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Chunks: out}, nil
}

// executeImages serves OpenAI /v1/images/generations requests by driving the
// Cursor Agent's built-in GenerateImage tool. Cursor's server performs the
// actual generation; the proxy auto-approves and returns base64 image data.
func (e *CursorExecutor) executeImages(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	_ = opts
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	prompt, refs, err := cursorImagesRequest(req.Payload)
	if err != nil {
		return resp, err
	}
	creds, err := e.ensureCredentials(ctx, auth)
	if err != nil {
		return resp, err
	}
	result, err := cursorlib.RunImageGeneration(ctx, creds, cursorlib.ImageGenerationAgentModel, prompt, refs...)
	if err != nil {
		return resp, err
	}
	reporter.Publish(ctx, usage.Detail{
		InputTokens:     result.InputTokens,
		OutputTokens:    result.OutputTokens,
		CachedTokens:    result.CacheReadTokens,
		CacheReadTokens: result.CacheReadTokens,
		ReasoningTokens: result.ReasoningTokens,
		TotalTokens:     result.InputTokens + result.OutputTokens,
	})
	resp = cliproxyexecutor.Response{Payload: buildCursorImagesResponse(result.Images)}
	return resp, nil
}

// executeImagesStream serves streaming /v1/images requests. Cursor has no
// partial-image protocol, so the completed image is emitted as one SSE event.
func (e *CursorExecutor) executeImagesStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	_ = opts
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	prompt, refs, err := cursorImagesRequest(req.Payload)
	if err != nil {
		return nil, err
	}
	creds, err := e.ensureCredentials(ctx, auth)
	if err != nil {
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		result, errRun := cursorlib.RunImageGeneration(ctx, creds, cursorlib.ImageGenerationAgentModel, prompt, refs...)
		if errRun != nil {
			reporter.PublishFailure(ctx, errRun)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errRun}:
			case <-ctx.Done():
			}
			return
		}
		reporter.Publish(ctx, usage.Detail{
			InputTokens:     result.InputTokens,
			OutputTokens:    result.OutputTokens,
			CachedTokens:    result.CacheReadTokens,
			CacheReadTokens: result.CacheReadTokens,
			ReasoningTokens: result.ReasoningTokens,
			TotalTokens:     result.InputTokens + result.OutputTokens,
		})
		created := time.Now().Unix()
		for _, img := range result.Images {
			data := []byte(`{"type":"image_generation.completed"}`)
			data, _ = sjson.SetBytes(data, "created", created)
			data, _ = sjson.SetBytes(data, "b64_json", img.Base64)
			if img.MimeType != "" {
				data, _ = sjson.SetBytes(data, "mime_type", img.MimeType)
			}
			frame := append([]byte("event: image_generation.completed\ndata: "), data...)
			frame = append(frame, '\n', '\n')
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: frame}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Chunks: out}, nil
}

// cursorMaxReferenceImages caps how many input images one edit run advertises.
// Cursor renders server-side and the bytes travel back over the read exec, so
// an unbounded list would be a cheap way to blow up a single turn.
const cursorMaxReferenceImages = 4

// cursorImagesRequest extracts the prompt and any input images from an images
// payload. Both the JSON and multipart shapes the OpenAI images handlers
// produce are accepted; input images turn the run into an image-to-image edit.
func cursorImagesRequest(payload []byte) (string, []cursorlib.ReferenceImage, error) {
	if boundary, ok := multipartBoundary(payload); ok {
		return cursorImagesFromMultipart(payload, boundary)
	}
	if !gjson.ValidBytes(payload) {
		return "", nil, fmt.Errorf("cursor: %s requires a JSON or multipart/form-data body", cursorImageModelID)
	}
	prompt := strings.TrimSpace(gjson.GetBytes(payload, "prompt").String())
	if prompt == "" {
		return "", nil, fmt.Errorf("cursor: images request prompt is required")
	}
	if gjson.GetBytes(payload, "mask").Exists() {
		return "", nil, fmt.Errorf("cursor: %s does not support masks; omit mask and describe the change in the prompt", cursorImageModelID)
	}
	var sources []string
	collect := func(res gjson.Result) {
		switch {
		case res.IsArray():
			for _, item := range res.Array() {
				if item.Type == gjson.String {
					sources = append(sources, item.String())
					continue
				}
				for _, key := range []string{"image_url", "url"} {
					if v := strings.TrimSpace(item.Get(key).String()); v != "" {
						sources = append(sources, v)
						break
					}
				}
			}
		case res.Type == gjson.String:
			sources = append(sources, res.String())
		case res.IsObject():
			for _, key := range []string{"image_url", "url"} {
				if v := strings.TrimSpace(res.Get(key).String()); v != "" {
					sources = append(sources, v)
					break
				}
			}
		}
	}
	collect(gjson.GetBytes(payload, "images"))
	collect(gjson.GetBytes(payload, "image"))

	refs := make([]cursorlib.ReferenceImage, 0, len(sources))
	for _, src := range sources {
		data, mime, err := decodeImageDataURL(src)
		if err != nil {
			return "", nil, err
		}
		refs = append(refs, cursorlib.ReferenceImage{Data: data, MimeType: mime})
	}
	refs, err := finalizeReferenceImages(refs)
	if err != nil {
		return "", nil, err
	}
	return prompt, refs, nil
}

// multipartBoundary recovers the boundary from a multipart body. The images
// handlers re-encode uploads before the executor sees them and the inbound
// Content-Type is not carried along, so it is read back off the payload.
func multipartBoundary(payload []byte) (string, bool) {
	if !bytes.HasPrefix(payload, []byte("--")) {
		return "", false
	}
	end := bytes.IndexByte(payload, '\n')
	if end <= 2 {
		return "", false
	}
	boundary := strings.TrimSpace(string(payload[2:end]))
	if boundary == "" {
		return "", false
	}
	return boundary, true
}

func cursorImagesFromMultipart(payload []byte, boundary string) (string, []cursorlib.ReferenceImage, error) {
	reader := multipart.NewReader(bytes.NewReader(payload), boundary)
	form, err := reader.ReadForm(cursorMultipartMemoryLimit)
	if err != nil {
		return "", nil, fmt.Errorf("cursor: parse multipart images request: %w", err)
	}
	defer func() { _ = form.RemoveAll() }()

	prompt := ""
	if values := form.Value["prompt"]; len(values) > 0 {
		prompt = strings.TrimSpace(values[0])
	}
	if prompt == "" {
		return "", nil, fmt.Errorf("cursor: images request prompt is required")
	}
	if len(form.File["mask"]) > 0 {
		return "", nil, fmt.Errorf("cursor: %s does not support masks; omit mask and describe the change in the prompt", cursorImageModelID)
	}

	headers := form.File["image[]"]
	if len(headers) == 0 {
		headers = form.File["image"]
	}
	refs := make([]cursorlib.ReferenceImage, 0, len(headers))
	for _, header := range headers {
		file, errOpen := header.Open()
		if errOpen != nil {
			return "", nil, fmt.Errorf("cursor: open uploaded image %q: %w", header.Filename, errOpen)
		}
		data, errRead := io.ReadAll(io.LimitReader(file, cursorMaxReferenceImageBytes+1))
		_ = file.Close()
		if errRead != nil {
			return "", nil, fmt.Errorf("cursor: read uploaded image %q: %w", header.Filename, errRead)
		}
		refs = append(refs, cursorlib.ReferenceImage{Data: data, MimeType: header.Header.Get("Content-Type")})
	}
	refs, err = finalizeReferenceImages(refs)
	if err != nil {
		return "", nil, err
	}
	return prompt, refs, nil
}

// cursorMultipartMemoryLimit bounds in-memory multipart buffering; larger parts
// spill to temp files that ReadForm cleans up.
const cursorMultipartMemoryLimit = 32 << 20

// cursorMaxReferenceImageBytes bounds a single decoded input image.
const cursorMaxReferenceImageBytes = 20 << 20

// finalizeReferenceImages validates decoded input images and assigns the
// workspace paths advertised to Cursor.
func finalizeReferenceImages(refs []cursorlib.ReferenceImage) ([]cursorlib.ReferenceImage, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if len(refs) > cursorMaxReferenceImages {
		return nil, fmt.Errorf("cursor: %s accepts at most %d input images, got %d", cursorImageModelID, cursorMaxReferenceImages, len(refs))
	}
	out := make([]cursorlib.ReferenceImage, 0, len(refs))
	for i, ref := range refs {
		if len(ref.Data) == 0 {
			return nil, fmt.Errorf("cursor: input image %d is empty", i+1)
		}
		if len(ref.Data) > cursorMaxReferenceImageBytes {
			return nil, fmt.Errorf("cursor: input image %d exceeds the %d MiB limit", i+1, cursorMaxReferenceImageBytes>>20)
		}
		// Sniff rather than trust the declared type: a caller-supplied MIME
		// can be absent, generic, or simply wrong, and shipping non-image
		// bytes upstream fails far less legibly than rejecting them here.
		mime := http.DetectContentType(ref.Data)
		if !strings.HasPrefix(mime, "image/") {
			return nil, fmt.Errorf("cursor: input image %d is not a recognised image (detected %q)", i+1, mime)
		}
		out = append(out, cursorlib.ReferenceImage{
			Path:     cursorlib.ReferenceImagePath(i, mime),
			Data:     ref.Data,
			MimeType: mime,
		})
	}
	return out, nil
}

// decodeImageDataURL accepts the data URLs the images handlers emit, plus bare
// base64. Remote URLs are rejected: fetching them would let a caller drive
// outbound requests from the proxy.
func decodeImageDataURL(src string) ([]byte, string, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return nil, "", fmt.Errorf("cursor: input image is empty")
	}
	mime := ""
	if strings.HasPrefix(src, "data:") {
		comma := strings.IndexByte(src, ',')
		if comma < 0 {
			return nil, "", fmt.Errorf("cursor: malformed image data URL")
		}
		meta := src[5:comma]
		if !strings.Contains(meta, ";base64") {
			return nil, "", fmt.Errorf("cursor: image data URLs must be base64 encoded")
		}
		mime = strings.TrimSpace(strings.SplitN(meta, ";", 2)[0])
		src = src[comma+1:]
	} else if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		return nil, "", fmt.Errorf("cursor: %s requires inline image data; remote image URLs are not supported", cursorImageModelID)
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(src))
	if err != nil {
		return nil, "", fmt.Errorf("cursor: decode input image: %w", err)
	}
	return data, mime, nil
}

// buildCursorImagesResponse renders images in the upstream JSON shape the
// OpenAI images handlers already parse (data[].b64_json / mime_type).
func buildCursorImagesResponse(images []cursorlib.GeneratedImage) []byte {
	out := []byte(`{"created":0,"data":[]}`)
	out, _ = sjson.SetBytes(out, "created", time.Now().Unix())
	for _, img := range images {
		item := []byte(`{}`)
		item, _ = sjson.SetBytes(item, "b64_json", img.Base64)
		if img.MimeType != "" {
			item, _ = sjson.SetBytes(item, "mime_type", img.MimeType)
		}
		out, _ = sjson.SetRawBytes(out, "data.-1", item)
	}
	return out
}

// rewriteImageChatMessages collapses a chat request against the cursor-image
// model into a single instruction built from the latest user turn. Inline
// images on that turn make it an edit; without them it is a plain generation.
func rewriteImageChatMessages(messages []cursorlib.ChatMessage, refs []cursorlib.ReferenceImage) ([]cursorlib.ChatMessage, error) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && strings.TrimSpace(messages[i].Content) != "" {
			instruction := cursorlib.ImageGenerationInstruction(messages[i].Content)
			if len(refs) > 0 {
				instruction = cursorlib.ImageEditInstruction(messages[i].Content, refs)
			}
			return []cursorlib.ChatMessage{{Role: "user", Content: instruction}}, nil
		}
	}
	if len(refs) > 0 {
		return nil, fmt.Errorf("cursor: image edit requires a text prompt describing the change")
	}
	return nil, fmt.Errorf("cursor: image generation requires a user prompt message")
}

// extractChatImageInputs collects the inline images attached to the latest user
// turn. Only that turn is considered: earlier images belong to previous
// generations, and feeding them back would silently blend unrelated inputs.
func extractChatImageInputs(body []byte) ([]cursorlib.ReferenceImage, error) {
	arr := gjson.GetBytes(body, "messages")
	if !arr.IsArray() {
		return nil, nil
	}
	items := arr.Array()
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Get("role").String() != "user" {
			continue
		}
		content := items[i].Get("content")
		if !content.IsArray() {
			return nil, nil
		}
		var refs []cursorlib.ReferenceImage
		for _, part := range content.Array() {
			url := strings.TrimSpace(part.Get("image_url.url").String())
			if url == "" {
				url = strings.TrimSpace(part.Get("image_url").String())
			}
			if url == "" {
				continue
			}
			data, mime, err := decodeImageDataURL(url)
			if err != nil {
				return nil, err
			}
			refs = append(refs, cursorlib.ReferenceImage{Data: data, MimeType: mime})
		}
		return finalizeReferenceImages(refs)
	}
	return nil, nil
}

func openCursorSession(ctx context.Context, creds cursorlib.AccountCredentials, model string, messages []cursorlib.ChatMessage, tools []cursorlib.ToolDefinition, opts ...cursorlib.SessionOption) (*cursorlib.Session, error) {
	results := trailingToolResults(messages)
	if len(results) > 0 {
		session, err := cursorlib.DefaultSessionManager().ResolveForToolResults(results)
		if err != nil {
			return nil, err
		}
		if err = session.SubmitToolResults(results); err != nil {
			return nil, err
		}
		return session, nil
	}
	return cursorlib.StartSession(ctx, creds, model, messages, tools, opts...)
}

func trailingToolResults(messages []cursorlib.ChatMessage) []cursorlib.ToolResult {
	start := -1
	for i := len(messages) - 1; i >= 0; i-- {
		switch messages[i].Role {
		case "tool":
			start = i
			continue
		case "assistant":
			if len(messages[i].ToolCalls) > 0 && start >= 0 {
				out := make([]cursorlib.ToolResult, 0, len(messages)-start)
				for _, msg := range messages[start:] {
					if msg.Role != "tool" || strings.TrimSpace(msg.ToolCallID) == "" {
						continue
					}
					out = append(out, cursorlib.ToolResult{
						ToolCallID: msg.ToolCallID,
						Name:       msg.Name,
						Content:    msg.Content,
					})
				}
				return out
			}
			return nil
		default:
			return nil
		}
	}
	return nil
}

// CountTokens returns a best-effort character-based estimate.
func (e *CursorExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = auth
	_ = opts
	chars := len(req.Payload)
	tokens := chars / 4
	if tokens < 1 {
		tokens = 1
	}
	payload := fmt.Appendf(nil, `{"input_tokens":%d}`, tokens)
	return cliproxyexecutor.Response{Payload: payload}, nil
}

// HttpRequest is unused for Cursor Agent protocol.
func (e *CursorExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	_ = ctx
	_ = auth
	_ = req
	return nil, fmt.Errorf("cursor executor: HttpRequest is not supported for Agent Connect")
}

func (e *CursorExecutor) ensureCredentials(ctx context.Context, auth *cliproxyauth.Auth) (cursorlib.AccountCredentials, error) {
	creds := cursorlib.CredentialsFromMetadata(authMetadata(auth))
	// API-key credentials (config cursor-api-key entries) carry no OAuth
	// refresh token; the key itself is exchanged for access tokens.
	refreshSecret := creds.RefreshToken
	if refreshSecret == "" {
		refreshSecret = creds.APIKey
	}
	if creds.AccessToken == "" && refreshSecret == "" {
		return creds, fmt.Errorf("cursor: auth missing access_token")
	}
	storage := &cursorauth.TokenStorage{
		AccessToken:  creds.AccessToken,
		RefreshToken: refreshSecret,
		Expired:      stringFromMeta(authMetadata(auth), "expired"),
	}
	if creds.AccessToken != "" && !storage.NeedsRefresh() {
		return creds, nil
	}
	refreshed, err := e.svc.RefreshToken(ctx, refreshSecret, creds.AuthClientID, creds.BaseURL)
	if err != nil {
		if creds.AccessToken == "" {
			return creds, fmt.Errorf("cursor: token exchange failed: %w", err)
		}
		log.Warnf("cursor token refresh failed, using existing token: %v", err)
		return creds, nil
	}
	creds.AccessToken = refreshed.AccessToken
	creds.RefreshToken = refreshed.RefreshToken
	if auth != nil && auth.Metadata != nil {
		auth.Metadata["access_token"] = refreshed.AccessToken
		auth.Metadata["refresh_token"] = refreshed.RefreshToken
		if !refreshed.ExpiresAt.IsZero() {
			auth.Metadata["expired"] = refreshed.ExpiresAt.UTC().Format(time.RFC3339)
		}
	}
	return creds, nil
}

func authMetadata(auth *cliproxyauth.Auth) map[string]any {
	if auth == nil {
		return nil
	}
	return auth.Metadata
}

func stringFromMeta(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	if v, ok := meta[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func extractChatMessages(body []byte) ([]cursorlib.ChatMessage, error) {
	arr := gjson.GetBytes(body, "messages")
	if !arr.IsArray() || len(arr.Array()) == 0 {
		return nil, fmt.Errorf("cursor: request messages are required")
	}
	out := make([]cursorlib.ChatMessage, 0, len(arr.Array()))
	for _, item := range arr.Array() {
		role := item.Get("role").String()
		content := item.Get("content")
		text := ""
		if content.IsArray() {
			var parts []string
			for _, part := range content.Array() {
				if part.Get("type").String() == "text" || part.Get("text").Exists() {
					parts = append(parts, part.Get("text").String())
				}
			}
			text = strings.Join(parts, "\n")
		} else {
			text = content.String()
		}
		if role == "" {
			continue
		}
		msg := cursorlib.ChatMessage{
			Role:       role,
			Content:    text,
			Name:       item.Get("name").String(),
			ToolCallID: item.Get("tool_call_id").String(),
		}
		if toolCalls := item.Get("tool_calls"); toolCalls.IsArray() {
			for _, tc := range toolCalls.Array() {
				argsRaw := tc.Get("function.arguments").String()
				var args map[string]any
				if argsRaw != "" {
					_ = json.Unmarshal([]byte(argsRaw), &args)
				}
				if args == nil {
					args = map[string]any{}
				}
				msg.ToolCalls = append(msg.ToolCalls, cursorlib.ToolCall{
					ID:        tc.Get("id").String(),
					Name:      tc.Get("function.name").String(),
					Arguments: args,
				})
			}
		}
		out = append(out, msg)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("cursor: no usable chat messages")
	}
	return out, nil
}

func extractTools(body []byte) []cursorlib.ToolDefinition {
	arr := gjson.GetBytes(body, "tools")
	if !arr.IsArray() {
		return nil
	}
	out := make([]cursorlib.ToolDefinition, 0, len(arr.Array()))
	for _, item := range arr.Array() {
		typ := item.Get("type").String()
		if typ != "" && typ != "function" {
			continue
		}
		fn := item.Get("function")
		name := fn.Get("name").String()
		if name == "" {
			name = item.Get("name").String()
		}
		if name == "" {
			continue
		}
		var params map[string]any
		paramsRaw := fn.Get("parameters").Raw
		if paramsRaw == "" {
			paramsRaw = item.Get("parameters").Raw
		}
		if paramsRaw != "" {
			_ = json.Unmarshal([]byte(paramsRaw), &params)
		}
		desc := fn.Get("description").String()
		if desc == "" {
			desc = item.Get("description").String()
		}
		out = append(out, cursorlib.ToolDefinition{
			Name:        name,
			Description: desc,
			Parameters:  params,
		})
	}
	return out
}

func buildOpenAIChatCompletion(model string, result *cursorlib.ChatResult) []byte {
	id := "chatcmpl-" + uuid.NewString()
	finish := result.FinishReason
	if finish == "" {
		finish = "stop"
	}
	message := map[string]any{
		"role":    "assistant",
		"content": result.Text,
	}
	if result.Thinking != "" {
		message["reasoning_content"] = result.Thinking
	}
	if len(result.Images) > 0 {
		images := make([]map[string]any, 0, len(result.Images))
		for i, img := range result.Images {
			images = append(images, map[string]any{
				"index":     i,
				"type":      "image_url",
				"image_url": map[string]any{"url": img.DataURL()},
			})
		}
		message["images"] = images
	}
	if len(result.ToolCalls) > 0 {
		calls := make([]map[string]any, 0, len(result.ToolCalls))
		for _, tc := range result.ToolCalls {
			args, _ := json.Marshal(tc.Arguments)
			if args == nil {
				args = []byte("{}")
			}
			calls = append(calls, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": string(args),
				},
			})
		}
		message["tool_calls"] = calls
		if finish == "stop" {
			finish = "tool_calls"
		}
	}
	payload := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       message,
				"finish_reason": finish,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     result.InputTokens,
			"completion_tokens": result.OutputTokens,
			"total_tokens":      result.InputTokens + result.OutputTokens,
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"error":"marshal failed"}`)
	}
	return b
}
