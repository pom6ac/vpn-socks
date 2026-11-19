package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func framesReader(frames ...[]byte) func() io.ReadCloser {
	return func() io.ReadCloser {
		return io.NopCloser(bytes.NewReader(bytes.Join(frames, nil)))
	}
}

// sseLines extracts the "line" values from SSE data events.
func sseLines(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(l, "data: ")
		if !ok || !strings.HasPrefix(data, "{") {
			continue
		}
		var p map[string]string
		if err := json.Unmarshal([]byte(data), &p); err != nil {
			t.Fatalf("bad payload %q: %v", data, err)
		}
		out = append(out, p["line"])
	}
	return out
}

func TestLogs_Stream(t *testing.T) {
	s, d := newTestServer(t)
	d.logs = framesReader(
		makeFrame(1, []byte("TLS Error: bad cert\n")),
		makeFrame(2, []byte("Initialization Sequence Completed\n")),
	)
	w := do(t, s.Routes(), "GET", "/api/logs/vpn-ovpn")
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("got %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	if !strings.HasPrefix(body, "event: connected\ndata: vpn-ovpn\n\n") {
		t.Errorf("missing connected event: %q", body)
	}
	if !strings.Contains(body, `"severity":"error"`) {
		t.Error("expected error severity")
	}
	if got := sseLines(t, body); len(got) != 2 {
		t.Errorf("lines = %q", got)
	}
	if !d.logsSince.IsZero() {
		t.Error("since must be zero without query")
	}
}

func TestLogs_LongLine(t *testing.T) {
	s, d := newTestServer(t)
	long := strings.Repeat("x", 200<<10) // 200 KiB > bufio default 64 KiB
	d.logs = framesReader(
		makeFrame(1, []byte(long+"\n")),
		makeFrame(1, []byte("after\n")),
	)
	w := do(t, s.Routes(), "GET", "/api/logs/vpn-ovpn")
	got := sseLines(t, w.Body.String())
	if len(got) != 2 || got[0] != long || got[1] != "after" {
		t.Fatalf("got %d lines (first len %d)", len(got), len(got[0]))
	}
}

func TestLogs_TooLongLineStops(t *testing.T) {
	s, d := newTestServer(t)
	d.logs = framesReader(makeFrame(1, []byte(strings.Repeat("y", maxLogLine+10)+"\n")))
	w := do(t, s.Routes(), "GET", "/api/logs/vpn-ovpn")
	if w.Code != 200 || len(sseLines(t, w.Body.String())) != 0 {
		t.Errorf("oversized line should end the stream, got %d", w.Code)
	}
}

func TestLogs_ZeroLengthFrame(t *testing.T) {
	s, d := newTestServer(t)
	d.logs = framesReader(
		makeFrame(1, []byte("one\n")),
		makeFrame(1, nil),
		makeFrame(2, nil),
		makeFrame(1, []byte("two\n")),
	)
	w := do(t, s.Routes(), "GET", "/api/logs/vpn-ovpn")
	if got := sseLines(t, w.Body.String()); len(got) != 2 || got[1] != "two" {
		t.Fatalf("lines = %q; zero-length frame must not end the stream", got)
	}
}

func TestLogs_Since(t *testing.T) {
	s, d := newTestServer(t)
	do(t, s.Routes(), "GET", "/api/logs/vpn-ovpn?since=2024-08-12T10:00:05.123456789Z")
	want := time.Date(2024, 8, 12, 10, 0, 5, 123456790, time.UTC)
	if !d.logsSince.Equal(want) {
		t.Errorf("since = %v, want %v (last seen + 1ns)", d.logsSince, want)
	}
	do(t, s.Routes(), "GET", "/api/logs/vpn-ovpn?since=garbage")
	if !d.logsSince.IsZero() {
		t.Errorf("invalid since must be ignored, got %v", d.logsSince)
	}
}

func TestLogs_UnknownContainer(t *testing.T) {
	s, _ := newTestServer(t)
	if w := do(t, s.Routes(), "GET", "/api/logs/vpn-admin"); w.Code != 400 {
		t.Errorf("code = %d", w.Code)
	}
}

func TestLogs_DockerUnavailable(t *testing.T) {
	s, d := newTestServer(t)
	d.err = errors.New("boom")
	w := do(t, s.Routes(), "GET", "/api/logs/vpn-ovpn")
	if w.Code != 503 || strings.Contains(w.Body.String(), "boom") {
		t.Errorf("got %d %q", w.Code, w.Body.String())
	}
}

func TestLogs_Semaphore(t *testing.T) {
	s, _ := newTestServer(t)
	for i := 0; i < maxLogStreams; i++ {
		s.logStreams <- struct{}{}
	}
	if w := do(t, s.Routes(), "GET", "/api/logs/vpn-ovpn"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503 when all slots are busy", w.Code)
	}
	<-s.logStreams
	if w := do(t, s.Routes(), "GET", "/api/logs/vpn-ovpn"); w.Code != 200 {
		t.Errorf("code = %d after a slot freed", w.Code)
	}
	if len(s.logStreams) != maxLogStreams-1 {
		t.Errorf("slot leaked: %d in use", len(s.logStreams))
	}
}

