package main

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pom6ac/vpn-socks/admin/internal/dockerapi"
	"github.com/pom6ac/vpn-socks/admin/internal/health"
	"github.com/pom6ac/vpn-socks/admin/internal/storage"
	"github.com/pom6ac/vpn-socks/admin/internal/traffic"
)

func newCmdTestStore(t *testing.T) *storage.Store {
	t.Helper()
	s, _ := newCmdTestStorePath(t)
	return s
}

func newCmdTestStorePath(t *testing.T) (*storage.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := storage.New(path)
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

// countRows runs a COUNT(*) query on the database file through a separate
// connection, counting real rows rather than aggregated history buckets.
func countRows(t *testing.T, path, query string, args ...any) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// fakeStore records every write; err, when set, is returned from each call.
type fakeStore struct {
	mu      sync.Mutex
	metrics map[string][]float64
	events  []string
	calls   int
	err     error
}

func newFakeStore() *fakeStore { return &fakeStore{metrics: map[string][]float64{}} }

func (f *fakeStore) WriteMetric(kind string, v float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.metrics[kind] = append(f.metrics[kind], v)
	return nil
}

func (f *fakeStore) WriteContainerEvent(container, event string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, container+":"+event)
	return nil
}

func (f *fakeStore) count(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.metrics[kind])
}

// startRecordMetrics runs recordMetrics with a manual tick channel. tick(n)
// returns only after the loop has fully processed n ticks: each send on the
// unbuffered channel completes when the loop is back in its select, so the
// final empty send proves the previous tick finished. stop cancels and joins.
func startRecordMetrics(w metricWriter, p *health.Prober, tr *traffic.Reader) (tick func(n int), stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() { recordMetrics(ctx, w, p, tr, ticks); close(done) }()
	tick = func(n int) {
		for i := 0; i < n; i++ {
			ticks <- time.Now()
		}
	}
	stop = func() {
		cancel()
		<-done
	}
	return tick, stop
}

// okTraffic returns a traffic reader whose current sample is OK.
func okTraffic(t *testing.T) *traffic.Reader {
	t.Helper()
	tr := traffic.NewReader(&fakeNetDev{}, "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tr.Run(ctx) // takes exactly one (baseline) sample, then returns
	if !tr.Current().OK {
		t.Fatal("traffic not OK")
	}
	return tr
}

func TestRecordMetrics_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recordMetrics(ctx, newCmdTestStore(t), health.NewProber("", nil, "", "", "", nil, "", nil), nil, make(chan time.Time))
}

