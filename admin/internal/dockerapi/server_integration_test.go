package dockerapi_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pom6ac/vpn-socks/admin/internal/config"
	"github.com/pom6ac/vpn-socks/admin/internal/dockerapi"
	"github.com/pom6ac/vpn-socks/admin/internal/health"
	"github.com/pom6ac/vpn-socks/admin/internal/server"
	"github.com/pom6ac/vpn-socks/admin/internal/storage"
	"github.com/pom6ac/vpn-socks/admin/internal/traffic"
)

func makeFrame(streamType byte, payload []byte) []byte {
	frame := make([]byte, 8+len(payload))
	frame[0] = streamType
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], payload)
	return frame
}

func newMockDockerClient(t *testing.T, handler http.HandlerFunc) *dockerapi.Client {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, "tcp", ts.Listener.Addr().String())
		},
	}
	return dockerapi.NewClientWithHTTP(&http.Client{Transport: tr, Timeout: 0})
}

func newIntegrationServer(t *testing.T, dockerHandler http.HandlerFunc) *server.Server {
	t.Helper()
	cfg := config.Config{
		ContainerNames: []string{"vpn-ovpn", "vpn-socks"},
		OVPNContainer:  "vpn-ovpn",
		SOCKSContainer: "vpn-socks",
		AdminContainer: "vpn-admin",
		// httptest.NewRequest uses Host "example.com".
		AllowedHosts: []string{"example.com"},
		ListenAddr:   ":8080",
	}
	dockerCli := newMockDockerClient(t, dockerHandler)
	prober := health.NewProber("", nil, "", "", "", nil, "", nil)
	tr := traffic.NewReader(nil, "vpn-ovpn")

	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := storage.New(dbPath)
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	srv, err := server.New(cfg, dockerCli, prober, tr, store, nil)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return srv
}

func TestIntegration_StatusSuccess(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(r.URL.Path, "/")
		name := parts[2]
		fmt.Fprintf(w, `{"Name":"/%s","RestartCount":1,"State":{"Status":"running","StartedAt":"2024-01-01T00:00:00Z","ExitCode":0}}`, name)
	}
	s := newIntegrationServer(t, handler)
	h := s.Routes()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/status", nil)
	h.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("code = %d, want 200", w.Code)
	}

	var resp struct {
		Containers           []dockerapi.ContainerInfo `json:"containers"`
		DockerAPIUnavailable bool                      `json:"docker_api_unavailable"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.DockerAPIUnavailable {
		t.Error("expected docker_api_unavailable=false")
	}
	if len(resp.Containers) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(resp.Containers))
	}
	if resp.Containers[0].Name != "vpn-ovpn" {
		t.Errorf("name = %q", resp.Containers[0].Name)
	}
	if resp.Containers[1].Name != "vpn-socks" {
		t.Errorf("name = %q", resp.Containers[1].Name)
	}
}

func TestIntegration_LogsSSE(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(makeFrame(1, []byte("TLS Error: bad cert\n")))
		_, _ = w.Write(makeFrame(2, []byte("Initialization Sequence Completed\n")))
		_, _ = w.Write(makeFrame(1, []byte("normal line\n")))
	}
	s := newIntegrationServer(t, handler)
	h := s.Routes()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/logs/vpn-ovpn", nil)
	h.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("code = %d, want 200", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "event: connected") {
		t.Error("expected SSE connected event")
	}
	if !strings.Contains(body, "TLS Error") {
		t.Error("expected TLS Error line in SSE")
	}
	if !strings.Contains(body, `"severity":"error"`) {
		t.Error("expected error severity for TLS Error")
	}
	if !strings.Contains(body, "Initialization Sequence Completed") {
		t.Error("expected init completed line in SSE")
	}
	if !strings.Contains(body, `"severity":"info"`) {
		t.Error("expected info severity for init completed")
	}
	if !strings.Contains(body, "normal line") {
		t.Error("expected normal line in SSE")
	}
}

func TestIntegration_LogsSSE_WithSince(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "since=") {
			t.Errorf("expected since in query: %q", r.URL.RawQuery)
		}
		w.WriteHeader(200)
		_, _ = w.Write(makeFrame(1, []byte("test line\n")))
	}
	s := newIntegrationServer(t, handler)
	h := s.Routes()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/logs/vpn-ovpn?since=2024-08-12T10:00:00Z", nil)
	h.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "test line") {
		t.Error("expected test line in SSE response")
	}
}

func TestIntegration_RestartSuccess(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restart") {
			w.WriteHeader(204)
			return
		}
		w.WriteHeader(404)
	}
	s := newIntegrationServer(t, handler)
	h := s.Routes()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/restart/vpn-ovpn", nil)
	r.Header.Set("X-Requested-With", "vpn-admin")
	h.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("code = %d, want 200", w.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %q, want ok", resp["status"])
	}
}

func TestIntegration_ReconnectSuccess(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/kill") {
			w.WriteHeader(204)
			return
		}
		w.WriteHeader(404)
	}
	s := newIntegrationServer(t, handler)
	h := s.Routes()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reconnect", nil)
	r.Header.Set("X-Requested-With", "vpn-admin")
	h.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("code = %d, want 200", w.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %q, want ok", resp["status"])
	}
}

func TestIntegration_StatusMultiContainer(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(r.URL.Path, "/")
		name := parts[2]
		fmt.Fprintf(w, `{"Name":"/%s","RestartCount":0,"State":{"Status":"running","StartedAt":"2024-01-01T00:00:00Z","ExitCode":0}}`, name)
	}
	s := newIntegrationServer(t, handler)
	h := s.Routes()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/status", nil)
	h.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	var resp struct {
		Containers []dockerapi.ContainerInfo `json:"containers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Containers) != 2 {
		t.Errorf("expected 2 containers, got %d", len(resp.Containers))
	}
}

