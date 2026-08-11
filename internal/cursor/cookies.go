package cursor

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// CookieJar stores upstream Set-Cookie values keyed by host (desktop parity).
type CookieJar struct {
	mu     sync.Mutex
	byHost map[string]map[string]string
}

var accountCookieJars sync.Map // account key -> *CookieJar

// CookieJarForAccount returns a process-wide jar for one Cursor account identity.
func CookieJarForAccount(key string) *CookieJar {
	key = strings.TrimSpace(key)
	if key == "" {
		key = "anonymous"
	}
	if v, ok := accountCookieJars.Load(key); ok {
		return v.(*CookieJar)
	}
	jar := &CookieJar{byHost: map[string]map[string]string{}}
	actual, _ := accountCookieJars.LoadOrStore(key, jar)
	return actual.(*CookieJar)
}

func cookieHost(baseURL string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Hostname() == "" {
		return strings.ToLower(strings.TrimSpace(baseURL))
	}
	return strings.ToLower(u.Hostname())
}

// Header returns the Cookie header for baseURL, or empty when the jar has none.
func (j *CookieJar) Header(baseURL string) string {
	if j == nil {
		return ""
	}
	host := cookieHost(baseURL)
	j.mu.Lock()
	defer j.mu.Unlock()
	cookies := j.byHost[host]
	if len(cookies) == 0 {
		return ""
	}
	parts := make([]string, 0, len(cookies))
	for name, value := range cookies {
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "; ")
}

// RememberResponse merges Set-Cookie values from an upstream response.
func (j *CookieJar) RememberResponse(baseURL string, hdr http.Header) {
	if j == nil || hdr == nil {
		return
	}
	values := hdr.Values("Set-Cookie")
	if len(values) == 0 {
		if single := hdr.Get("Set-Cookie"); single != "" {
			values = []string{single}
		}
	}
	if len(values) == 0 {
		return
	}
	host := cookieHost(baseURL)
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.byHost == nil {
		j.byHost = map[string]map[string]string{}
	}
	bucket := j.byHost[host]
	if bucket == nil {
		bucket = map[string]string{}
		j.byHost[host] = bucket
	}
	for _, raw := range values {
		name, value, deleted := parseSetCookiePair(raw)
		if name == "" {
			continue
		}
		if deleted {
			delete(bucket, name)
			continue
		}
		bucket[name] = value
	}
	if len(bucket) == 0 {
		delete(j.byHost, host)
	}
}

func parseSetCookiePair(raw string) (name, value string, deleted bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	parts := strings.Split(raw, ";")
	nv := strings.TrimSpace(parts[0])
	eq := strings.IndexByte(nv, '=')
	if eq <= 0 {
		return "", "", false
	}
	name = strings.TrimSpace(nv[:eq])
	value = strings.TrimSpace(nv[eq+1:])
	if name == "" {
		return "", "", false
	}
	if value == "" {
		return name, "", true
	}
	for _, attr := range parts[1:] {
		attr = strings.TrimSpace(attr)
		lower := strings.ToLower(attr)
		if lower == "max-age=0" {
			return name, value, true
		}
		if strings.HasPrefix(lower, "expires=") {
			expiresRaw := strings.TrimSpace(attr[len("expires="):])
			if t, err := http.ParseTime(expiresRaw); err == nil && !t.After(time.Now().UTC()) {
				return name, value, true
			}
		}
	}
	return name, value, false
}

func transportCursorCookie(accessToken string) string {
	return "CursorCookie=Cookie-" + accessTokenPrefix(accessToken)
}
