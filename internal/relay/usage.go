package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

// Self-contained usage reporting for the relay invoke path (docs 6.6.2: the invoke
// path must not reach egress-capable stacks). This mirrors the semantics of
// internal/runtime/executor/helps.UsageReporter WITHOUT importing the helps package:
// helps drags utls/gin/home into the relay dependency closure (helps/utls_client.go),
// which the compile-time gate forbids. Only sdk/cliproxy/usage + gjson are used here.
type usageReporter struct {
	provider     string
	executorType string
	model        string
	alias        string
	source       string
	apiKey       string
	authID       string
	authIndex    string
	authType     string
	reasoning    string
	serviceTier  string
	requestedAt  time.Time
	once         sync.Once
}

func newUsageReporter(ctx context.Context, executor *Executor, model string, auth *cliproxyauth.Auth) *usageReporter {
	provider := ""
	executorType := ""
	if executor != nil {
		provider = executor.Identifier()
		executorType = executorTypeName(executor)
	}
	alias := usage.RequestedModelAliasFromContext(ctx)
	if alias == "" {
		alias = model
	}
	apiKey := apiKeyFromContext(ctx)
	r := &usageReporter{
		provider:     provider,
		executorType: executorType,
		model:        model,
		alias:        strings.TrimSpace(alias),
		source:       usageSource(auth, apiKey),
		apiKey:       apiKey,
		authType:     usageAuthType(auth),
		reasoning:    usage.ReasoningEffortFromContext(ctx),
		serviceTier:  usage.ServiceTierFromContext(ctx),
		requestedAt:  time.Now(),
	}
	if auth != nil {
		r.authID = auth.ID
		r.authIndex = auth.EnsureIndex()
	}
	return r
}

func (r *usageReporter) publish(ctx context.Context, detail usage.Detail) {
	r.publishWithOutcome(ctx, detail, false, usage.Failure{})
}

func (r *usageReporter) publishFailure(ctx context.Context, errs ...error) {
	r.publishWithOutcome(ctx, usage.Detail{}, true, failureFromErrors(errs...))
}

func (r *usageReporter) publishWithOutcome(ctx context.Context, detail usage.Detail, failed bool, fail usage.Failure) {
	if r == nil {
		return
	}
	if detail.TotalTokens == 0 {
		if total := detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens; total > 0 {
			detail.TotalTokens = total
		}
	}
	r.once.Do(func() {
		latency := time.Since(r.requestedAt)
		if latency < 0 {
			latency = 0
		}
		usage.PublishRecord(ctx, usage.Record{
			Provider:            r.provider,
			ExecutorType:        r.executorType,
			Model:               r.model,
			Alias:               r.alias,
			Source:              r.source,
			APIKey:              r.apiKey,
			AuthID:              r.authID,
			AuthIndex:           r.authIndex,
			AuthType:            r.authType,
			ReasoningEffort:     r.reasoning,
			ServiceTier:         r.serviceTier,
			RequestServiceTier:  r.serviceTier,
			ResponseServiceTier: strings.TrimSpace(detail.ResponseServiceTier),
			RequestedAt:         r.requestedAt,
			Latency:             latency,
			Failed:              failed,
			Fail:                fail,
			Detail:              detail,
		})
	})
}

func executorTypeName(executor any) string {
	if executor == nil {
		return ""
	}
	t := reflect.TypeOf(executor)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return strings.TrimSpace(t.Name())
}

// apiKeyFromContext extracts the tenant API key captured by the auth middleware. The
// gin context is reached through a structural interface so this package never imports
// gin (which would pull crypto/tls and net/http server stacks into the closure).
func apiKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ginCtx, ok := ctx.Value("gin").(interface{ Get(string) (any, bool) })
	if !ok || ginCtx == nil {
		return ""
	}
	v, exists := ginCtx.Get("userApiKey")
	if !exists {
		return ""
	}
	switch value := v.(type) {
	case string:
		return value
	case fmt.Stringer:
		return value.String()
	default:
		return fmt.Sprintf("%v", value)
	}
}

func usageSource(auth *cliproxyauth.Auth, ctxAPIKey string) string {
	if auth != nil {
		if _, value := auth.AccountInfo(); value != "" {
			return strings.TrimSpace(value)
		}
		if auth.Metadata != nil {
			if email, ok := auth.Metadata["email"].(string); ok {
				if trimmed := strings.TrimSpace(email); trimmed != "" {
					return trimmed
				}
			}
		}
		if auth.Attributes != nil {
			if key := strings.TrimSpace(auth.Attributes["api_key"]); key != "" {
				return key
			}
		}
	}
	return strings.TrimSpace(ctxAPIKey)
}

func usageAuthType(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	return auth.AuthKind()
}

func failureFromErrors(errs ...error) usage.Failure {
	for _, err := range errs {
		if err == nil {
			continue
		}
		fail := usage.Failure{Body: strings.TrimSpace(err.Error())}
		var se interface{ StatusCode() int }
		if errors.As(err, &se) && se != nil {
			fail.StatusCode = se.StatusCode()
		}
		return fail
	}
	return usage.Failure{}
}

// parseClaudeStreamUsage extracts the usage node from one SSE data line of a Claude
// stream (message_start / message_delta). Same semantics as helps.ParseClaudeStreamUsage.
func parseClaudeStreamUsage(line []byte) (usage.Detail, bool) {
	payload := sseJSONPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	usageNode := gjson.GetBytes(payload, "usage")
	if !usageNode.Exists() {
		return usage.Detail{}, false
	}
	cacheReadTokens := usageNode.Get("cache_read_input_tokens").Int()
	cacheCreationTokens := usageNode.Get("cache_creation_input_tokens").Int()
	detail := usage.Detail{
		InputTokens:         usageNode.Get("input_tokens").Int(),
		OutputTokens:        usageNode.Get("output_tokens").Int(),
		CachedTokens:        cacheReadTokens,
		CacheReadTokens:     cacheReadTokens,
		CacheCreationTokens: cacheCreationTokens,
	}
	if detail.CachedTokens == 0 {
		detail.CachedTokens = detail.CacheCreationTokens
	}
	detail.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.CacheReadTokens + detail.CacheCreationTokens
	return detail, true
}

func sseJSONPayload(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil
	}
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("event:")) {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	return trimmed
}
