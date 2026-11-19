package server

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	// csrfHeader must accompany every state-changing request. A cross-site
	// form cannot set it and a cross-origin fetch with it needs a CORS
	// preflight, which the panel never answers.
	csrfHeader = "X-Requested-With"
	csrfValue  = "vpn-admin"

	tokenCookie = "vpn_admin_token" //nolint:gosec // G101: cookie name, not a credential
)

// loggingMiddleware logs state-changing requests and failures with status
// and duration. Successful GETs (the UI polls every second) are not logged.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.Method != http.MethodGet || rec.status >= 400 {
			//nolint:gosec // G706: the path is %q-quoted, so it cannot inject log lines.
			log.Printf("http %s %q -> %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
		}
	})
}

// statusRecorder captures the response status. Unwrap keeps
// http.ResponseController (flush, write deadlines) working through it.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// securityHeaders sets a strict CSP and related headers on every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// normalizeHost returns the lower-cased hostname of a Host header value,
// without port or IPv6 brackets.
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
}

// hostMiddleware rejects requests whose Host is not a loopback name or an
// explicitly allowed host, defeating DNS rebinding.
func (s *Server) hostMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedHosts[normalizeHost(r.Host)] {
			http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrfMiddleware protects state-changing requests: they must carry the
// custom header and pass Go's Fetch-metadata/Origin based cross-origin check.
func csrfMiddleware(next http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	protected := cop.Handler(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead &&
			r.Header.Get(csrfHeader) != csrfValue {
			http.Error(w, "missing "+csrfHeader+" header", http.StatusForbidden)
			return
		}
		protected.ServeHTTP(w, r)
	})
}

// authMiddleware enforces the optional admin token on /api/* (except login).
// The token is accepted as a Bearer header or as the cookie set by login.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" || !strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api/login" {
			next.ServeHTTP(w, r)
			return
		}
		if !s.validToken(requestToken(r)) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requestToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		if tok, ok := strings.CutPrefix(auth, "Bearer "); ok {
			return strings.TrimSpace(tok)
		}
		return ""
	}
	if c, err := r.Cookie(tokenCookie); err == nil {
		return c.Value
	}
	return ""
}

func (s *Server) validToken(tok string) bool {
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) == 1
}

// handleLogin exchanges a token typed into the UI for an HttpOnly cookie, so
// that EventSource (which cannot send headers) is authorised too.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.token == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "auth disabled"})
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if !s.validToken(strings.TrimSpace(req.Token)) {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	// Secure is set only over TLS: the panel is normally served over plain
	// HTTP on loopback, where a Secure cookie would not be stored.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: see above
		Name:     tokenCookie,
		Value:    s.token,
		Path:     "/api/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