// probedOnce returns a prober that has completed exactly one round of
// (failing) probes and stays idle afterwards.
func probedOnce(t *testing.T) *health.Prober {
	t.Helper()
	prober := health.NewProber("", nil, "127.0.0.1", "1", "http://127.0.0.1:1", nil, "", nil)
	prober.TunnelInterval, prober.DNSInterval, prober.HTTPInterval = time.Hour, time.Hour, time.Hour
	pctx, pcancel := context.WithCancel(context.Background())
	t.Cleanup(func() { pcancel(); prober.Wait() })
	prober.Run(pctx)
	deadline := time.Now().Add(2 * time.Second)
	for {
		r := prober.GetReport()
		if len(r.Probes) == 3 && r.Probes[0].LastRun != "" && r.Probes[1].LastRun != "" && r.Probes[2].LastRun != "" {
			return prober
		}
		if time.Now().After(deadline) {
			t.Fatal("probes did not run")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRecordMetrics_ProbeWrittenOncePerRun checks that a probe result is
// stored once per probe run, not on every tick.
func TestRecordMetrics_ProbeWrittenOncePerRun(t *testing.T) {
	w := newFakeStore()
	tick, stop := startRecordMetrics(w, probedOnce(t), okTraffic(t))
	tick(50)
	stop()

	for _, k := range []string{health.ProbeTunnel, health.ProbeDNS, health.ProbeHTTP} {
		if n := w.count(k); n != 1 {
			t.Errorf("%s rows = %d after 50 ticks, want 1 (one per probe run)", k, n)
		}
		if v := w.metrics[k]; len(v) == 1 && v[0] != -1 {
			t.Errorf("%s value = %v, want -1 for a failed probe", k, v[0])
		}
	}
	// Traffic is written on every tick: proves 50 ticks were processed.
	if n := w.count("rx_bytes"); n != 50 {
		t.Errorf("rx_bytes rows = %d, want 50", n)
	}
}

func TestRecordMetrics_NoTrafficWhileNotOK(t *testing.T) {
	w := newFakeStore()
	tick, stop := startRecordMetrics(w, health.NewProber("", nil, "", "", "", nil, "", nil), traffic.NewReader(nil, "x"))
	tick(10)
	stop()
	if w.count("rx_bytes") != 0 || w.count("tx_bytes") != 0 {
		t.Error("traffic must not be written while !OK")
	}
	if w.calls != 0 {
		t.Errorf("writes = %d, want 0 (no traffic, no probe runs)", w.calls)
	}
}

func TestRecordMetrics_WritesToSQLite(t *testing.T) {
	store, path := newCmdTestStorePath(t)
	tick, stop := startRecordMetrics(store, probedOnce(t), okTraffic(t))
	tick(20)
	stop()
	const q = "SELECT COUNT(*) FROM metrics WHERE kind = ?"
	for _, k := range []string{health.ProbeTunnel, health.ProbeDNS, health.ProbeHTTP} {
		if n := countRows(t, path, q, k); n != 1 {
			t.Errorf("%s rows = %d after 20 ticks, want 1", k, n)
		}
	}
	if n := countRows(t, path, q, "rx_bytes"); n != 20 {
		t.Errorf("rx_bytes rows = %d, want 20", n)
	}
}

func TestRecordMetrics_WriteErrorsKeepGoing(t *testing.T) {
	w := newFakeStore()
	w.err = errors.New("disk full")
	tick, stop := startRecordMetrics(w, health.NewProber("", nil, "", "", "", nil, "", nil), okTraffic(t))
	tick(5)
	stop()
	if w.calls != 10 { // rx + tx per tick; errors are logged (rate limited)
		t.Errorf("write attempts = %d, want 10", w.calls)
	}
}

type fakeNetDev struct{ n int64 }

func (f *fakeNetDev) ExecProcNetDev(context.Context, string) (string, error) {
	f.n += 1000
	return "tun0: " + strconv.FormatInt(f.n, 10) + " 0 0 0 0 0 0 0 " + strconv.FormatInt(f.n, 10) + " 0 0 0 0 0 0 0\n", nil
}

func TestRateLogger(t *testing.T) {
	l := &rateLogger{interval: time.Hour}
	l.Printf("a")
	l.Printf("b")
	l.Printf("c")
	if l.suppressed != 2 {
		t.Errorf("suppressed = %d, want 2", l.suppressed)
	}
	l.last = time.Now().Add(-2 * time.Hour)
	l.Printf("d")
	if l.suppressed != 0 {
		t.Errorf("suppressed = %d after interval, want 0", l.suppressed)
	}
}

type fakeEvents struct {
	msgs chan dockerapi.EventMessage
	errs chan error
}

func (f *fakeEvents) Events(context.Context, []string) (<-chan dockerapi.EventMessage, <-chan error) {
	return f.msgs, f.errs
}

func ev(action, name string) dockerapi.EventMessage {
	return dockerapi.EventMessage{Action: action, Actor: dockerapi.EventActor{Attributes: map[string]string{"name": name}}}
}

func TestRecordContainerEvents(t *testing.T) {
	w := newFakeStore()
	src := &fakeEvents{msgs: make(chan dockerapi.EventMessage), errs: make(chan error)}
	done := make(chan struct{})
	go func() {
		recordContainerEvents(context.Background(), w, src, []string{"vpn-ovpn", "vpn-socks"})
		close(done)
	}()

	src.msgs <- ev("start", "vpn-ovpn")
	src.msgs <- ev("die", "other")                  // filtered out
	src.msgs <- dockerapi.EventMessage{Action: "x"} // no attributes
	src.errs <- errors.New("stream closed")         // logged, keeps going
	close(src.errs)                                 // closed error channel is ignored
	src.msgs <- ev("restart", "vpn-socks")
	close(src.msgs)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recordContainerEvents did not return after msgs closed")
	}
	want := []string{"vpn-ovpn:start", "vpn-socks:restart"}
	if !reflect.DeepEqual(w.events, want) {
		t.Errorf("stored events = %q, want %q", w.events, want)
	}
}

func TestRecordContainerEvents_SQLite(t *testing.T) {
	store, path := newCmdTestStorePath(t)
	src := &fakeEvents{msgs: make(chan dockerapi.EventMessage, 3), errs: make(chan error)}
	src.msgs <- ev("start", "vpn-ovpn")
	src.msgs <- ev("die", "other") // filtered out
	src.msgs <- ev("die", "vpn-socks")
	close(src.msgs)
	recordContainerEvents(context.Background(), store, src, []string{"vpn-ovpn", "vpn-socks"})
	if n := countRows(t, path, "SELECT COUNT(*) FROM container_events"); n != 2 {
		t.Errorf("container_events rows = %d, want 2", n)
	}
	if n := countRows(t, path, "SELECT COUNT(*) FROM container_events WHERE container = 'vpn-ovpn' AND event = 'start'"); n != 1 {
		t.Errorf("vpn-ovpn start rows = %d, want 1", n)
	}
	if n := countRows(t, path, "SELECT COUNT(*) FROM container_events WHERE container = 'other'"); n != 0 {
		t.Errorf("filtered container stored: %d rows", n)
	}
}

func TestRecordContainerEvents_WriteError(t *testing.T) {
	w := newFakeStore()
	w.err = errors.New("db closed")
	src := &fakeEvents{msgs: make(chan dockerapi.EventMessage, 1), errs: make(chan error)}
	src.msgs <- ev("start", "vpn-ovpn")
	close(src.msgs)
	recordContainerEvents(context.Background(), w, src, []string{"vpn-ovpn"}) // logged, not fatal
	if w.calls != 1 {
		t.Errorf("write attempts = %d", w.calls)
	}
}

func TestRecordContainerEvents_Cancel(t *testing.T) {
	src := &fakeEvents{msgs: make(chan dockerapi.EventMessage), errs: make(chan error)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recordContainerEvents(ctx, newCmdTestStore(t), src, nil)
}

func TestRecordContainerEvents_RealClientCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recordContainerEvents(ctx, newCmdTestStore(t), dockerapi.NewClient(), []string{"vpn-ovpn"})
}

func TestNewHTTPServer(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "base")
	s := newHTTPServer(ctx, ":0", http.NotFoundHandler())
	if s.ReadHeaderTimeout != 5*time.Second || s.WriteTimeout != 30*time.Second {
		t.Errorf("timeouts = %v / %v", s.ReadHeaderTimeout, s.WriteTimeout)
	}
	if s.BaseContext(nil) != ctx {
		t.Error("BaseContext must return the app context")
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestServe_ShutdownCancelsRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done() // a long-lived SSE-like request
	})
	srv := newHTTPServer(ctx, freeAddr(t), h)
	errCh := make(chan error, 1)
	go func() { errCh <- serve(ctx, cancel, srv) }()

	go func() {
		for i := 0; i < 100; i++ {
			resp, err := http.Get("http://" + srv.Addr + "/")
			if err == nil {
				_ = resp.Body.Close()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	<-started
	start := time.Now()
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return; in-flight request blocked shutdown")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("shutdown took %v", d)
	}
}

func TestServe_ListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := newHTTPServer(ctx, ln.Addr().String(), http.NotFoundHandler())
	if err := serve(ctx, cancel, srv); err == nil {
		t.Fatal("expected listen error for busy port")
	}
	if ctx.Err() == nil {
		t.Error("serve must cancel the app context on failure")
	}
}

func TestRun_StorageError(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DB_PATH", filepath.Join(f, "admin.db"))
	if err := run(); err == nil {
		t.Fatal("expected storage error")
	}
}

func TestRun_ProxyPasswordFileError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DB_PATH", filepath.Join(dir, "admin.db"))
	t.Setenv("PROXY_USER", "u")
	t.Setenv("PROXY_PASSWORD_FILE", filepath.Join(dir, "missing"))
	if err := run(); err == nil {
		t.Fatal("expected PROXY_PASSWORD_FILE error")
	}
}

