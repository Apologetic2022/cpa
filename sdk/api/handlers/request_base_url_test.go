package handlers

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*http.Request)
		want    string
	}{
		{
			name:    "plain http",
			prepare: func(*http.Request) {},
			want:    "http://gw.example.com",
		},
		{
			name:    "direct tls",
			prepare: func(r *http.Request) { r.TLS = &tls.ConnectionState{} },
			want:    "https://gw.example.com",
		},
		{
			name: "behind a terminating reverse proxy",
			prepare: func(r *http.Request) {
				r.Header.Set("X-Forwarded-Proto", "https")
			},
			want: "https://gw.example.com",
		},
		{
			name: "forwarded chain uses the client-facing hop",
			prepare: func(r *http.Request) {
				r.Header.Set("X-Forwarded-Proto", "https, http")
				r.Header.Set("X-Forwarded-Host", "public.example.com, internal")
			},
			want: "https://public.example.com",
		},
		{
			name: "hostile host header is refused",
			prepare: func(r *http.Request) {
				r.Host = "gw.example.com/evil?x=1"
			},
			want: "",
		},
		{
			name: "unknown scheme is refused",
			prepare: func(r *http.Request) {
				r.Header.Set("X-Forwarded-Proto", "javascript")
			},
			want: "",
		},
		{
			name: "missing host is refused",
			prepare: func(r *http.Request) {
				r.Host = ""
			},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			req.Host = "gw.example.com"
			tc.prepare(req)
			if got := requestBaseURL(req); got != tc.want {
				t.Fatalf("requestBaseURL = %q, want %q", got, tc.want)
			}
		})
	}

	if got := requestBaseURL(nil); got != "" {
		t.Fatalf("nil request produced %q", got)
	}
}
