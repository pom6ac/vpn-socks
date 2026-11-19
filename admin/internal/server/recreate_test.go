package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pom6ac/vpn-socks/admin/internal/config"
)

// recordingRunner captures command invocations; it can be made to block.
type recordingRunner struct {
	mu      sync.Mutex
	calls   [][]string
	envs    [][]string
	ctxs    []context.Context
	ctxErrs []error
	out     string
	err     error
	started chan struct{}
	release chan struct{}
}

func (r *recordingRunner) run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.envs = append(r.envs, env)
	r.ctxs = append(r.ctxs, ctx)
	r.ctxErrs = append(r.ctxErrs, ctx.Err())
	r.mu.Unlock()
	if r.started != nil {
		close(r.started)
	}
	if r.release != nil {
		<-r.release
	}
	return []byte(r.out), r.err
}

func TestRecreate_Command(t *testing.T) {
	s, _ := newTestServer(t)
	rr := &recordingRunner{out: "Container vpn-ovpn Recreated\nsecret compose detail"}
	s.runCommand = rr.run

	w := do(t, s.Routes(), "POST", "/api/recreate")
	if w.Code != 200 {
		t.Fatalf("code = %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret compose detail") {
		t.Error("compose output must not be returned to the client")
	}
	want := []string{"docker", "compose",
		"--project-directory", "/srv/vpn-socks",
		"-p", "vpn-socks",
		"-f", "/srv/vpn-socks/docker-compose.yml",
		"up", "-d", "--force-recreate", "ovpn", "socks5"}
	if len(rr.calls) != 1 || !reflect.DeepEqual(rr.calls[0], want) {
		t.Fatalf("args = %q\nwant   %q", rr.calls, want)
	}
	if _, ok := rr.ctxs[0].Deadline(); !ok {
		t.Error("recreate context must have a timeout")
	}
}

func TestRecreate_ExplicitComposeFile(t *testing.T) {
	cfg := testConfig()
	cfg.ComposeFile = "/srv/vpn-socks/compose.custom.yml"
	cfg.ComposeProject = "other"
	s := newTestServerWith(t, cfg, &fakeDocker{})
	rr := &recordingRunner{}
	s.runCommand = rr.run
	if w := do(t, s.Routes(), "POST", "/api/recreate"); w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	got := strings.Join(rr.calls[0], " ")
	if !strings.Contains(got, "-p other -f /srv/vpn-socks/compose.custom.yml") {
		t.Errorf("args = %q", got)
	}
}

func TestRecreate_DetachedFromRequestContext(t *testing.T) {
	s, _ := newTestServer(t)
	rr := &recordingRunner{}
	s.runCommand = rr.run
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := (&http.Request{}).WithContext(ctx)
	s.handleRecreate(newDiscardWriter(), r)
	if err := rr.ctxErrs[0]; err != nil {
		t.Errorf("runner ctx err = %v; must not inherit the request context", err)
	}
}

func TestRecreate_RejectsBadProjectDir(t *testing.T) {
	for _, dir := range []string{"", ".", "./project", "relative/dir"} {
		t.Run(dir, func(t *testing.T) {
			cfg := testConfig()
			cfg.HostProjectDir = dir
			s := newTestServerWith(t, cfg, &fakeDocker{})
			rr := &recordingRunner{}
			s.runCommand = rr.run
			w := do(t, s.Routes(), "POST", "/api/recreate")
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("code = %d, want 503", w.Code)
			}
			if resp := decode[map[string]string](t, w); !strings.Contains(resp["error"], "HOST_PROJECT_DIR") {
				t.Errorf("error = %q", resp["error"])
			}
			if len(rr.calls) != 0 {
				t.Error("command must not run")
			}
		})
	}
}

func TestRecreate_ConcurrentIs409(t *testing.T) {
	s, _ := newTestServer(t)
	rr := &recordingRunner{started: make(chan struct{}), release: make(chan struct{})}
	s.runCommand = rr.run
	h := s.Routes()

	first := make(chan int)
	go func() { first <- do(t, h, "POST", "/api/recreate").Code }()
	<-rr.started

	if w := do(t, h, "POST", "/api/recreate"); w.Code != http.StatusConflict {
		t.Errorf("concurrent recreate code = %d, want 409", w.Code)
	}
	close(rr.release)
	if code := <-first; code != 200 {
		t.Errorf("first recreate code = %d", code)
	}

	// The lock is released afterwards.
	rr.started, rr.release = nil, nil
	if w := do(t, h, "POST", "/api/recreate"); w.Code != 200 {
		t.Errorf("follow-up recreate code = %d", w.Code)
	}
}

