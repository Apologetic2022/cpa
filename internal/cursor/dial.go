package cursor

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
)

// dialTLSViaEnvProxy dials addr for TLS, honoring HTTPS_PROXY/HTTP_PROXY/NO_PROXY
// via an HTTP CONNECT tunnel. Egress-guarded deployments kernel-block direct
// gateway egress and route Cursor traffic through a loopback CONNECT proxy.
func dialTLSViaEnvProxy(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
	proxyURL, err := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
	if err != nil {
		return nil, fmt.Errorf("cursor proxy resolve: %w", err)
	}
	if proxyURL == nil {
		d := &tls.Dialer{Config: cfg}
		return d.DialContext(ctx, network, addr)
	}

	proxyAddr := proxyURL.Host
	if proxyURL.Port() == "" {
		port := "80"
		if proxyURL.Scheme == "https" {
			port = "443"
		}
		proxyAddr = net.JoinHostPort(proxyURL.Hostname(), port)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("cursor proxy dial %s: %w", proxyAddr, err)
	}

	connectReq := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: http.Header{},
	}
	if user := proxyURL.User; user != nil {
		pass, _ := user.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(user.Username() + ":" + pass))
		connectReq.Header.Set("Proxy-Authorization", "Basic "+cred)
	}
	if err = connectReq.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("cursor proxy connect write: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("cursor proxy connect read: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("cursor proxy connect: HTTP %d", resp.StatusCode)
	}
	if br.Buffered() > 0 {
		// CONNECT success must not carry payload; leftover bytes would corrupt TLS.
		_ = conn.Close()
		return nil, fmt.Errorf("cursor proxy connect: unexpected buffered data after response")
	}

	tlsConn := tls.Client(conn, cfg)
	if err = tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("cursor proxy tls handshake: %w", err)
	}
	return tlsConn, nil
}
