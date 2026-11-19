// Package server implements the admin panel HTTP API and static UI.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pom6ac/vpn-socks/admin/internal/config"
	"github.com/pom6ac/vpn-socks/admin/internal/dockerapi"
	"github.com/pom6ac/vpn-socks/admin/internal/health"
	"github.com/pom6ac/vpn-socks/admin/internal/storage"
	"github.com/pom6ac/vpn-socks/admin/internal/topology"
	"github.com/pom6ac/vpn-socks/admin/internal/traffic"
	"github.com/pom6ac/vpn-socks/admin/web"
)

const (
	// maxLogStreams bounds concurrent SSE log streams.
	maxLogStreams = 10
	// maxLogLine is the longest log line the SSE stream accepts.
	maxLogLine = 1 << 20
	// recreateTimeout bounds `docker compose up --force-recreate`.
	recreateTimeout = 5 * time.Minute
	// recreateWriteSlack is extra write time after recreateTimeout for the
	// final JSON response.
	recreateWriteSlack = 30 * time.Second
)

// Docker is the subset of the Docker API the handlers use.
type Docker interface {
	ListContainers(ctx context.Context, names []string) ([]dockerapi.ContainerInfo, error)
	InspectContainer(ctx context.Context, name string) (dockerapi.ContainerInfo, error)
	RestartContainer(ctx context.Context, name string) error
	SignalContainer(ctx context.Context, name, signal string) error
	LogsStream(ctx context.Context, name string, since time.Time) (io.ReadCloser, error)
}

// CommandRunner runs an external command with exactly the given environment
// (never the inherited one) and returns its combined output.
type CommandRunner func(ctx context.Context, env []string, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// A nil Env would make the child inherit the admin environment.
	cmd.Env = append([]string{}, env...)
	return cmd.CombinedOutput()
}

// composeEnvAllowed lists the inherited variables the docker CLI needs.
// Everything else is dropped: docker compose interpolates docker-compose.yml
// with the process environment taking precedence over the project's .env
// (even when a value is empty), and admin itself receives PROXY_USER,
// PROXY_PASSWORD, ADMIN_ALLOWED_HOSTS, ... from compose as a snapshot taken
// when it was created. Passing them on would let a recreate resurrect stale
// SOCKS credentials instead of the current .env values.
var composeEnvAllowed = map[string]bool{
	"PATH":   true,
	"HOME":   true,
	"TMPDIR": true,
}

// composeEnvPrefixes are inherited variable prefixes the docker CLI needs
// (DOCKER_HOST, DOCKER_CONFIG, DOCKER_CONTEXT, DOCKER_TLS_VERIFY, ...).
var composeEnvPrefixes = []string{"DOCKER_"}

// composeEnv builds the environment of the recreate command from an allowlist
// of environ, plus HOST_PROJECT_DIR (the same path passed as
// --project-directory, required by docker-compose.yml interpolation).
func composeEnv(environ []string, hostProjectDir string) []string {
	env := make([]string, 0, len(composeEnvAllowed)+2)
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if composeEnvAllowed[k] || hasAnyPrefix(k, composeEnvPrefixes) {
			env = append(env, kv)
		}
	}
	return append(env, "HOST_PROJECT_DIR="+hostProjectDir)
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// Server holds all dependencies and HTTP handlers for the admin panel.
type Server struct {
	cfg     config.Config
	docker  Docker
	prober  *health.Prober
	traffic *traffic.Reader
	store   *storage.Store
	topo    *topology.Collector

	runCommand      CommandRunner
	recreateTimeout time.Duration
	recreateMu      sync.Mutex

	logStreams chan struct{}

	allowedHosts map[string]bool
	token        string
}

// New creates a Server from the given dependencies. It fails only when
// ADMIN_TOKEN_FILE is configured but unreadable or empty.
func New(cfg config.Config, docker Docker, prober *health.Prober, tr *traffic.Reader, store *storage.Store, topo *topology.Collector) (*Server, error) {
	s := &Server{
		cfg:             cfg,
		docker:          docker,
		prober:          prober,
		traffic:         tr,
		store:           store,
		topo:            topo,
		runCommand:      execRunner,
		recreateTimeout: recreateTimeout,
		logStreams:      make(chan struct{}, maxLogStreams),
		allowedHosts:    map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true},
	}
	for _, h := range cfg.AllowedHosts {
		s.allowedHosts[normalizeHost(h)] = true
	}
	if cfg.AdminTokenFile != "" {
		b, err := os.ReadFile(cfg.AdminTokenFile)
		if err != nil {
			return nil, fmt.Errorf("read admin token file: %w", err)
		}
		s.token = strings.TrimSpace(string(b))
		if s.token == "" {
			return nil, errors.New("admin token file is empty")
		}
	}
	return s, nil
}

