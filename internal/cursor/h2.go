package cursor

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
)

// BidiStream is a Connect bidi stream over HTTP/2.
type BidiStream struct {
	writer *io.PipeWriter
	body   io.ReadCloser
	hdr    http.Header
	closed bool
	mu     sync.Mutex
}

var (
	sharedH2Once      sync.Once
	sharedH2Transport *http2.Transport
)

// agentTransport returns the HTTP/2 transport for Agent runs. By default all
// runs share one transport so consecutive turns of a conversation ride the
// same TCP+TLS connection, the way the desktop client keeps a single
// connection open for its whole session. CPA_CURSOR_SHARED_CONN=0 restores
// the previous one-connection-per-run behaviour for A/B comparison.
func agentTransport(serverName string) *http2.Transport {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPA_CURSOR_SHARED_CONN"))) {
	case "0", "false", "off", "no":
		return &http2.Transport{
			DialTLSContext: dialTLSViaEnvProxy,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				NextProtos: []string{"h2", "http/1.1"},
				ServerName: serverName,
			},
		}
	}
	sharedH2Once.Do(func() {
		sharedH2Transport = &http2.Transport{
			DialTLSContext: dialTLSViaEnvProxy,
			// ServerName is left empty: the transport fills it per host, so
			// one shared transport serves every configured base URL.
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				NextProtos: []string{"h2", "http/1.1"},
			},
			// Idle pooled connections are health-checked with h2 pings so a
			// tunnel dropped by a proxy or LB is pruned instead of failing
			// the next run.
			ReadIdleTimeout: 30 * time.Second,
			PingTimeout:     15 * time.Second,
		}
	})
	return sharedH2Transport
}

// OpenAgentRun opens POST /agent.v1.AgentService/Run and sends the first Connect frame.
// The server typically emits response headers after the initial AgentClientMessage.
// The shared transport reuses one connection across runs; a pooled connection
// can have gone stale between turns, so a transport-level open failure is
// retried once on a fresh connection before it is reported.
func OpenAgentRun(ctx context.Context, baseURL string, headers map[string]string, firstPayload []byte) (*BidiStream, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://api2.cursor.sh"
	}
	u, err := url.Parse(baseURL + "/agent.v1.AgentService/Run")
	if err != nil {
		return nil, err
	}
	transport := agentTransport(u.Hostname())
	stream, err := openAgentRunOnce(ctx, transport, u.String(), headers, firstPayload)
	if err != nil && transport == sharedH2Transport && ctx.Err() == nil {
		transport.CloseIdleConnections()
		log.Debugf("cursor h2 open failed on pooled connection, retrying fresh: %v", err)
		stream, err = openAgentRunOnce(ctx, transport, u.String(), headers, firstPayload)
	}
	return stream, err
}

func openAgentRunOnce(ctx context.Context, transport *http2.Transport, runURL string, headers map[string]string, firstPayload []byte) (*BidiStream, error) {
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, runURL, pr)
	if err != nil {
		_ = pw.Close()
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Proto = "HTTP/2.0"
	req.ProtoMajor = 2

	client := &http.Client{Transport: transport}

	type roundTripResult struct {
		resp *http.Response
		err  error
	}
	ch := make(chan roundTripResult, 1)
	go func() {
		resp, errDo := client.Do(req)
		ch <- roundTripResult{resp: resp, err: errDo}
	}()

	if _, err = pw.Write(EncodeEnvelope(firstPayload, false)); err != nil {
		_ = pw.CloseWithError(err)
		return nil, fmt.Errorf("cursor h2 write first frame: %w", err)
	}

	select {
	case <-ctx.Done():
		_ = pw.CloseWithError(ctx.Err())
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			_ = pw.CloseWithError(r.err)
			return nil, fmt.Errorf("cursor h2 open: %w", r.err)
		}
		if r.resp.StatusCode < 200 || r.resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(r.resp.Body, 4096))
			_ = pw.Close()
			_ = r.resp.Body.Close()
			return nil, fmt.Errorf("cursor agent run HTTP %d: %s", r.resp.StatusCode, string(b))
		}
		return &BidiStream{
			writer: pw,
			body:   r.resp.Body,
			hdr:    r.resp.Header.Clone(),
		}, nil
	}
}

// ResponseHeader returns the response headers from the Agent Run open.
func (s *BidiStream) ResponseHeader() http.Header {
	if s == nil {
		return nil
	}
	return s.hdr
}

// WriteEnvelope writes one Connect envelope to the request body.
func (s *BidiStream) WriteEnvelope(payload []byte, endStream bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("cursor h2: stream closed")
	}
	_, err := s.writer.Write(EncodeEnvelope(payload, endStream))
	return err
}

// Read reads raw bytes from the response body.
func (s *BidiStream) Read(p []byte) (int, error) {
	if s.body == nil {
		return 0, io.EOF
	}
	return s.body.Read(p)
}

// Close closes both sides of the bidi stream.
func (s *BidiStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	_ = s.writer.Close()
	if s.body != nil {
		return s.body.Close()
	}
	return nil
}
