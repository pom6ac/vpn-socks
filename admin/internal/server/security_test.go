package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostCheck(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedHosts = []string{"VPN.lan", "[fd00::1]"}
	h := newTestServerWith(t, cfg, &fakeDocker{}).Routes()

	tests := []struct {
		host string
		want int
	}{
		{"localhost:8080", 200}, // container healthcheck
		{"localhost:8090", 200},
		{"localhost", 200},
		{"127.0.0.1:8090", 200},
		{"[::1]:8090", 200},
		{"[::1]", 200},
		{"vpn.lan:8090", 200},
		{"VPN.LAN", 200},
		{"[fd00::1]:8090", 200},
		{"evil.example", http.StatusMisdirectedRequest},
		{"evil.example:8090", http.StatusMisdirectedRequest},
		{"127.0.0.2:8090", http.StatusMisdirectedRequest},
		{"localhost.evil.example", http.StatusMisdirectedRequest},
		{"", http.StatusMisdirectedRequest},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/healthz", nil)
			r.Host = tt.host
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Errorf("Host %q: code = %d, want %d", tt.host, w.Code, tt.want)
			}
		})
	}
}

func post(h http.Handler, target string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", target, nil)
	r.Host = "127.0.0.1:8090"
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestCSRF(t *testing.T) {
	s, d := newTestServer(t)
	h := s.Routes()

	tests := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no header", nil, http.StatusForbidden},
		{"wrong header value", map[string]string{csrfHeader: "XMLHttpRequest"}, http.StatusForbidden},
		{"same-origin with header", map[string]string{csrfHeader: csrfValue, "Sec-Fetch-Site": "same-origin"}, 200},
		{"non-browser client with header", map[string]string{csrfHeader: csrfValue}, 200},
		{"cross-site fetch", map[string]string{csrfHeader: csrfValue, "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site other origin", map[string]string{csrfHeader: csrfValue, "Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"foreign Origin", map[string]string{csrfHeader: csrfValue, "Origin": "http://evil.example"}, http.StatusForbidden},
		{"matching Origin", map[string]string{csrfHeader: csrfValue, "Origin": "http://127.0.0.1:8090"}, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d.restarted = nil
			w := post(h, "/api/restart/vpn-ovpn", tt.headers)
			if w.Code != tt.want {
				t.Fatalf("code = %d, want %d (%s)", w.Code, tt.want, w.Body.String())
			}
			if (len(d.restarted) == 1) != (tt.want == 200) {
				t.Errorf("restarted = %v", d.restarted)
			}
		})
	}
}

func TestCSRF_GetNeedsNoHeader(t *testing.T) {
	s, _ := newTestServer(t)
	r := httptest.NewRequest("GET", "/api/health", nil)
	r.Host = "localhost"
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("code = %d", w.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Routes()
	for _, target := range []string{"/", "/api/health", "/static/app.js"} {
		w := do(t, h, "GET", target)
		if got := w.Header().Get("Content-Security-Policy"); got != "default-src 'self'; frame-ancestors 'none'" {
			t.Errorf("%s CSP = %q", target, got)
		}
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s nosniff = %q", target, got)
		}
		if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("%s Referrer-Policy = %q", target, got)
		}
	}
	// Also on rejected requests.
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "evil.example"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Header().Get("Content-Security-Policy") == "" {
		t.Error("CSP missing on 421 response")
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"LocalHost:8080": "localhost",
		"[::1]:80":       "::1",
		"[::1]":          "::1",
		" 127.0.0.1 ":    "127.0.0.1",
		"example.com":    "example.com",
	} {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func newTokenServer(t *testing.T) http.Handler {
	t.Helper()
	f := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(f, []byte("s3cret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.AdminTokenFile = f
	return newTestServerWith(t, cfg, &fakeDocker{}).Routes()
}

func TestToken_RequiredForAPI(t *testing.T) {
	h := newTokenServer(t)

	get := func(target string, hdr map[string]string, cookies ...*http.Cookie) int {
		r := httptest.NewRequest("GET", target, nil)
		r.Host = "localhost"
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	if c := get("/api/health", nil); c != http.StatusUnauthorized {
		t.Errorf("no token: %d", c)
	}
	if c := get("/api/health", map[string]string{"Authorization": "Bearer wrong"}); c != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", c)
	}
	if c := get("/api/health", map[string]string{"Authorization": "Basic s3cret-token"}); c != http.StatusUnauthorized {
		t.Errorf("non-bearer auth: %d", c)
	}
	if c := get("/api/health", map[string]string{"Authorization": "Bearer s3cret-token"}); c != 200 {
		t.Errorf("bearer: %d", c)
	}
	if c := get("/api/health", nil, &http.Cookie{Name: tokenCookie, Value: "s3cret-token"}); c != 200 {
		t.Errorf("cookie: %d", c)
	}
	// UI shell and container healthcheck stay public.
	if c := get("/healthz", nil); c != 200 {
		t.Errorf("healthz: %d", c)
	}
	if c := get("/", nil); c != 200 {
		t.Errorf("index: %d", c)
	}
}

func login(h http.Handler, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/login", bytes.NewBufferString(body))
	r.Host = "localhost"
	r.Header.Set(csrfHeader, csrfValue)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestToken_Login(t *testing.T) {
	h := newTokenServer(t)

	if w := login(h, `{"token":"nope"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("bad token login: %d", w.Code)
	}
	if w := login(h, `not json`); w.Code != http.StatusBadRequest {
		t.Errorf("bad json login: %d", w.Code)
	}
	w := login(h, `{"token":" s3cret-token "}`)
	if w.Code != 200 {
		t.Fatalf("login: %d", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != tokenCookie || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %+v", cookies)
	}
	r := httptest.NewRequest("GET", "/api/status", nil)
	r.Host = "localhost"
	r.AddCookie(cookies[0])
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	if rw.Code != 200 {
		t.Errorf("status with cookie: %d", rw.Code)
	}
}

func TestLogin_AuthDisabled(t *testing.T) {
	s, _ := newTestServer(t)
	w := login(s.Routes(), `{}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "auth disabled") {
		t.Errorf("got %d %q", w.Code, w.Body.String())
	}
}

func TestLoggingMiddleware_RecordsStatus(t *testing.T) {
	h := loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		w.WriteHeader(http.StatusOK) // ignored second call
		_, _ = w.Write([]byte("x"))
		w.(http.Flusher).Flush()
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != http.StatusTeapot || !w.Flushed {
		t.Errorf("code = %d flushed = %v", w.Code, w.Flushed)
	}
	rec := &statusRecorder{ResponseWriter: w}
	if rec.Unwrap() != w {
		t.Error("Unwrap must return the wrapped writer")
	}
}

// noFlushWriter is a ResponseWriter without Flush.
type noFlushWriter struct{ http.ResponseWriter }

func TestStatusRecorder_FlushWithoutFlusher(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: noFlushWriter{httptest.NewRecorder()}}
	rec.Flush() // must not panic
}
