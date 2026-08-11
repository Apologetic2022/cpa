// Package relay implements the Mode-B control-plane gateway (docs ch.6): a pure
// control plane that never contacts Anthropic. Claude traffic is admitted (6.6.3),
// bound strictly 1 tenant : 1 account (S1), gated by a fail-closed ban/quarantine
// authority (F20), rate-limited per account (S2), then dispatched to the account's real
// Claude Code agent over a local socket. The agent's real Bun binary is the only thing
// that ever touches the Anthropic wire — this package cannot: its only network client
// dials per-account local sockets (see agentclient.go).
package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Executor is the relay control-plane executor for provider "claude". It replaces the
// direct-egress ClaudeExecutor when relay mode is enabled (docs 6.6).
type Executor struct {
	cfg       *config.Config
	admission *Admission
	limiters  *LimiterSet
	store     Authority

	turnTimeout    time.Duration // hard per-turn wall-clock cap (F11)
	turnMaxOutToks int64         // per-turn output token cap (F11), 0 = unlimited

	mu      sync.Mutex
	clients map[string]*AgentClient
}

// NewExecutor builds the relay executor from config. The affinity/ban authority is
// fail-closed (docs F8/F20): if it cannot be read, every dispatch is denied.
func NewExecutor(cfg *config.Config) *Executor {
	store, err := NewAuthority(cfg.Relay, cfg.AuthDir)
	if err != nil {
		// Unknown driver etc.: fail closed via an always-denying authority rather
		// than starting without the ban gate.
		log.Errorf("relay: authority init failed (%v); all dispatch will be denied (fail-closed)", err)
		store = &failedAuthority{err: err}
	}
	timeout := time.Duration(cfg.Relay.TurnTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	return &Executor{
		cfg:            cfg,
		admission:      NewAdmission(cfg.Relay.Admission),
		limiters:       NewLimiterSet(cfg.Relay),
		store:          store,
		turnTimeout:    timeout,
		turnMaxOutToks: int64(cfg.Relay.TurnMaxOutputTokens),
		clients:        make(map[string]*AgentClient),
	}
}

// Identifier returns the provider key handled by this executor.
func (e *Executor) Identifier() string { return "claude" }

// Store exposes the affinity/ban authority for management surfaces.
func (e *Executor) Store() Authority { return e.store }

// failedAuthority denies everything (construction-time fail-closed).
type failedAuthority struct{ err error }

func (f *failedAuthority) Healthy() bool { return false }
func (f *failedAuthority) BindTenant(_, _ string) error {
	return &BanError{Reason: "relay authority init failed (fail-closed): " + f.err.Error()}
}
func (f *failedAuthority) CheckDispatch(_ string) error {
	return &BanError{Reason: "relay authority init failed (fail-closed): " + f.err.Error()}
}
func (f *failedAuthority) Quarantine(_, _ string) error { return f.err }
func (f *failedAuthority) Ban(_, _ string) error        { return f.err }
func (f *failedAuthority) Reinstate(_ string) error     { return f.err }
func (f *failedAuthority) Snapshot() (map[string]string, map[string]AccountState) {
	return map[string]string{}, map[string]AccountState{}
}

// CountTokens is not part of the constrained coding-agent surface.
func (e *Executor) CountTokens(_ context.Context, _ *cliproxyauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, reject(501, "count_tokens is unsupported on the coding-agent surface; use the official API")
}

// HttpRequest injects credentials into arbitrary upstream requests in classic mode; in
// relay mode the gateway holds no egress path at all, so this is refused.
func (e *Executor) HttpRequest(_ context.Context, _ *cliproxyauth.Auth, _ *http.Request) (*http.Response, error) {
	return nil, errors.New("relay mode: the gateway never contacts Anthropic (no provider egress)")
}

// turn is one admitted, dispatched tenant turn.
type turn struct {
	stream    *InvokeStream
	client    *AgentClient
	limiter   *AccountLimiter
	release   func()
	accountID string
	model     string

	mu        sync.Mutex
	streamID  string
	maxIn     int64 // Anthropic usage events are cumulative per component
	maxOut    int64
	maxCache  int64
	metered   int64 // total already pushed into the limiter (F12)
}

func (t *turn) setStreamID(id string) {
	t.mu.Lock()
	t.streamID = id
	t.mu.Unlock()
}

func (t *turn) getStreamID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.streamID
}

// meter records incremental usage (F12): Anthropic usage events are cumulative per
// component (message_start carries input, message_delta carries running output), so we
// keep per-component maxima and push only the growth into the limiter. Aborted turns
// still account every token they consumed.
func (t *turn) meter(input, output, cacheRead int64) {
	if input > t.maxIn {
		t.maxIn = input
	}
	if output > t.maxOut {
		t.maxOut = output
	}
	if cacheRead > t.maxCache {
		t.maxCache = cacheRead
	}
	t.push()
}

