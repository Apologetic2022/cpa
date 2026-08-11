package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func testAuth(id string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: id, Provider: "claude"}
}

// ---------- T5 (docs 6.10): admission fail-closed ----------

func TestAdmissionRejectsUnsupportedShapes(t *testing.T) {
	ad := NewAdmission(config.RelayAdmissionConfig{})
	cases := []struct {
		name    string
		payload string
	}{
		{"custom system", `{"messages":[{"role":"user","content":"fix the bug in main.go"}], "system":"You are a pirate"}`},
		{"custom tools", `{"messages":[{"role":"user","content":"fix the bug in main.go"}], "tools":[{"name":"get_weather","input_schema":{}}]}`},
		{"assistant prefill", `{"messages":[{"role":"user","content":"fix the bug in main.go"},{"role":"assistant","content":"Sure,"}]}`},
		{"json mode", `{"messages":[{"role":"user","content":"fix the bug in main.go"}], "response_format":{"type":"json_object"}}`},
		{"stop sequences", `{"messages":[{"role":"user","content":"fix the bug in main.go"}], "stop_sequences":["\n\nHuman:"]}`},
		{"non-coding long-form", `{"messages":[{"role":"user","content":"请写一篇关于宋代经济与海上贸易关系演变的议论文，要求旁征博引，不少于两千字，并给出三种不同史观下的解读"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ad.Admit([]byte(tc.payload))
			if err == nil {
				t.Fatalf("expected rejection for %s", tc.name)
			}
			rej, ok := err.(*RejectError)
			if !ok {
				t.Fatalf("expected *RejectError, got %T (%v)", err, err)
			}
			if rej.Status < 400 || rej.Status >= 500 {
				t.Fatalf("expected documented 4xx, got %d", rej.Status)
			}
		})
	}
}

func TestAdmissionAcceptsCodingTurns(t *testing.T) {
	ad := NewAdmission(config.RelayAdmissionConfig{})
	cases := []struct{ name, payload string }{
		{"code request", `{"messages":[{"role":"user","content":"refactor internal/relay/limiter.go to use a sliding window"}]}`},
		{"short continuation", `{"messages":[{"role":"user","content":"继续"}]}`},
		{"stack trace", `{"messages":[{"role":"user","content":"panic: runtime error in conductor.go, nil pointer when I call Admit — 帮我看下这个报错"}]}`},
		{"tool result block", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ad.Admit([]byte(tc.payload)); err != nil {
				t.Fatalf("expected admit for %s, got %v", tc.name, err)
			}
		})
	}
}

// ---------- T7 (docs 6.10): limiter — no spillover, strict serialization ----------

func testLimiterSet() *LimiterSet {
	quiet := false
	return NewLimiterSet(config.RelayConfig{
		QueueDepth: 1,
		Limits: config.RelayLimitsConfig{
			PlanCap5h:  1000,
			QuietHours: &quiet,
		},
	})
}

func TestLimiterConcurrencyOne(t *testing.T) {
	ls := testLimiterSet()
	l := ls.For("acct-1")

	release, err := l.Acquire(context.Background(), 1, "claude-sonnet-4-5")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = l.Acquire(ctx, 1, "claude-sonnet-4-5")
	qe, ok := err.(*QuotaError)
	if !ok {
		t.Fatalf("second acquire while one in flight: expected *QuotaError(429), got %T (%v)", err, err)
	}
	if qe.StatusCode() != 429 {
		t.Fatalf("expected 429, got %d", qe.StatusCode())
	}
}

func TestLimiterFixedBudgetExhaustion(t *testing.T) {
	ls := testLimiterSet()
	l := ls.For("acct-2")

	// Budget = seeded 25-35% of 1000 => at most 350. Burn through it.
	l.OnUsage("claude-sonnet-4-5", 0, 400, 0)

	_, err := l.Acquire(context.Background(), 100, "claude-sonnet-4-5")
	if _, ok := err.(*QuotaError); !ok {
		t.Fatalf("over-budget acquire: expected *QuotaError, got %T (%v)", err, err)
	}
}

func TestLimiterSeparateOpusBucket(t *testing.T) {
	quiet := false
	ls := NewLimiterSet(config.RelayConfig{
		QueueDepth: 1,
		Limits: config.RelayLimitsConfig{
			PlanCap5h:      100000,
			OpusPlanCap5h:  1000,
			QuietHours:     &quiet,
		},
	})
	l := ls.For("acct-3")
	l.OnUsage("claude-opus-4-1", 0, 400, 0) // exceeds 25-35% of 1000 opus cap
	// Opus bucket exhausted -> an Opus turn must 429...
	if _, err := l.Acquire(context.Background(), 100, "claude-opus-4-1"); err == nil {
		t.Fatal("opus turn must be denied once the separate opus budget is spent")
	}
	// ...while a Sonnet turn still admits (separate buckets, docs 6.8.5).
	release, err := l.Acquire(context.Background(), 100, "claude-sonnet-4-5")
	if err != nil {
		t.Fatalf("sonnet turn should still admit with separate opus bucket spent elsewhere: %v", err)
	}
	release()
}

func TestLimiterAccountsIndependent(t *testing.T) {
	ls := testLimiterSet()
	a := ls.For("acct-a")
	b := ls.For("acct-b")
	releaseA, err := a.Acquire(context.Background(), 1, "claude-sonnet-4-5")
	if err != nil {
		t.Fatalf("acct-a acquire: %v", err)
	}
	defer releaseA()
	// Same moment, different account must not be blocked by acct-a's in-flight turn.
	releaseB, err := b.Acquire(context.Background(), 1, "claude-sonnet-4-5")
	if err != nil {
		t.Fatalf("acct-b acquire must not be affected by acct-a in flight: %v", err)
	}
	releaseB()
}

// ---------- Store: 1:1 affinity + fail-closed ban authority (docs F8/F20) ----------

func TestStoreLifetimeBinding(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "store.json"))
	if err := s.BindTenant("t-1", "acct-1"); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if err := s.BindTenant("t-1", "acct-1"); err != nil {
		t.Fatalf("idempotent rebind: %v", err)
	}
	err := s.BindTenant("t-1", "acct-2")
	be, ok := err.(*BanError)
	if !ok || be.Status != 409 {
		t.Fatalf("binding to another account must 409 (no spillover), got %T (%v)", err, err)
	}

	// Persistence: reload from disk — binding survives (T6: affinity outlives a crash).
	s2 := NewStore(s.path)
	if err := s2.BindTenant("t-1", "acct-1"); err != nil {
		t.Fatalf("binding must survive store reload: %v", err)
	}
	if err := s2.BindTenant("t-1", "acct-2"); err == nil {
		t.Fatal("reloaded store forgot the lifetime binding")
	}
}

func TestStoreQuarantineFailClosedAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "store.json"))
	if err := s.Quarantine("acct-9", "test signature"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if err := s.CheckDispatch("acct-9"); err == nil {
		t.Fatal("quarantined account must be denied dispatch")
	}
	if err := s.CheckDispatch("acct-other"); err != nil {
		t.Fatalf("unrelated account must stay dispatchable: %v", err)
	}
	if err := s.Ban("acct-9", "confirmed"); err != nil {
		t.Fatalf("ban: %v", err)
	}
	if err := s.Reinstate("acct-9"); err == nil {
		t.Fatal("terminal ban must not be downgraded")
	}

	// Corrupt store => fail-closed everywhere (docs F20: stale/missing authority denies).
	bad := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(bad, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := NewStore(bad)
	if sc.Healthy() {
		t.Fatal("corrupt store must report unhealthy")
	}
	if err := sc.CheckDispatch("acct-x"); err == nil {
		t.Fatal("corrupt store must deny all dispatch (fail-closed)")
	}
	if err := sc.BindTenant("t", "a"); err == nil {
		t.Fatal("corrupt store must deny all binds (fail-closed)")
	}
}

func TestBanSignatureDetection(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"permission_error","message":"This credential is only authorized for use with Claude Code and cannot be used for other API requests."}}`)
	if _, hit := DetectBanSignature(body); !hit {
		t.Fatal("must detect the Claude-Code-only authorization body")
	}
	if _, hit := DetectBanSignature([]byte(`{"type":"error","error":{"message":"rate limited"}}`)); hit {
		t.Fatal("ordinary 429 body must not trip the ban detector")
	}
}

// ---------- Executor end-to-end against a fake local agent ----------

// fakeAgent implements the docs 6.7.2 surface: POST /v1/invoke streams relay_start ->
// Anthropic SSE -> relay_control; POST /cancel records cancellations.
type fakeAgent struct {
	srv       *httptest.Server
	cancelled atomic.Int32
	lastBody  atomic.Value
}

func newFakeAgent(t *testing.T) *fakeAgent {
	t.Helper()
	fa := &fakeAgent{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/invoke", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		defer r.Body.Close()
		_ = json.NewDecoder(r.Body).Decode(&body)
		fa.lastBody.Store(body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, "event: relay_start\ndata: {\"stream_id\":\"st-123\",\"agent_session_id\":\"sess-abc\"}\n\n")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-5\",\"content\":[],\"usage\":{\"input_tokens\":42,\"output_tokens\":1}}}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello \"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"world\"}}\n\n")
		fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":7}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		fmt.Fprint(w, "event: relay_control\ndata: {\"agent_session_id\":\"sess-abc\",\"usage\":{\"input_tokens\":42,\"output_tokens\":7,\"cache_read_input_tokens\":0}}\n\n")
		if fl != nil {
			fl.Flush()
		}
	})
	mux.HandleFunc("/cancel/", func(w http.ResponseWriter, r *http.Request) {
		fa.cancelled.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	fa.srv = httptest.NewServer(mux)
	t.Cleanup(fa.srv.Close)
	return fa
}

func testExecutor(t *testing.T, agentURL string) *Executor {
	t.Helper()
	cfg := &config.Config{AuthDir: t.TempDir()}
	quiet := false
	cfg.Relay = config.RelayConfig{
		Agents: map[string]string{"acct-1": strings.Replace(agentURL, "http://", "tcp://", 1)},
		Store: config.RelayStoreConfig{
			Driver: "file",
			Path:   filepath.Join(cfg.AuthDir, "relay-store.json"),
		},
		Limits: config.RelayLimitsConfig{
			PlanCap5h:  10_000_000,
			QuietHours: &quiet,
		},
	}
	return NewExecutor(cfg)
}

func claudeReqPayload() []byte {
	return []byte(`{"model":"claude-sonnet-4-5","max_tokens":1024,"messages":[{"role":"user","content":"fix the nil pointer in limiter.go"}]}`)
}

func TestExecutorRejectsNonLoopbackAgent(t *testing.T) {
	if _, err := NewAgentClient("a", "tcp://8.8.8.8:443"); err == nil {
		t.Fatal("non-loopback tcp endpoint must be refused (structural egress ban)")
	}
	if _, err := NewAgentClient("a", "http://example.com"); err == nil {
		t.Fatal("non-local scheme must be refused")
	}
}

func TestExecutorStreamPassthrough(t *testing.T) {
	fa := newFakeAgent(t)
	ex := testExecutor(t, fa.srv.URL)

	auth := testAuth("acct-1")
	req := cliproxyexecutor.Request{Model: "claude-sonnet-4-5", Payload: claudeReqPayload()}
	opts := cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FromString("claude")}

	res, err := ex.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	var got strings.Builder
	for chunk := range res.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	out := got.String()
	if !strings.Contains(out, "message_start") || !strings.Contains(out, "hello ") || !strings.Contains(out, "world") {
		t.Fatalf("stream must pass through Anthropic SSE, got: %q", out)
	}
	if !strings.Contains(out, "relay_control") {
		t.Fatalf("claude-surface tenant must receive the tail relay_control frame, got: %q", out)
	}
	// The agent must have received a real user message (only the last user turn).
	body, _ := fa.lastBody.Load().(map[string]any)
	msg, _ := body["message"].(map[string]any)
	if msg["role"] != "user" {
		t.Fatalf("agent invoke must inject a user message, got %v", body)
	}
	if body["session_key"] == nil || body["session_key"] == "" {
		t.Fatalf("session_key must be derived, got %v", body)
	}
}

