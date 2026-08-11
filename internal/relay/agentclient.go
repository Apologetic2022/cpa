package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AgentClient speaks the gateway<->instance contract (docs 6.7) with ONE account's
// Claude Code agent:
//
//	POST /v1/invoke        tenant turn injected as a real user message; SSE back,
//	                       framed as: relay_start -> Anthropic events -> relay_control
//	POST /cancel/{stream}  tenant disconnect aborts upstream generation (F11, mandatory)
//	GET  /status           session/load/health
//	GET  /healthz          process liveness (not budgeted)
//
// The data plane is plaintext HTTP/1.1 + SSE over a per-account local socket (UDS in
// the fleet; loopback TCP tolerated for dev). The client is structurally incapable of
// reaching Anthropic: it dials only the configured unix socket or an explicit loopback
// IP literal — no DNS, no TLS, no proxy, no hostnames.
type AgentClient struct {
	account  string
	endpoint string
	hc       *http.Client
}

// NewAgentClient builds a client for endpoint `unix:///path/to.sock` or
// `tcp://127.0.0.1:port`. Anything else fails closed at construction.
func NewAgentClient(accountID, endpoint string) (*AgentClient, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return nil, fmt.Errorf("relay agent endpoint %q unparsable: %w", endpoint, err)
	}
	var dial func(ctx context.Context, _, _ string) (net.Conn, error)
	switch strings.ToLower(u.Scheme) {
	case "unix":
		sockPath := u.Path
		if sockPath == "" {
			return nil, fmt.Errorf("relay agent endpoint %q missing socket path", endpoint)
		}
		d := &net.Dialer{Timeout: 10 * time.Second}
		dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", sockPath)
		}
	case "tcp":
		host := u.Hostname()
		port := u.Port()
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("relay agent tcp endpoint %q refused: loopback IP literal only (never DNS, never remote)", endpoint)
		}
		if port == "" {
			return nil, fmt.Errorf("relay agent tcp endpoint %q missing port", endpoint)
		}
		addr := net.JoinHostPort(host, port)
		d := &net.Dialer{Timeout: 10 * time.Second}
		dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp", addr)
		}
	default:
		return nil, fmt.Errorf("relay agent endpoint %q refused: only unix:// or tcp:// (loopback) are valid local channels", endpoint)
	}

	transport := &http.Transport{
		DialContext:           dial,
		ForceAttemptHTTP2:     false, // HTTP/1.1 only, per contract
		DisableKeepAlives:     true,  // docs H5: no connection pooling toward agents
		MaxIdleConns:          -1,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	return &AgentClient{
		account:  accountID,
		endpoint: endpoint,
		hc:       &http.Client{Transport: transport},
	}, nil
}

// InvokeRequest is the POST /v1/invoke body (docs 6.7.2).
type InvokeRequest struct {
	SessionKey string          `json:"session_key"`
	Model      string          `json:"model,omitempty"`
	Betas      []string        `json:"betas,omitempty"`
	Message    json.RawMessage `json:"message"`
}

// RelayStart is the mandatory first SSE frame, carrying the stream ID the gateway needs
// for the cancel contract before any content flows.
type RelayStart struct {
	StreamID       string `json:"stream_id"`
	AgentSessionID string `json:"agent_session_id,omitempty"`
}

// RelayControl is the trailing SSE frame (docs 6.7.2): usage for metering, ratelimit
// for reconciliation, session identity for observability.
type RelayControl struct {
	AgentSessionID string         `json:"agent_session_id,omitempty"`
	Usage          *RelayUsage    `json:"usage,omitempty"`
	Ratelimit      map[string]any `json:"ratelimit,omitempty"`
	Aborted        bool           `json:"aborted,omitempty"`
}

// RelayUsage mirrors Anthropic usage accounting.
type RelayUsage struct {
	InputTokens          int64 `json:"input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
}

// SSEEvent is one parsed event-stream frame.
type SSEEvent struct {
	Event string
	Data  []byte
}

// InvokeStream is an in-flight agent turn: SSE events flow on Events; Err reports the
// terminal scan error (if any) after the channel closes.
type InvokeStream struct {
	Events <-chan SSEEvent

	done chan error
	body io.Closer
}

// Err blocks until the stream drains and returns the terminal error, if any.
func (s *InvokeStream) Err() error { return <-s.done }

// Invoke POSTs one tenant turn. The POST and status check happen synchronously so a
// dead/unreachable agent fails fast (letting the manager's cooldown FSM react); SSE
// scanning then proceeds in the background. The stream aborts when ctx is done.
func (c *AgentClient) Invoke(ctx context.Context, reqBody InvokeRequest) (*InvokeStream, error) {
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal invoke request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://relay.local/v1/invoke", strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("build invoke request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent invoke transport: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		return nil, &AgentHTTPError{Status: resp.StatusCode, Body: body}
	}

	events := make(chan SSEEvent, 32)
	stream := &InvokeStream{Events: events, done: make(chan error, 1), body: resp.Body}
	go func() {
		defer close(events)
		defer resp.Body.Close()
		err := scanSSE(resp.Body, events)
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		stream.done <- err
		close(stream.done)
	}()
	return stream, nil
}

// AgentHTTPError carries a non-2xx agent response (body retained for ban scanning).
type AgentHTTPError struct {
	Status int
	Body   []byte
}

func (e *AgentHTTPError) Error() string {
	return fmt.Sprintf("agent returned HTTP %d: %s", e.Status, truncate(string(e.Body), 300))
}

// StatusCode implements the executor StatusError contract.
func (e *AgentHTTPError) StatusCode() int { return e.Status }

// Cancel aborts an in-flight turn (docs 6.7.2/F11): tenant disconnect MUST abort the
// upstream generation. Best-effort with a short timeout.
func (c *AgentClient) Cancel(streamID string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://relay.local/cancel/"+url.PathEscape(streamID), nil)
	if err != nil {
		return
	}
	resp, err := c.hc.Do(req)
	if err == nil && resp != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}
}

// Healthz checks agent liveness (not budgeted).
func (c *AgentClient) Healthz(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://relay.local/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent healthz: HTTP %d", resp.StatusCode)
	}
	return nil
}

// scanSSE reads an event-stream body and emits parsed frames. It returns the scanner's
// terminal error, if any.
func scanSSE(body io.Reader, out chan<- SSEEvent) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var event string
	var data strings.Builder
	flush := func() bool {
		if data.Len() == 0 && event == "" {
			return true
		}
		ev := event
		if ev == "" {
			ev = "message"
		}
		payload := strings.TrimSuffix(data.String(), "\n")
		select {
		case out <- SSEEvent{Event: ev, Data: []byte(payload)}:
		}
		event, data = "", strings.Builder{}
		return true
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// comment / keep-alive
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			data.WriteByte('\n')
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
