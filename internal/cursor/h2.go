package cursor

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/net/http2"
)

// BidiStream is a Connect bidi stream over HTTP/2.
type BidiStream struct {
	writer *io.PipeWriter
	body   io.ReadCloser
	status int
	hdr    http.Header
	closed bool
	mu     sync.Mutex
}

// OpenAgentRun opens POST /agent.v1.AgentService/Run and sends the first Connect frame.
// The server typically emits response headers after the initial AgentClientMessage.
func OpenAgentRun(ctx context.Context, baseURL string, headers map[string]string, firstPayload []byte) (*BidiStream, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://api2.cursor.sh"
	}
	u, err := url.Parse(baseURL + "/agent.v1.AgentService/Run")
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), pr)
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

	transport := &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			d := &tls.Dialer{Config: cfg}
			return d.DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"h2", "http/1.1"},
			ServerName: u.Hostname(),
		},
	}
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
			status: r.resp.StatusCode,
			hdr:    r.resp.Header.Clone(),
		}, nil
	}
}

// Status returns the HTTP status code.
func (s *BidiStream) Status() int { return s.status }

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