func TestRecreate_FailureIsGeneric(t *testing.T) {
	s, _ := newTestServer(t)
	rr := &recordingRunner{out: "Error: secret path /srv/x", err: errors.New("exit status 1")}
	s.runCommand = rr.run
	w := do(t, s.Routes(), "POST", "/api/recreate")
	if w.Code != 500 {
		t.Fatalf("code = %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "secret") || strings.Contains(body, "exit status") {
		t.Errorf("internal details leaked: %s", body)
	}
}

func TestRecreate_Timeout(t *testing.T) {
	s, _ := newTestServer(t)
	s.recreateTimeout = 20 * time.Millisecond
	s.runCommand = func(ctx context.Context, _ []string, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if w := do(t, s.Routes(), "POST", "/api/recreate"); w.Code != 500 {
		t.Errorf("code = %d", w.Code)
	}
}

// TestRecreate_OutlivesWriteTimeout runs a real server whose WriteTimeout is
// shorter than the recreate command: the handler must extend the deadline
// so the client still gets the successful result.
func TestRecreate_OutlivesWriteTimeout(t *testing.T) {
	s, _ := newTestServer(t)
	s.recreateTimeout = 5 * time.Second
	s.runCommand = func(ctx context.Context, _ []string, _ string, _ ...string) ([]byte, error) {
		select {
		case <-time.After(300 * time.Millisecond):
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ts := httptest.NewUnstartedServer(s.Routes())
	ts.Config.WriteTimeout = 100 * time.Millisecond
	ts.Start()
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", ts.URL+"/api/recreate", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "localhost"
	req.Header.Set(csrfHeader, csrfValue)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("recreate response lost (write deadline not extended?): %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok"`) {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
}

func TestExecRunner(t *testing.T) {
	out, err := execRunner(context.Background(), composeEnv(os.Environ(), "/srv/p"), "go", "env", "GOOS")
	if err != nil || len(out) == 0 {
		t.Fatalf("execRunner: %q %v", out, err)
	}
}

// TestExecRunner_ExactEnv checks the child sees only the given environment,
// never the inherited one (also when the given env is empty).
func TestExecRunner_ExactEnv(t *testing.T) {
	envBin, err := exec.LookPath("env")
	if err != nil {
		t.Skip("env binary not available")
	}
	t.Setenv("PROXY_PASSWORD", "inherited-secret")
	for _, env := range [][]string{nil, {}, {"FOO=bar"}} {
		out, err := execRunner(context.Background(), env, envBin)
		if err != nil {
			t.Fatalf("execRunner(%q): %q %v", env, out, err)
		}
		got := strings.Fields(string(out))
		want := append([]string{}, env...)
		if len(got) == 0 {
			got = nil
		}
		if len(want) == 0 {
			want = nil
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("env %q: child environment = %q", env, got)
		}
	}
}

// credentialEnv are variables admin receives from compose that docker compose
// would otherwise prefer over the project's .env when interpolating.
var credentialEnv = []string{
	"PROXY_USER", "PROXY_PASSWORD", "PROXY_PASSWORD_FILE",
	"SOCKS_REQUIRE_AUTH", "OVPN_CONFIG", "ADMIN_ALLOWED_HOSTS",
	"ADMIN_TOKEN_FILE", "COMPOSE_FILE", "COMPOSE_PROJECT_NAME",
}

// TestRecreate_EnvWithoutCredentials is the N1 regression test: whatever
// SOCKS credentials admin was started with (set or empty), the compose child
// must not receive them, so it interpolates the current project .env.
func TestRecreate_EnvWithoutCredentials(t *testing.T) {
	for _, tc := range []struct{ name, val string }{
		{"stale values", "old-A"},
		{"empty values", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range credentialEnv {
				t.Setenv(k, tc.val)
			}
			t.Setenv("PATH", "/usr/bin:/bin")
			t.Setenv("HOME", "/root")
			t.Setenv("DOCKER_CONFIG", "/tmp/.docker")
			t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
			t.Setenv("HOST_PROJECT_DIR", "/elsewhere")

			s, _ := newTestServer(t)
			rr := &recordingRunner{}
			s.runCommand = rr.run
			if w := do(t, s.Routes(), "POST", "/api/recreate"); w.Code != 200 {
				t.Fatalf("code = %d: %s", w.Code, w.Body.String())
			}
			if len(rr.envs) != 1 {
				t.Fatalf("runner calls = %d", len(rr.envs))
			}
			env := rr.envs[0]
			if env == nil {
				t.Fatal("nil env would inherit the admin environment")
			}
			got := map[string]string{}
			for _, kv := range env {
				k, v, _ := strings.Cut(kv, "=")
				got[k] = v
			}
			for _, k := range credentialEnv {
				if _, ok := got[k]; ok {
					t.Errorf("%s passed to compose: %q", k, env)
				}
			}
			want := map[string]string{
				"PATH":             "/usr/bin:/bin",
				"HOME":             "/root",
				"DOCKER_CONFIG":    "/tmp/.docker",
				"DOCKER_HOST":      "unix:///var/run/docker.sock",
				"HOST_PROJECT_DIR": "/srv/vpn-socks",
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q (env %q)", k, got[k], v, env)
				}
			}
			for _, kv := range env {
				if strings.Contains(kv, "old-A") {
					t.Errorf("stale value leaked: %q", kv)
				}
			}
		})
	}
}

func TestComposeEnv_Allowlist(t *testing.T) {
	got := composeEnv([]string{
		"PATH=/bin", "HOME=/root", "TMPDIR=/tmp", "DOCKER_CONTEXT=x",
		"PROXY_USER=", "PROXY_PASSWORD=", "PROXY_PASSWORD_FILE=/run/secrets/p",
		"OVPN_CONFIG=./a.ovpn", "HOST_PROJECT_DIR=/stale", "noequals",
		"MY_DOCKER_X=1", "PATHX=1",
	}, "/srv/p")
	want := []string{"PATH=/bin", "HOME=/root", "TMPDIR=/tmp", "DOCKER_CONTEXT=x", "HOST_PROJECT_DIR=/srv/p"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("composeEnv = %q, want %q", got, want)
	}
}

type discardWriter struct{ h http.Header }

func newDiscardWriter() *discardWriter               { return &discardWriter{h: http.Header{}} }
func (d *discardWriter) Header() http.Header         { return d.h }
func (d *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardWriter) WriteHeader(int)             {}

// writeProjectFiles creates the named files (or directories, for names
// ending in "/") in a fresh project directory and returns its path.
func writeProjectFiles(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		p := filepath.Join(dir, n)
		if strings.HasSuffix(n, "/") {
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(p, []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func recreateCall(t *testing.T, cfg config.Config) []string {
	t.Helper()
	s := newTestServerWith(t, cfg, &fakeDocker{})
	rr := &recordingRunner{}
	s.runCommand = rr.run
	if w := do(t, s.Routes(), "POST", "/api/recreate"); w.Code != 200 {
		t.Fatalf("code = %d: %s", w.Code, w.Body.String())
	}
	return rr.calls[0]
}

func TestRecreate_IncludesOverride(t *testing.T) {
	dir := writeProjectFiles(t, "docker-compose.yml", "docker-compose.override.yml")
	for _, composeFile := range []string{"", filepath.Join(dir, "docker-compose.yml")} {
		cfg := testConfig()
		cfg.HostProjectDir = dir
		cfg.ComposeFile = composeFile
		want := []string{"docker", "compose",
			"--project-directory", dir,
			"-p", "vpn-socks",
			"-f", filepath.Join(dir, "docker-compose.yml"),
			"-f", filepath.Join(dir, "docker-compose.override.yml"),
			"up", "-d", "--force-recreate", "ovpn", "socks5"}
		if got := recreateCall(t, cfg); !reflect.DeepEqual(got, want) {
			t.Errorf("ComposeFile=%q: args = %q\nwant   %q", composeFile, got, want)
		}
	}
}

func TestRecreate_OverrideSkipped(t *testing.T) {
	cases := map[string]struct {
		files       []string
		composeFile string
	}{
		"absent":         {files: []string{"docker-compose.yml"}, composeFile: "docker-compose.yml"},
		"directory":      {files: []string{"docker-compose.yml", "docker-compose.override.yml/"}, composeFile: "docker-compose.yml"},
		"custom compose": {files: []string{"compose.custom.yml", "docker-compose.override.yml"}, composeFile: "compose.custom.yml"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeProjectFiles(t, tc.files...)
			cfg := testConfig()
			cfg.HostProjectDir = dir
			cfg.ComposeFile = filepath.Join(dir, tc.composeFile)
			got := recreateCall(t, cfg)
			if strings.Contains(strings.Join(got, " "), "override") {
				t.Errorf("args = %q; override must not be passed", got)
			}
		})
	}
}

func TestComposeOverride_StatError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "docker-compose.override.yml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if p, ok := composeOverride(filepath.Join(locked, "docker-compose.yml")); ok {
		t.Errorf("composeOverride = %q, true; want skipped on stat error", p)
	}
}