// Routes returns the HTTP handler with all routes and middleware registered.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(web.Files)))

	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/logs/{container}", s.handleLogs)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/metrics/traffic", s.handleTraffic)
	mux.HandleFunc("GET /api/metrics/history", s.handleHistory)
	mux.HandleFunc("GET /api/topology", s.handleTopology)

	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/restart/{container}", s.handleRestart)
	mux.HandleFunc("POST /api/reconnect", s.handleReconnect)
	mux.HandleFunc("POST /api/recreate", s.handleRecreate)

	var h http.Handler = mux
	h = s.authMiddleware(h)
	h = csrfMiddleware(h)
	h = s.hostMiddleware(h)
	h = securityHeaders(h)
	return loggingMiddleware(h)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := web.Files.ReadFile("index.html")
	if err != nil {
		log.Printf("index: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// configResponse tells the UI which containers it manages.
type configResponse struct {
	OVPN  string `json:"ovpn"`
	SOCKS string `json:"socks"`
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, configResponse{OVPN: s.cfg.OVPNContainer, SOCKS: s.cfg.SOCKSContainer})
}

// statusResponse is the JSON shape for /api/status.
type statusResponse struct {
	Containers           []dockerapi.ContainerInfo `json:"containers"`
	DockerAPIUnavailable bool                      `json:"docker_api_unavailable,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	containers, err := s.docker.ListContainers(ctx, s.cfg.ContainerNames)
	if err != nil {
		log.Printf("status: docker api unavailable: %v", err)
		writeJSON(w, http.StatusOK, statusResponse{DockerAPIUnavailable: true})
		return
	}
	writeJSON(w, http.StatusOK, statusResponse{Containers: containers})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.prober.GetReport())
}

func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	in := topology.Inputs{
		OVPN:        s.cfg.OVPNContainer,
		SOCKS:       s.cfg.SOCKSContainer,
		Admin:       s.cfg.AdminContainer,
		AdminListen: s.cfg.ListenAddr,
		Containers:  map[string]dockerapi.ContainerInfo{},
		Health:      s.prober.GetReport(),
	}
	if s.topo != nil {
		in.Snapshot = s.topo.Snapshot()
	}
	for _, name := range []string{in.OVPN, in.SOCKS, in.Admin} {
		info, err := s.docker.InspectContainer(ctx, name)
		if err != nil {
			log.Printf("topology: inspect %s: %v", name, err)
			continue
		}
		in.Containers[name] = info
	}
	writeJSON(w, http.StatusOK, topology.Build(in))
}

func (s *Server) handleTraffic(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.traffic.Current())
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "rx_bytes"
	}
	since := time.Now().Add(-24 * time.Hour)
	if r.URL.Query().Get("range") == "1h" {
		since = time.Now().Add(-1 * time.Hour)
	}
	points, err := s.store.History(r.Context(), kind, since)
	if err != nil {
		log.Printf("history query failed: %v", err)
		writeError(w, http.StatusInternalServerError, "history unavailable")
		return
	}
	writeJSON(w, http.StatusOK, points)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	container := r.PathValue("container")
	if !s.isValidContainer(container) {
		http.Error(w, "unknown container", http.StatusBadRequest)
		return
	}

	select {
	case s.logStreams <- struct{}{}:
		defer func() { <-s.logStreams }()
	default:
		http.Error(w, "too many log streams", http.StatusServiceUnavailable)
		return
	}

	var since time.Time
	if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
		if t, err := time.Parse(time.RFC3339Nano, sinceStr); err == nil {
			// Docker's since is inclusive; skip the line the client already has.
			since = t.Add(time.Nanosecond)
		}
	}

	rc, err := s.docker.LogsStream(r.Context(), container, since)
	if err != nil {
		log.Printf("logs stream error: %v", err)
		http.Error(w, "docker api unavailable", http.StatusServiceUnavailable)
		return
	}
	demux := newDockerLogDemux(rc)
	defer func() { _ = demux.Close() }()

	// The stream is long-lived: lift the server-wide write timeout.
	rcCtl := http.NewResponseController(w)
	if err := rcCtl.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		log.Printf("logs: clear write deadline: %v", err)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	// container is one of the configured names (checked above).
	if _, err := fmt.Fprintf(w, "event: connected\ndata: %s\n\n", container); err != nil { //nolint:gosec // G705: whitelisted value, text/event-stream
		return
	}
	if err := rcCtl.Flush(); err != nil {
		log.Printf("logs: streaming unsupported: %v", err)
		return
	}

	scanner := bufio.NewScanner(demux)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLogLine)
	for scanner.Scan() {
		line := scanner.Text()
		data, err := json.Marshal(map[string]string{
			"line":     line,
			"severity": string(classifyLogLine(line)),
		})
		if err != nil {
			continue
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return
		}
		if err := rcCtl.Flush(); err != nil {
			return
		}
	}
	if err := scanner.Err(); err != nil && r.Context().Err() == nil {
		log.Printf("logs %q: read: %v", container, err) //nolint:gosec // G706: whitelisted, quoted
	}
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	container := r.PathValue("container")
	if !s.isValidContainer(container) {
		http.Error(w, "unknown container", http.StatusBadRequest)
		return
	}
	if err := s.docker.RestartContainer(r.Context(), container); err != nil {
		log.Printf("restart %q failed: %v", container, err) //nolint:gosec // G706: whitelisted, quoted
		writeError(w, http.StatusInternalServerError, "restart failed; see admin logs")
		return
	}
	s.logEvent(container, "restart")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.docker.SignalContainer(r.Context(), s.cfg.OVPNContainer, "SIGUSR1"); err != nil {
		log.Printf("reconnect failed: %v", err)
		writeError(w, http.StatusInternalServerError, "reconnect failed; see admin logs")
		return
	}
	s.logEvent(s.cfg.OVPNContainer, "reconnect")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

const (
	defaultComposeFile  = "docker-compose.yml"
	defaultOverrideFile = "docker-compose.override.yml"
)

// recreateArgs is the docker CLI invocation that recreates the VPN services.
//
// --project-directory makes compose read the project's .env, as a manual
// `docker compose up` in that directory does. An explicit -f disables the
// automatic docker-compose.override.yml merge, so for the default compose
// file the override next to it is passed explicitly when it exists:
// otherwise a recreate would silently drop the settings it adds.
func (s *Server) recreateArgs() []string {
	composeFile := s.cfg.ComposeFile
	if composeFile == "" {
		composeFile = filepath.Join(s.cfg.HostProjectDir, defaultComposeFile)
	}
	args := []string{"compose",
		"--project-directory", s.cfg.HostProjectDir,
		"-p", s.cfg.ComposeProject,
		"-f", composeFile}
	if override, ok := composeOverride(composeFile); ok {
		args = append(args, "-f", override)
	}
	return append(args, "up", "-d", "--force-recreate", "ovpn", "socks5")
}

// composeOverride returns docker-compose.override.yml next to composeFile
// when composeFile is a docker-compose.yml and the override is a regular
// file. Other compose file names are used as given, without an override.
func composeOverride(composeFile string) (string, bool) {
	if filepath.Base(composeFile) != defaultComposeFile {
		return "", false
	}
	override := filepath.Join(filepath.Dir(composeFile), defaultOverrideFile)
	fi, err := os.Stat(override)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("recreate: %s ignored: %v", override, err)
		}
		return "", false
	}
	if !fi.Mode().IsRegular() {
		log.Printf("recreate: %s ignored: not a regular file", override)
		return "", false
	}
	return override, true
}

func (s *Server) handleRecreate(w http.ResponseWriter, _ *http.Request) {
	if !filepath.IsAbs(s.cfg.HostProjectDir) {
		log.Printf("recreate: HOST_PROJECT_DIR %q is not an absolute path", s.cfg.HostProjectDir)
		writeError(w, http.StatusServiceUnavailable,
			"recreate is unavailable: HOST_PROJECT_DIR must be set to the absolute project path on the host")
		return
	}
	if !s.recreateMu.TryLock() {
		writeError(w, http.StatusConflict, "recreate is already in progress")
		return
	}
	defer s.recreateMu.Unlock()

	// The command may run far longer than the server-wide WriteTimeout:
	// extend this response's write deadline so a successful recreate can
	// still report its result.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.recreateTimeout + recreateWriteSlack)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		log.Printf("recreate: extend write deadline: %v", err)
	}

	// Detached from the request: a closed browser tab must not abort compose
	// half-way through recreating the stack.
	ctx, cancel := context.WithTimeout(context.Background(), s.recreateTimeout)
	defer cancel()

	env := composeEnv(os.Environ(), s.cfg.HostProjectDir)
	out, err := s.runCommand(ctx, env, "docker", s.recreateArgs()...)
	log.Printf("recreate output:\n%s", out)
	if err != nil {
		log.Printf("recreate failed: %v", err)
		writeError(w, http.StatusInternalServerError, "recreate failed; see admin logs")
		return
	}
	s.logEvent(s.cfg.OVPNContainer, "recreate")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) isValidContainer(name string) bool {
	for _, c := range s.cfg.ContainerNames {
		if c == name {
			return true
		}
	}
	return false
}

func (s *Server) logEvent(container, event string) {
	if s.store != nil {
		if err := s.store.WriteContainerEvent(container, event); err != nil {
			log.Printf("write container event failed: %v", err)
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