// meterAbsolute reconciles against the authoritative turn-total from relay_control.
func (t *turn) meterAbsolute(u *RelayUsage) {
	if u == nil {
		return
	}
	t.meter(u.InputTokens, u.OutputTokens, u.CacheReadInputTokens)
}

func (t *turn) push() {
	total := t.maxIn + t.maxOut + t.maxCache
	if total > t.metered {
		t.limiter.OnUsage(t.model, 0, total-t.metered, 0)
		t.metered = total
	}
}

// outTokens reports the cumulative output tokens seen so far (F11 per-turn cap).
func (t *turn) outTokens() int64 { return t.maxOut }

// prepare runs the full dispatch gate (docs 2.2):
//
//	admission -> tenant 1:1 bind -> fail-closed ban check -> session key ->
//	per-account limiter (concurrency=1, fixed budget, quiet hours) -> agent invoke.
func (e *Executor) prepare(ctx context.Context, auth *cliproxyauth.Auth, claudeBody []byte, model string, opts cliproxyexecutor.Options) (*turn, error) {
	// 1. Admission (6.6.3): reject what a coding agent cannot faithfully serve.
	if err := e.admission.Admit(claudeBody); err != nil {
		return nil, err
	}

	// 2. Tenant identity (docs 2.2.1: tenant key -> tenant_id) and strict 1:1 binding.
	tenant := tenantFromContext(ctx)
	accountID := accountIDOf(auth)
	if err := e.store.BindTenant(tenant, accountID); err != nil {
		return nil, err
	}

	// 3. Synchronous fail-closed ban/quarantine gate (F20).
	if err := e.store.CheckDispatch(accountID); err != nil {
		return nil, err
	}

	// 4. Agent endpoint (fail-closed: no endpoint, no dispatch).
	client, err := e.clientFor(accountID)
	if err != nil {
		return nil, err
	}

	// 5. Per-account S2 limiter: concurrency=1 + fixed budget + quiet hours + bounded FIFO.
	limiter := e.limiters.For(accountID)
	estTokens := gjson.GetBytes(claudeBody, "max_tokens").Int()
	if estTokens <= 0 {
		estTokens = 4096
	}
	release, err := limiter.Acquire(ctx, estTokens, model)
	if err != nil {
		return nil, err
	}

	// 6. Inject the turn as a real user message; the CC session owns history (6.3).
	message, err := lastUserMessage(claudeBody)
	if err != nil {
		release()
		return nil, err
	}
	sessionKey := sessionKeyFor(tenant, relaySessionOverride(opts), claudeBody)
	var betas []string
	if b := gjson.GetBytes(claudeBody, "betas"); b.Exists() && b.IsArray() {
		for _, item := range b.Array() {
			betas = append(betas, item.String())
		}
	}

	stream, err := client.Invoke(ctx, InvokeRequest{
		SessionKey: sessionKey,
		Model:      model,
		Betas:      betas,
		Message:    message,
	})
	if err != nil {
		release()
		var httpErr *AgentHTTPError
		if errors.As(err, &httpErr) {
			e.scanBanSignature(accountID, httpErr.Body)
		}
		return nil, err
	}

	return &turn{
		stream:    stream,
		client:    client,
		limiter:   limiter,
		release:   release,
		accountID: accountID,
		model:     model,
	}, nil
}

// scanBanSignature quarantines the account immediately on a ban marker (docs 6.7.3:
// disabled-body = instant quarantine, no auto-retry, page ops).
func (e *Executor) scanBanSignature(accountID string, body []byte) {
	sig, hit := DetectBanSignature(body)
	if !hit {
		return
	}
	if err := e.store.Quarantine(accountID, "upstream ban signature: "+sig); err != nil {
		log.Errorf("relay: failed to persist quarantine for %s: %v", accountID, err)
	}
	log.Errorf("relay: ACCOUNT QUARANTINED %s — ban signature %q; no auto-retry, operator action required", accountID, sig)
}

