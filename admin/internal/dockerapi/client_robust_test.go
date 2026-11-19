package dockerapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsRetryable_OnlyDialErrors(t *testing.T) {
	dialErr := &net.OpError{Op: "dial", Net: "unix", Err: errors.New("connect: no such file or directory")}
	readErr := &net.OpError{Op: "read", Net: "unix", Err: errors.New("connection reset by peer")}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"dial", dialErr, true},
		{"wrapped dial", fmt.Errorf("Get x: %w", dialErr), true},
		{"read", readErr, false},
		{"EOF text", errors.New("unexpected EOF"), false},
		{"refused text", errors.New("connection refused"), false},
		{"http status", errors.New("HTTP 500"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryable(tt.err); got != tt.want {
				t.Errorf("isRetryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// flakyDialTransport fails the first n dials with a dial OpError.
func flakyDialTransport(addr string, n int32, dials *int32) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if atomic.AddInt32(dials, 1) <= n {
				return nil, &net.OpError{Op: "dial", Net: "unix", Err: errors.New("no such file or directory")}
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		},
		DisableKeepAlives: true,
	}
}

func TestWithRetry_RetriesDialErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"Name":"/vpn-ovpn","State":{"Status":"running"}}`)
	}))
	defer ts.Close()
	var dials int32
	c := NewClientWithHTTP(&http.Client{Transport: flakyDialTransport(ts.Listener.Addr().String(), 2, &dials)})

	info, err := c.InspectContainer(context.Background(), "vpn-ovpn")
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	if info.State != "running" {
		t.Errorf("state = %q", info.State)
	}
	if got := atomic.LoadInt32(&dials); got != 3 {
		t.Errorf("dials = %d, want 3", got)
	}
}

func TestWithRetry_GivesUpAfterMaxRetries(t *testing.T) {
	var dials int32
	c := NewClientWithHTTP(&http.Client{Transport: flakyDialTransport("127.0.0.1:1", 100, &dials)})
	if err := c.RestartContainer(context.Background(), "x"); err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(&dials); got != maxRetries {
		t.Errorf("dials = %d, want %d", got, maxRetries)
	}
}

func TestWithRetry_DoesNotRetryAfterRequestReachedDaemon(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no hijacker")
			return
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close() // client sees EOF
	})
	if err := c.RestartContainer(context.Background(), "vpn-ovpn"); err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want 1 (restart is not idempotent)", got)
	}
}

func TestWithRetry_CancelledDuringBackoff(t *testing.T) {
	var dials int32
	c := NewClientWithHTTP(&http.Client{Transport: flakyDialTransport("127.0.0.1:1", 100, &dials)})
	c.retryBackoff = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.SignalContainer(ctx, "x", "SIGUSR1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

func TestNewClient_HasRequestTimeout(t *testing.T) {
	c := NewClient()
	if c.http.Timeout != requestTimeout {
		t.Errorf("request timeout = %v, want %v", c.http.Timeout, requestTimeout)
	}
	if c.stream.Timeout != 0 {
		t.Errorf("stream client must not time out, got %v", c.stream.Timeout)
	}
	if c.eventsBackoff != eventsBackoff || c.retryBackoff != backoffInitial {
		t.Error("unexpected default backoffs")
	}
}

func TestLogsPath(t *testing.T) {
	p := logsPath("vpn-ovpn", time.Time{})
	if !strings.Contains(p, "tail=100") || strings.Contains(p, "since=") {
		t.Errorf("no-since path = %q", p)
	}
	since := time.Unix(1700000000, 5).UTC()
	p = logsPath("vpn-ovpn", since)
	if !strings.Contains(p, "since=1700000000.000000005") {
		t.Errorf("since path = %q, want nanosecond since", p)
	}
	if strings.Contains(p, "tail=") {
		t.Errorf("since path must not set tail: %q", p)
	}
}

func TestExec_CreateHTTPError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/start") {
			t.Error("start must not be called after failed create")
		}
		http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
	})
	_, err := c.Exec(context.Background(), "nope", []string{"true"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("err = %v, want HTTP 404", err)
	}
}

func TestExec_EmptyID(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/start") {
			t.Error("start must not be called with empty id")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"Id":""}`)
	})
	_, err := c.Exec(context.Background(), "vpn-ovpn", []string{"true"})
	if err == nil || !strings.Contains(err.Error(), "empty exec id") {
		t.Fatalf("err = %v, want empty exec id", err)
	}
}