func TestIntegration_LogsUnknownContainer(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}
	s := newIntegrationServer(t, handler)
	h := s.Routes()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/logs/unknown", nil)
	h.ServeHTTP(w, r)

	if w.Code != 400 {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestIntegration_ReadBody(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"Name":"/%s","RestartCount":0,"State":{"Status":"running","StartedAt":"2024-01-01T00:00:00Z","ExitCode":0}}`, string(body))
	}
	s := newIntegrationServer(t, handler)
	h := s.Routes()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/status", nil)
	h.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
}

func TestIntegration_TrafficEndpoint(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}
	s := newIntegrationServer(t, handler)
	h := s.Routes()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/metrics/traffic", nil)
	h.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("code = %d", w.Code)
	}
	var sample traffic.Sample
	if err := json.Unmarshal(w.Body.Bytes(), &sample); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sample.OK {
		t.Error("expected OK=false")
	}
}

func TestIntegration_HealthEndpoint(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}
	s := newIntegrationServer(t, handler)
	h := s.Routes()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/health", nil)
	h.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("code = %d", w.Code)
	}
}

func TestIntegration_Timeout(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(200)
	}
	s := newIntegrationServer(t, handler)
	h := s.Routes()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/status", nil).WithContext(ctx)
	h.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("code = %d, want 200 (should return docker_api_unavailable on timeout)", w.Code)
	}
}

func TestIntegration_Topology(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/containers/vpn-ovpn/json":
			fmt.Fprint(w, `{"Name":"/vpn-ovpn","State":{"Status":"running"},
				"NetworkSettings":{"Ports":{"1080/tcp":[{"HostIp":"127.0.0.1","HostPort":"1080"}]}}}`)
		case "/containers/vpn-socks/json":
			fmt.Fprint(w, `{"Name":"/vpn-socks","State":{"Status":"exited"},"HostConfig":{"NetworkMode":"container:x"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
	h := newIntegrationServer(t, handler).Routes()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/topology", nil))
	if w.Code != 200 {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	var view struct {
		Paths []struct {
			Nodes []struct {
				ID     string   `json:"id"`
				Lines  []string `json:"lines"`
				Status string   `json:"status"`
			} `json:"nodes"`
			Links []struct {
				Status string `json:"status"`
			} `json:"links"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(view.Paths) != 2 {
		t.Fatalf("paths = %d, want 2", len(view.Paths))
	}
	main := view.Paths[0]
	if main.Nodes[0].Lines[0] != "127.0.0.1:1080" {
		t.Errorf("host listener = %v", main.Nodes[0].Lines)
	}
	if main.Nodes[1].ID != "socks" || main.Nodes[1].Status != "down" {
		t.Errorf("socks node = %+v", main.Nodes[1])
	}
	if main.Links[0].Status != "down" {
		t.Errorf("host->socks link = %s, want down", main.Links[0].Status)
	}
	// vpn-admin inspect failed (404): falls back to the configured listen addr.
	if got := view.Paths[1].Nodes[1].Lines[0]; got != ":8080" {
		t.Errorf("admin target = %q, want :8080", got)
	}
}
