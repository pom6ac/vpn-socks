package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pom6ac/vpn-socks/admin/internal/config"
	"github.com/pom6ac/vpn-socks/admin/internal/dockerapi"
	"github.com/pom6ac/vpn-socks/admin/internal/health"
	"github.com/pom6ac/vpn-socks/admin/internal/storage"
	"github.com/pom6ac/vpn-socks/admin/internal/topology"
	"github.com/pom6ac/vpn-socks/admin/internal/traffic"
)

// fakeDocker is an in-memory Docker implementation.
type fakeDocker struct {
	mu         sync.Mutex
	err        error
	containers map[string]dockerapi.ContainerInfo
	restarted  []string
	signalled  []string
	logs       func() io.ReadCloser
	logsSince  time.Time
}

func (f *fakeDocker) ListContainers(ctx context.Context, names []string) ([]dockerapi.ContainerInfo, error) {
	var out []dockerapi.ContainerInfo
	for _, n := range names {
		c, err := f.InspectContainer(ctx, n)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeDocker) InspectContainer(_ context.Context, name string) (dockerapi.ContainerInfo, error) {
	if f.err != nil {
		return dockerapi.ContainerInfo{}, f.err
	}
	c, ok := f.containers[name]
	if !ok {
		return c, errors.New("no such container")
	}
	return c, nil
}

func (f *fakeDocker) RestartContainer(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarted = append(f.restarted, name)
	return f.err
}

func (f *fakeDocker) SignalContainer(_ context.Context, name, sig string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signalled = append(f.signalled, name+":"+sig)
	return f.err
}

func (f *fakeDocker) LogsStream(ctx context.Context, _ string, since time.Time) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	f.logsSince = since
	f.mu.Unlock()
	if f.logs == nil {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	rc := f.logs()
	// Like the real client, cancelling the request ends the stream.
	stop := context.AfterFunc(ctx, func() { _ = rc.Close() })
	return &stopCloser{ReadCloser: rc, stop: stop}, nil
}

type stopCloser struct {
	io.ReadCloser
	stop func() bool
}

func (s *stopCloser) Close() error {
	s.stop()
	return s.ReadCloser.Close()
}

func testConfig() config.Config {
	return config.Config{
		ListenAddr:     ":8080",
		OVPNContainer:  "vpn-ovpn",
		SOCKSContainer: "vpn-socks",
		AdminContainer: "vpn-admin",
		ContainerNames: []string{"vpn-ovpn", "vpn-socks"},
		ComposeProject: "vpn-socks",
		HostProjectDir: "/srv/vpn-socks",
	}
}

func newTestServerWith(t *testing.T, cfg config.Config, d *fakeDocker) *Server {
	t.Helper()
	store, err := storage.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := New(cfg, d, health.NewProber("", nil, "", "", "", nil, "", nil), traffic.NewReader(nil, cfg.OVPNContainer), store, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func newTestServer(t *testing.T) (*Server, *fakeDocker) {
	t.Helper()
	d := &fakeDocker{containers: map[string]dockerapi.ContainerInfo{
		"vpn-ovpn":  {Name: "vpn-ovpn", State: "running"},
		"vpn-socks": {Name: "vpn-socks", State: "running", NetworkMode: "container:abc"},
		"vpn-admin": {Name: "vpn-admin", State: "running"},
	}}
	return newTestServerWith(t, testConfig(), d), d
}

// do sends a request through the full middleware chain as a same-origin
// browser on localhost would.
func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Host = "localhost:8090"
	if method != http.MethodGet {
		r.Header.Set(csrfHeader, csrfValue)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("unmarshal %q: %v", w.Body.String(), err)
	}
	return v
}

func TestHealthz(t *testing.T) {
	s, _ := newTestServer(t)
	w := do(t, s.Routes(), "GET", "/healthz")
	if w.Code != 200 || w.Body.String() != "ok" {
		t.Errorf("got %d %q", w.Code, w.Body.String())
	}
}

func TestIndex(t *testing.T) {
	s, _ := newTestServer(t)
	w := do(t, s.Routes(), "GET", "/")
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("got %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	if strings.Contains(body, "htmx") || strings.Contains(body, "rebuild") {
		t.Error("index must not reference htmx or rebuild")
	}
	if strings.Contains(body, "<script>") || strings.Contains(body, "style=") || strings.Contains(body, "onclick") {
		t.Error("index must not contain inline scripts/styles (CSP)")
	}
}

func TestIndex_NotFound(t *testing.T) {
	s, _ := newTestServer(t)
	if w := do(t, s.Routes(), "GET", "/nonexistent"); w.Code != 404 {
		t.Errorf("code = %d", w.Code)
	}
}

func TestStatic(t *testing.T) {
	s, _ := newTestServer(t)
	for _, f := range []string{"style.css", "app.js", "chart.min.js"} {
		if w := do(t, s.Routes(), "GET", "/static/"+f); w.Code != 200 {
			t.Errorf("%s: code = %d", f, w.Code)
		}
	}
	if w := do(t, s.Routes(), "GET", "/static/htmx.min.js"); w.Code != 404 {
		t.Errorf("htmx.min.js must be gone, code = %d", w.Code)
	}
}

func TestConfigEndpoint(t *testing.T) {
	s, _ := newTestServer(t)
	w := do(t, s.Routes(), "GET", "/api/config")
	got := decode[configResponse](t, w)
	if got.OVPN != "vpn-ovpn" || got.SOCKS != "vpn-socks" {
		t.Errorf("config = %+v", got)
	}
}

func TestHealth(t *testing.T) {
	s, _ := newTestServer(t)
	w := do(t, s.Routes(), "GET", "/api/health")
	if r := decode[health.Report](t, w); r.Overall != health.StatusDown {
		t.Errorf("overall = %q", r.Overall)
	}
}

func TestTraffic(t *testing.T) {
	s, _ := newTestServer(t)
	w := do(t, s.Routes(), "GET", "/api/metrics/traffic")
	if sample := decode[traffic.Sample](t, w); sample.OK {
		t.Error("expected OK=false for new reader")
	}
}

func TestStatus(t *testing.T) {
	s, _ := newTestServer(t)
	w := do(t, s.Routes(), "GET", "/api/status")
	resp := decode[statusResponse](t, w)
	if resp.DockerAPIUnavailable || len(resp.Containers) != 2 || resp.Containers[0].Name != "vpn-ovpn" {
		t.Errorf("status = %+v", resp)
	}
}

func TestStatus_DockerUnavailable(t *testing.T) {
	s, d := newTestServer(t)
	d.err = errors.New("dial unix /var/run/docker.sock: connect: no such file")
	w := do(t, s.Routes(), "GET", "/api/status")
	if resp := decode[statusResponse](t, w); w.Code != 200 || !resp.DockerAPIUnavailable {
		t.Errorf("got %d %+v", w.Code, resp)
	}
	if strings.Contains(w.Body.String(), "docker.sock") {
		t.Error("internal error leaked to client")
	}
}

func TestHistory(t *testing.T) {
	s, _ := newTestServer(t)
	for _, v := range []float64{100, 200} {
		if err := s.store.WriteMetric("rx_bytes", v); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{"", "?kind=rx_bytes&range=24h", "?range=1h"} {
		w := do(t, s.Routes(), "GET", "/api/metrics/history"+q)
		points := decode[[]storage.HistoryPoint](t, w)
		if w.Code != 200 || len(points) == 0 {
			t.Errorf("%q: got %d %v", q, w.Code, points)
		}
	}
	w := do(t, s.Routes(), "GET", "/api/metrics/history?kind=none")
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("empty history = %q, want []", w.Body.String())
	}
}

func TestHistory_ErrorIsGeneric(t *testing.T) {
	s, _ := newTestServer(t)
	_ = s.store.Close()
	w := do(t, s.Routes(), "GET", "/api/metrics/history")
	resp := decode[map[string]string](t, w)
	if w.Code != 500 || resp["error"] != "history unavailable" {
		t.Errorf("got %d %v", w.Code, resp)
	}
}

func TestTopology(t *testing.T) {
	s, _ := newTestServer(t)
	w := do(t, s.Routes(), "GET", "/api/topology")
	v := decode[topology.View](t, w)
	if len(v.Paths) != 2 {
		t.Fatalf("paths = %+v", v.Paths)
	}
	nodes := v.Paths[0].Nodes
	if nodes[1].Title != "vpn-socks" || nodes[1].Status != topology.Up {
		t.Errorf("socks node = %+v", nodes[1])
	}
	if nodes[2].Title != "vpn-ovpn" {
		t.Errorf("ovpn node = %+v", nodes[2])
	}
	if v.Paths[1].Nodes[1].Title != "vpn-admin" {
		t.Errorf("admin node = %+v", v.Paths[1].Nodes[1])
	}
}

func TestTopology_CustomNamesAndCollector(t *testing.T) {
	cfg := testConfig()
	cfg.OVPNContainer, cfg.SOCKSContainer = "my-ovpn", "my-socks"
	cfg.ContainerNames = []string{"my-ovpn", "my-socks"}
	d := &fakeDocker{containers: map[string]dockerapi.ContainerInfo{
		"my-ovpn": {State: "exited"},
	}}
	s := newTestServerWith(t, cfg, d)
	s.topo = topology.NewCollector(nil, "my-ovpn", "/vpn/config.ovpn", "127.0.0.1:1", nil)
	w := do(t, s.Routes(), "GET", "/api/topology")
	v := decode[topology.View](t, w)
	nodes := v.Paths[0].Nodes
	if nodes[1].Title != "my-socks" || nodes[1].Status != topology.Unknown {
		t.Errorf("socks node = %+v", nodes[1])
	}
	if nodes[2].Title != "my-ovpn" || nodes[2].Status != topology.Down {
		t.Errorf("ovpn node = %+v", nodes[2])
	}
}

func TestRestart(t *testing.T) {
	s, d := newTestServer(t)
	w := do(t, s.Routes(), "POST", "/api/restart/vpn-socks")
	if w.Code != 200 || len(d.restarted) != 1 || d.restarted[0] != "vpn-socks" {
		t.Fatalf("got %d, restarted %v", w.Code, d.restarted)
	}
}

func TestRestart_UnknownContainer(t *testing.T) {
	s, d := newTestServer(t)
	if w := do(t, s.Routes(), "POST", "/api/restart/vpn-admin"); w.Code != 400 {
		t.Errorf("code = %d", w.Code)
	}
	if len(d.restarted) != 0 {
		t.Error("must not restart unknown container")
	}
}

func TestRestart_ErrorIsGeneric(t *testing.T) {
	s, d := newTestServer(t)
	d.err = errors.New("secret internal detail")
	w := do(t, s.Routes(), "POST", "/api/restart/vpn-ovpn")
	if w.Code != 500 || strings.Contains(w.Body.String(), "secret") {
		t.Errorf("got %d %q", w.Code, w.Body.String())
	}
}

func TestReconnect(t *testing.T) {
	cfg := testConfig()
	cfg.OVPNContainer = "custom-ovpn"
	d := &fakeDocker{}
	s := newTestServerWith(t, cfg, d)
	w := do(t, s.Routes(), "POST", "/api/reconnect")
	if w.Code != 200 || len(d.signalled) != 1 || d.signalled[0] != "custom-ovpn:SIGUSR1" {
		t.Fatalf("got %d, signalled %v", w.Code, d.signalled)
	}
}

func TestReconnect_ErrorIsGeneric(t *testing.T) {
	s, d := newTestServer(t)
	d.err = errors.New("secret internal detail")
	w := do(t, s.Routes(), "POST", "/api/reconnect")
	if w.Code != 500 || strings.Contains(w.Body.String(), "secret") {
		t.Errorf("got %d %q", w.Code, w.Body.String())
	}
}

func TestRebuildRemoved(t *testing.T) {
	s, _ := newTestServer(t)
	if w := do(t, s.Routes(), "POST", "/api/rebuild"); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Errorf("/api/rebuild code = %d, want 404/405", w.Code)
	}
}

func TestLogEvent(t *testing.T) {
	s, _ := newTestServer(t)
	s.logEvent("vpn-ovpn", "restart")
	(&Server{}).logEvent("vpn-ovpn", "restart") // nil store
	_ = s.store.Close()
	s.logEvent("vpn-ovpn", "restart") // store error is only logged
}

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, 418, map[string]string{"status": "teapot"})
	if w.Code != 418 || w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("got %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	writeJSON(httptest.NewRecorder(), 200, func() {}) // unencodable: logged, no panic
}

func TestNew_TokenFile(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()

	cfg.AdminTokenFile = filepath.Join(dir, "missing")
	if _, err := New(cfg, &fakeDocker{}, nil, nil, nil, nil); err == nil {
		t.Error("expected error for missing token file")
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.AdminTokenFile = empty
	if _, err := New(cfg, &fakeDocker{}, nil, nil, nil, nil); err == nil {
		t.Error("expected error for empty token file")
	}
}