func TestRun_ExpectedExitIPError(t *testing.T) {
	t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "admin.db"))
	t.Setenv("EXPECTED_EXIT_IP", "203.0.113.7,not-an-ip")
	err := run()
	if err == nil || !strings.Contains(err.Error(), "EXPECTED_EXIT_IP") {
		t.Fatalf("err = %v, want EXPECTED_EXIT_IP error", err)
	}
}

func TestRun_TokenFileError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DB_PATH", filepath.Join(dir, "admin.db"))
	t.Setenv("ADMIN_TOKEN_FILE", filepath.Join(dir, "missing-token"))
	t.Setenv("SOCKS5_PROXY", "127.0.0.1:1")
	t.Setenv("HTTP_CHECK_URLS", "http://127.0.0.1:1")
	t.Setenv("DNS_CHECK_URL", "http://127.0.0.1:1")
	t.Setenv("EXIT_IP_URL", "http://127.0.0.1:1")
	if err := run(); err == nil {
		t.Fatal("expected token file error")
	}
}

func TestRun_ListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	dir := t.TempDir()
	t.Setenv("DB_PATH", filepath.Join(dir, "admin.db"))
	t.Setenv("LISTEN_ADDR", ln.Addr().String())
	t.Setenv("SOCKS5_PROXY", "127.0.0.1:1")
	t.Setenv("HTTP_CHECK_URLS", "http://127.0.0.1:1")
	t.Setenv("DNS_CHECK_URL", "http://127.0.0.1:1")
	t.Setenv("EXIT_IP_URL", "http://127.0.0.1:1")
	if err := run(); err == nil {
		t.Fatal("expected listen error")
	}
}