func TestExecutorNonStreamAggregation(t *testing.T) {
	fa := newFakeAgent(t)
	ex := testExecutor(t, fa.srv.URL)

	auth := testAuth("acct-1")
	req := cliproxyexecutor.Request{Model: "claude-sonnet-4-5", Payload: claudeReqPayload()}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}

	resp, err := ex.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	payload := string(resp.Payload)
	if !strings.Contains(payload, "hello world") {
		t.Fatalf("aggregated message must contain concatenated text, got: %s", payload)
	}
	if !strings.Contains(payload, `"stop_reason":"end_turn"`) {
		t.Fatalf("aggregated message must carry stop_reason, got: %s", payload)
	}
	if !strings.Contains(payload, `"msg_1"`) {
		t.Fatalf("aggregated message must keep the upstream id, got: %s", payload)
	}
}

func TestExecutorAdmissionGateEndToEnd(t *testing.T) {
	fa := newFakeAgent(t)
	ex := testExecutor(t, fa.srv.URL)
	auth := testAuth("acct-1")
	req := cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: []byte(`{"model":"claude-sonnet-4-5","system":"You are a pirate","messages":[{"role":"user","content":"fix bug in x.go"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}
	if _, err := ex.ExecuteStream(context.Background(), auth, req, opts); err == nil {
		t.Fatal("system-carrying request must be rejected at admission")
	} else if rej, ok := err.(*RejectError); !ok || rej.Status != 400 {
		t.Fatalf("expected documented 400, got %T (%v)", err, err)
	}
}

func TestExecutorHttpRequestDisabled(t *testing.T) {
	// F3/6.6: the relay gateway holds no provider egress at all — HttpRequest is refused.
	ex := testExecutor(t, "http://127.0.0.1:1")
	if _, err := ex.HttpRequest(context.Background(), testAuth("acct-1"), nil); err == nil {
		t.Fatal("relay executor must refuse arbitrary provider HttpRequest (no egress)")
	}
}
