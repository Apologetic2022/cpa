package cursor

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"google.golang.org/protobuf/proto"
)

const availableModelsPath = "/aiserver.v1.AiService/AvailableModels"

// UnaryPOSTWithHeader performs a Cursor Connect unary RPC with raw
// application/proto body and returns the response headers (Set-Cookie).
func UnaryPOSTWithHeader(ctx context.Context, baseURL, path string, headers map[string]string, req proto.Message, resp proto.Message) (http.Header, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://api2.cursor.sh"
	}
	u, err := url.Parse(baseURL + path)
	if err != nil {
		return nil, err
	}
	body, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Content-Type", "application/proto")
	httpReq.Header.Set("Connect-Protocol-Version", "1")
	httpReq.Header.Set("Accept-Encoding", "gzip, br")
	httpReq.Proto = "HTTP/2.0"
	httpReq.ProtoMajor = 2

	transport := &http2.Transport{
		DialTLSContext: dialTLSViaEnvProxy,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"h2", "http/1.1"},
			ServerName: u.Hostname(),
		},
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = httpResp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, 8<<20))
	if err != nil {
		return httpResp.Header.Clone(), err
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return httpResp.Header.Clone(), fmt.Errorf("cursor unary %s HTTP %d: %s", path, httpResp.StatusCode, truncateForErr(raw, 512))
	}

	payload, err := decodeUnaryBody(raw, httpResp.Header)
	if err != nil {
		return httpResp.Header.Clone(), err
	}
	if err = proto.Unmarshal(payload, resp); err != nil {
		return httpResp.Header.Clone(), fmt.Errorf("cursor unary decode %s: %w", path, err)
	}
	return httpResp.Header.Clone(), nil
}

func decodeUnaryBody(raw []byte, hdr http.Header) ([]byte, error) {
	payload := raw
	encoding := strings.ToLower(strings.TrimSpace(hdr.Get("Content-Encoding")))
	if encoding == "" {
		encoding = strings.ToLower(strings.TrimSpace(hdr.Get("Connect-Content-Encoding")))
	}
	if encoding == "gzip" || (len(payload) >= 2 && payload[0] == 0x1f && payload[1] == 0x8b) {
		gr, err := gzip.NewReader(bytes.NewReader(payload))
		if err == nil {
			decompressed, errRead := io.ReadAll(io.LimitReader(gr, 16<<20))
			_ = gr.Close()
			if errRead == nil {
				payload = decompressed
			}
		}
	}

	ct := strings.ToLower(hdr.Get("Content-Type"))
	if strings.Contains(ct, "application/connect") || looksLikeConnectEnvelope(payload) {
		decoder := NewDecoder()
		envs, err := decoder.Feed(payload)
		if err != nil {
			return nil, err
		}
		for _, env := range envs {
			if env.EndStream() {
				continue
			}
			if len(env.Payload) > 0 {
				return env.Payload, nil
			}
		}
		return nil, fmt.Errorf("cursor unary: empty connect response")
	}
	return payload, nil
}

func looksLikeConnectEnvelope(b []byte) bool {
	return len(b) >= 5 && (b[0] == 0x00 || b[0] == 0x01 || b[0] == 0x02)
}

func truncateForErr(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