// ExecuteStream handles one streaming tenant turn (docs 6.7.2): the agent's raw
// Anthropic SSE is passed through byte-for-byte, closed by the relay_control frame.
func (e *Executor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := newUsageReporter(ctx, e, baseModel, auth)
	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("claude")

	stream := from != to
	claudeBody := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, stream)
	claudeBody, _ = sjson.SetBytes(claudeBody, "model", baseModel)

	// F11: hard per-turn wall-clock cap. Hitting it aborts the turn with a clean error
	// (and cancels upstream generation), never leaves a half-response hanging.
	turnCtx, turnCancel := context.WithTimeout(ctx, e.turnTimeout)

	t, err := e.prepare(turnCtx, auth, claudeBody, baseModel, opts)
	if err != nil {
		turnCancel()
		reporter.publishFailure(ctx, err)
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer t.release()
		defer turnCancel()

		// Cancel contract (F11): tenant disconnect or wall-clock timeout aborts
		// upstream generation — mandatory, not optional.
		cancelWatch := make(chan struct{})
		defer close(cancelWatch)
		go func() {
			select {
			case <-turnCtx.Done():
				if id := t.getStreamID(); id != "" {
					t.client.Cancel(id)
				}
			case <-cancelWatch:
			}
		}()

		var param any
		emit := func(payload []byte) bool {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: payload}:
				return true
			case <-ctx.Done():
				return false
			}
		}

		originalPayload := req.Payload
		if len(opts.OriginalRequest) > 0 {
			originalPayload = opts.OriginalRequest
		}

		for ev := range t.stream.Events {
			switch ev.Event {
			case "relay_start":
				var start RelayStart
				if json.Unmarshal(ev.Data, &start) == nil {
					t.setStreamID(start.StreamID)
				}
				continue
			case "relay_control":
				var control RelayControl
				if json.Unmarshal(ev.Data, &control) == nil {
					t.meterAbsolute(control.Usage)
				}
				// Tenants on the constrained surface receive the tail frame (docs 6.7.2).
				if from == to {
					if !emit(formatSSE("relay_control", ev.Data)) {
						return
					}
				}
				continue
			case "error":
				e.scanBanSignature(t.accountID, ev.Data)
			}

			// Regular Anthropic event: incremental metering (F12) + passthrough/translate.
			raw := formatSSE(ev.Event, ev.Data)
			for _, line := range splitSSELines(raw) {
				if detail, ok := parseClaudeStreamUsage(line); ok {
					reporter.publish(ctx, detail)
					t.meter(detail.InputTokens, detail.OutputTokens, detail.CacheReadTokens)
				}
				if from == to {
					if !emit(append(line, '\n')) {
						return
					}
				}
			}
			// F11: per-turn output token cap — abort the runaway turn cleanly.
			if e.turnMaxOutToks > 0 && t.outTokens() > e.turnMaxOutToks {
				if id := t.getStreamID(); id != "" {
					t.client.Cancel(id)
				}
				capErr := reject(422, "turn aborted: per-turn output token cap reached (F11)")
				reporter.publishFailure(ctx, capErr)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: capErr}:
				case <-ctx.Done():
				}
				return
			}
			if from != to {
				for _, line := range splitSSELines(raw) {
					chunks := sdktranslator.TranslateStream(ctx, to, responseFormat, req.Model, originalPayload, claudeBody, append(line, '\n'), &param)
					for i := range chunks {
						if !emit(chunks[i]) {
							return
						}
					}
				}
			} else {
				emit([]byte("\n"))
			}
		}

		if scanErr := t.stream.Err(); scanErr != nil && !errors.Is(scanErr, context.Canceled) {
			reporter.publishFailure(ctx, scanErr)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: scanErr}:
			case <-ctx.Done():
			}
		}
	}()

	headers := http.Header{}
	headers.Set("Content-Type", "text/event-stream")
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, nil
}

// Execute handles one non-streaming tenant turn by aggregating the agent's SSE into a
// complete Anthropic message response (the constrained surface is SSE-first; this path
// exists for clients that cannot stream).
func (e *Executor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := newUsageReporter(ctx, e, baseModel, auth)
	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("claude")

	claudeBody := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, from != to)
	claudeBody, _ = sjson.SetBytes(claudeBody, "model", baseModel)

	// F11: hard per-turn wall-clock cap.
	turnCtx, turnCancel := context.WithTimeout(ctx, e.turnTimeout)
	defer turnCancel()

	t, err := e.prepare(turnCtx, auth, claudeBody, baseModel, opts)
	if err != nil {
		reporter.publishFailure(ctx, err)
		return cliproxyexecutor.Response{}, err
	}
	defer t.release()

	// Cancel contract for non-stream turns too.
	cancelWatch := make(chan struct{})
	defer close(cancelWatch)
	go func() {
		select {
		case <-turnCtx.Done():
			if id := t.getStreamID(); id != "" {
				t.client.Cancel(id)
			}
		case <-cancelWatch:
		}
	}()

	agg := newMessageAggregator()
	for ev := range t.stream.Events {
		switch ev.Event {
		case "relay_start":
			var start RelayStart
			if json.Unmarshal(ev.Data, &start) == nil {
				t.setStreamID(start.StreamID)
			}
		case "relay_control":
			var control RelayControl
			if json.Unmarshal(ev.Data, &control) == nil {
				t.meterAbsolute(control.Usage)
			}
		case "error":
			e.scanBanSignature(t.accountID, ev.Data)
		default:
			for _, line := range splitSSELines(formatSSE(ev.Event, ev.Data)) {
				if detail, ok := parseClaudeStreamUsage(line); ok {
					reporter.publish(ctx, detail)
					t.meter(detail.InputTokens, detail.OutputTokens, detail.CacheReadTokens)
				}
			}
			if err := agg.add(ev.Event, ev.Data); err != nil {
				reporter.publishFailure(ctx, err)
				return cliproxyexecutor.Response{}, err
			}
		}
	}
	if scanErr := t.stream.Err(); scanErr != nil && !errors.Is(scanErr, context.Canceled) {
		reporter.publishFailure(ctx, scanErr)
		return cliproxyexecutor.Response{}, scanErr
	}

	payload, err := agg.message()
	if err != nil {
		reporter.publishFailure(ctx, err)
		return cliproxyexecutor.Response{}, err
	}
	if from != to {
		originalPayload := req.Payload
		if len(opts.OriginalRequest) > 0 {
			originalPayload = opts.OriginalRequest
		}
		var param any
		payload = sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, originalPayload, claudeBody, payload, &param)
	}
	return cliproxyexecutor.Response{Payload: payload}, nil
}