// slowLogs emits one frame now and one after a delay, then blocks until
// closed.
type slowLogs struct {
	r      *io.PipeReader
	w      *io.PipeWriter
	closed chan struct{}
}

func newSlowLogs(delay time.Duration) *slowLogs {
	r, w := io.Pipe()
	sl := &slowLogs{r: r, w: w, closed: make(chan struct{})}
	go func() {
		_, _ = w.Write(makeFrame(1, []byte("first\n")))
		select {
		case <-time.After(delay):
			_, _ = w.Write(makeFrame(1, []byte("second\n")))
		case <-sl.closed:
		}
	}()
	return sl
}

func (s *slowLogs) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *slowLogs) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return s.r.Close()
}

// TestLogs_OutlivesWriteTimeout runs a real server with a short
// WriteTimeout: the SSE handler must lift the deadline. The fake stream is
// closed explicitly before ts.Close (and also by request cancellation via
// fakeDocker.LogsStream), so the test cannot hang on cleanup.
func TestLogs_OutlivesWriteTimeout(t *testing.T) {
	s, d := newTestServer(t)
	var (
		mu      sync.Mutex
		streams []*slowLogs
	)
	d.logs = func() io.ReadCloser {
		sl := newSlowLogs(300 * time.Millisecond)
		mu.Lock()
		streams = append(streams, sl)
		mu.Unlock()
		return sl
	}
	ts := httptest.NewUnstartedServer(s.Routes())
	ts.Config.WriteTimeout = 100 * time.Millisecond
	ts.Start()
	defer ts.Close()
	// Deferred after ts.Close, so it runs first: unblock the handler.
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, sl := range streams {
			_ = sl.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/logs/vpn-ovpn", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "localhost"
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	sc := bufio.NewScanner(resp.Body)
	var got []string
	for len(got) < 2 && sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: {"); ok {
			got = append(got, data)
		}
	}
	_ = resp.Body.Close()
	if len(got) != 2 || !strings.Contains(got[1], "second") {
		t.Fatalf("got %q, err %v; stream died at WriteTimeout", got, sc.Err())
	}

	// Disconnecting the client must end the handler and free its slot.
	deadline := time.Now().Add(5 * time.Second)
	for len(s.logStreams) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("handler did not return after the client disconnected")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// failingWriter fails every write, as a disconnected client does.
type failingWriter struct {
	h    http.Header
	code int
}

func (f *failingWriter) Header() http.Header       { return f.h }
func (f *failingWriter) WriteHeader(c int)         { f.code = c }
func (f *failingWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }
func (f *failingWriter) Flush()                    {}

func TestLogs_ClientGone(t *testing.T) {
	s, d := newTestServer(t)
	d.logs = framesReader(makeFrame(1, []byte("line\n")))
	r := httptest.NewRequest("GET", "/api/logs/vpn-ovpn", nil)
	r.SetPathValue("container", "vpn-ovpn")
	s.handleLogs(&failingWriter{h: http.Header{}}, r)
	if len(s.logStreams) != 0 {
		t.Error("slot not released")
	}
}

// noFlush has no Flush method at all.
type noFlush struct{ h http.Header }

func (n *noFlush) Header() http.Header         { return n.h }
func (n *noFlush) WriteHeader(int)             {}
func (n *noFlush) Write(b []byte) (int, error) { return len(b), nil }

func TestLogs_NoFlusher(t *testing.T) {
	s, d := newTestServer(t)
	d.logs = framesReader(makeFrame(1, []byte("line\n")))
	r := httptest.NewRequest("GET", "/api/logs/vpn-ovpn", nil)
	r.SetPathValue("container", "vpn-ovpn")
	s.handleLogs(&noFlush{h: http.Header{}}, r) // must return, not panic
}

// errReader yields some data and then a non-EOF error.
type errReader struct{ data []byte }

func (e *errReader) Read(p []byte) (int, error) {
	if len(e.data) > 0 {
		n := copy(p, e.data)
		e.data = e.data[n:]
		return n, nil
	}
	return 0, errors.New("read failed")
}
func (e *errReader) Close() error { return nil }

func TestLogs_ReadError(t *testing.T) {
	s, d := newTestServer(t)
	d.logs = func() io.ReadCloser { return &errReader{data: makeFrame(1, []byte("ok\n"))} }
	w := do(t, s.Routes(), "GET", "/api/logs/vpn-ovpn")
	if got := sseLines(t, w.Body.String()); len(got) != 1 {
		t.Errorf("lines = %q", got)
	}
}