func TestExec_BadJSON(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `not json`)
	})
	if _, err := c.Exec(context.Background(), "vpn-ovpn", []string{"true"}); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestExec_StartHTTPError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/start") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"Id":"abc"}`)
	})
	_, err := c.Exec(context.Background(), "vpn-ovpn", []string{"true"})
	if err == nil || !strings.Contains(err.Error(), "exec start") {
		t.Fatalf("err = %v, want exec start error", err)
	}
}

func TestSignalContainer_Error(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "conflict", http.StatusConflict)
	})
	if err := c.SignalContainer(context.Background(), "vpn-ovpn", "SIGUSR1"); err == nil {
		t.Fatal("expected error")
	}
}

func TestInspectContainer_BadJSON(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{`)
	})
	if _, err := c.InspectContainer(context.Background(), "x"); err == nil {
		t.Fatal("expected error")
	}
}

func TestListContainers_Error(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	if _, err := c.ListContainers(context.Background(), []string{"a"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestEvents_HTTP500_BacksOffWithoutHotLoop(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "daemon error", http.StatusInternalServerError)
	})
	c.eventsBackoff = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	msgs, errs := c.Events(ctx, []string{"vpn-ovpn"})

	var got []error
	deadline := time.After(350 * time.Millisecond)
loop:
	for {
		select {
		case err := <-errs:
			got = append(got, err)
		case <-deadline:
			break loop
		}
	}
	cancel()
	for range msgs {
	}

	if len(got) == 0 || !strings.Contains(got[0].Error(), "HTTP 500") {
		t.Fatalf("errors = %v, want HTTP 500", got)
	}
	// 350ms with a 100ms backoff allows ~4 attempts; a hot loop would make
	// thousands.
	if n := atomic.LoadInt32(&calls); n < 2 || n > 5 {
		t.Errorf("calls = %d, want 2..5", n)
	}
}

func TestEvents_CleanEOFReconnects(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = fmt.Fprintln(w, `{"Action":"die","Actor":{"Attributes":{"name":"vpn-ovpn"}}}`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	msgs, errs := c.Events(ctx, []string{"vpn-ovpn"})

	if m := <-msgs; m.Action != "die" {
		t.Fatalf("action = %q", m.Action)
	}
	if err := <-errs; err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("err = %v, want stream closed", err)
	}
	if m := <-msgs; m.Action != "die" {
		t.Fatalf("second action = %q", m.Action)
	}
	cancel()
	for range msgs {
	}
	if atomic.LoadInt32(&calls) < 2 {
		t.Error("expected a reconnect")
	}
}

func TestEvents_DecodeError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{garbage`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, errs := c.Events(ctx, nil)
	if err := <-errs; err == nil {
		t.Fatal("expected decode error")
	}
}

// TestEvents_NoGoroutineLeakOnCancel checks that the events goroutine exits
// when ctx is cancelled even though nobody drains the error channel.
func TestEvents_NoGoroutineLeakOnCancel(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	})
	c.eventsBackoff = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	msgs, errs := c.Events(ctx, nil)

	time.Sleep(50 * time.Millisecond) // goroutine is now blocked sending on errs
	cancel()

	done := make(chan struct{})
	go func() {
		for range msgs {
		}
		for range errs {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("events goroutine did not exit after cancel")
	}
}

func TestEvents_CancelWhileStreaming(t *testing.T) {
	release := make(chan struct{})
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < 32; i++ {
			_, _ = fmt.Fprintln(w, `{"Action":"start","Actor":{"Attributes":{"name":"a"}}}`)
		}
		w.(http.Flusher).Flush()
		<-release
	})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	msgs, _ := c.Events(ctx, []string{"a"})
	<-msgs
	cancel() // msg buffer (16) is full; sender must observe ctx.Done
	done := make(chan struct{})
	go func() {
		for range msgs {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("events goroutine did not exit")
	}
}