// clientFor returns (building on first use) the per-account agent client. Missing or
// invalid endpoint config fails closed.
func (e *Executor) clientFor(accountID string) (*AgentClient, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.clients[accountID]; ok {
		return c, nil
	}
	endpoint := ""
	if e.cfg.Relay.Agents != nil {
		endpoint = strings.TrimSpace(e.cfg.Relay.Agents[accountID])
	}
	if endpoint == "" {
		return nil, &BanError{Status: 503, Reason: fmt.Sprintf("no relay agent endpoint configured for account %q (fail-closed)", accountID)}
	}
	c, err := NewAgentClient(accountID, endpoint)
	if err != nil {
		return nil, &BanError{Status: 503, Reason: err.Error()}
	}
	e.clients[accountID] = c
	return c, nil
}

// accountIDOf derives the stable account identifier from the selected auth.
func accountIDOf(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return "unknown"
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		return id
	}
	if label := strings.TrimSpace(auth.Label); label != "" {
		return label
	}
	return "unknown"
}

// tenantFromContext extracts the tenant identity (docs 2.2.1) from the inbound API key
// captured by the auth middleware, hashed so raw keys never touch the store.
func tenantFromContext(ctx context.Context) string {
	if ctx != nil {
		if ginCtx, ok := ctx.Value("gin").(interface{ Get(string) (any, bool) }); ok && ginCtx != nil {
			if raw, ok := ginCtx.Get("userApiKey"); ok {
				if s, ok := raw.(string); ok && strings.TrimSpace(s) != "" {
					sum := sha256.Sum256([]byte("relay-tenant:" + s))
					return "t-" + hex.EncodeToString(sum[:8])
				}
			}
		}
	}
	return "t-default"
}

// relaySessionOverride reads the explicit session override (docs 6.3: X-Relay-Session).
func relaySessionOverride(opts cliproxyexecutor.Options) string {
	if opts.Headers != nil {
		if v := strings.TrimSpace(opts.Headers.Get("X-Relay-Session")); v != "" {
			return v
		}
	}
	if opts.Metadata != nil {
		if v, ok := opts.Metadata["relay_session"].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// lastUserMessage extracts the final user message from a /v1/messages body as the turn
// to inject (docs 6.2.3: tenant turn = one real user message; the CC session owns the
// conversation history).
func lastUserMessage(body []byte) (json.RawMessage, error) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return nil, reject(400, "request carries no messages; the coding-agent surface takes one user turn per call")
	}
	arr := messages.Array()
	for i := len(arr) - 1; i >= 0; i-- {
		if strings.EqualFold(arr[i].Get("role").String(), "user") {
			return json.RawMessage(arr[i].Raw), nil
		}
	}
	return nil, reject(400, "request carries no user message; nothing to inject into the agent session")
}

// firstUserText returns the first user message's text for session-key derivation.
func firstUserText(body []byte) string {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return ""
	}
	for _, msg := range messages.Array() {
		if !strings.EqualFold(msg.Get("role").String(), "user") {
			continue
		}
		var b strings.Builder
		extractText(msg.Get("content"), &b)
		return b.String()
	}
	return ""
}

// formatSSE re-serializes one parsed frame as canonical SSE text.
func formatSSE(event string, data []byte) []byte {
	var b strings.Builder
	if event != "" && event != "message" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteByte('\n')
	}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// splitSSELines splits serialized SSE text back into lines for the usage parser and
// per-line translators (the downstream pipeline consumes line-granularity input).
func splitSSELines(raw []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\n' {
			lines = append(lines, raw[start:i])
			start = i + 1
		}
	}
	if start < len(raw) {
		lines = append(lines, raw[start:])
	}
	return lines
}
