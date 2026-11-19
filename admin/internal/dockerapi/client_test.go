package dockerapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, "tcp", ts.Listener.Addr().String())
		},
	}
	return NewClientWithHTTP(&http.Client{Transport: tr})
}

func makeDemuxFrame(streamType byte, payload []byte) []byte {
	frame := make([]byte, 8+len(payload))
	frame[0] = streamType
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], payload)
	return frame
}

func TestDemuxDockerStream_SingleFrame(t *testing.T) {
	raw := makeDemuxFrame(1, []byte("hello"))
	if got := demuxDockerStream(raw); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestDemuxDockerStream_MultipleFrames(t *testing.T) {
	raw := append(makeDemuxFrame(1, []byte("foo")), makeDemuxFrame(2, []byte("bar"))...)
	if got := demuxDockerStream(raw); got != "foobar" {
		t.Errorf("got %q, want %q", got, "foobar")
	}
}

func TestDemuxDockerStream_Empty(t *testing.T) {
	if got := demuxDockerStream(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestDemuxDockerStream_TruncatedPayload(t *testing.T) {
	frame := makeDemuxFrame(1, []byte("hello"))
	truncated := frame[:10]
	got := demuxDockerStream(truncated)
	if got != "he" { // partial payload is emitted once, not duplicated
		t.Errorf("got %q, want %q", got, "he")
	}
}

func TestDemuxDockerStream_LessThanHeader(t *testing.T) {
	got := demuxDockerStream([]byte{1, 2, 3})
	if got != "\x01\x02\x03" {
		t.Errorf("got %q, want trailing bytes", got)
	}
}

func TestDemuxDockerStream_ZeroPayload(t *testing.T) {
	raw := append(makeDemuxFrame(1, nil), makeDemuxFrame(2, []byte("data"))...)
	if got := demuxDockerStream(raw); got != "data" {
		t.Errorf("got %q, want %q", got, "data")
	}
}

func TestInspectContainer(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/vpn-ovpn/json" {
			t.Errorf("path = %q, want /containers/vpn-ovpn/json", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"Name": "/vpn-ovpn",
			"RestartCount": 3,
			"State": {
				"Status": "running",
				"StartedAt": "2024-08-12T10:00:00Z",
				"ExitCode": 0
			}
		}`)
	}
	client := newTestClient(t, handler)

	info, err := client.InspectContainer(context.Background(), "vpn-ovpn")
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	if info.Name != "vpn-ovpn" {
		t.Errorf("Name = %q, want %q", info.Name, "vpn-ovpn")
	}
	if info.State != "running" {
		t.Errorf("State = %q, want %q", info.State, "running")
	}
	if info.Status != "running" {
		t.Errorf("Status = %q, want %q", info.Status, "running")
	}
	if info.RestartCount != 3 {
		t.Errorf("RestartCount = %d, want 3", info.RestartCount)
	}
	if info.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", info.ExitCode)
	}
	if info.StartedAt != "2024-08-12T10:00:00Z" {
		t.Errorf("StartedAt = %q", info.StartedAt)
	}
}

func TestListContainers(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(r.URL.Path, "/")
		name := parts[2]
		fmt.Fprintf(w, `{"Name":"/%s","RestartCount":0,"State":{"Status":"running","StartedAt":"2024-01-01T00:00:00Z","ExitCode":0}}`, name)
	}
	client := newTestClient(t, handler)

	containers, err := client.ListContainers(context.Background(), []string{"vpn-ovpn", "vpn-socks"})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(containers) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(containers))
	}
	if containers[0].Name != "vpn-ovpn" {
		t.Errorf("containers[0].Name = %q", containers[0].Name)
	}
	if containers[1].Name != "vpn-socks" {
		t.Errorf("containers[1].Name = %q", containers[1].Name)
	}
}

func TestRestartContainer(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if !strings.HasPrefix(r.URL.Path, "/containers/") || !strings.HasSuffix(r.URL.Path, "/restart") {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(204)
	}
	client := newTestClient(t, handler)

	if err := client.RestartContainer(context.Background(), "vpn-ovpn"); err != nil {
		t.Fatalf("RestartContainer: %v", err)
	}
}

func TestRestartContainer_Error(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}
	client := newTestClient(t, handler)

	if err := client.RestartContainer(context.Background(), "vpn-ovpn"); err == nil {
		t.Error("expected error on 500")
	}
}

func TestSignalContainer(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if !strings.Contains(r.URL.RawQuery, "signal=SIGUSR1") {
			t.Errorf("query = %q, expected signal=SIGUSR1", r.URL.RawQuery)
		}
		w.WriteHeader(204)
	}
	client := newTestClient(t, handler)

	if err := client.SignalContainer(context.Background(), "vpn-ovpn", "SIGUSR1"); err != nil {
		t.Fatalf("SignalContainer: %v", err)
	}
}

func TestLogsStream(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/logs") {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(200)
		_, _ = w.Write(makeDemuxFrame(1, []byte("line one\n")))
		_, _ = w.Write(makeDemuxFrame(2, []byte("line two\n")))
	}
	client := newTestClient(t, handler)

	rc, err := client.LogsStream(context.Background(), "vpn-ovpn", time.Time{})
	if err != nil {
		t.Fatalf("LogsStream: %v", err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(string(data), "line one") || !strings.Contains(string(data), "line two") {
		t.Errorf("unexpected data: %q", string(data))
	}
}

func TestLogsStream_WithSince(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "since=") {
			t.Errorf("query missing since: %q", r.URL.RawQuery)
		}
		w.WriteHeader(200)
	}
	client := newTestClient(t, handler)

	rc, err := client.LogsStream(context.Background(), "vpn-ovpn", time.Now().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("LogsStream: %v", err)
	}
	rc.Close()
}

func TestLogsStream_Error(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		fmt.Fprintf(w, "container not found")
	}
	client := newTestClient(t, handler)

	_, err := client.LogsStream(context.Background(), "nonexistent", time.Time{})
	if err == nil {
		t.Error("expected error on 404")
	}
}

func TestExecProcNetDev(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/exec") {
			if strings.Contains(r.URL.Path, "/containers/") {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"Id":"exec123"}`)
			} else if strings.Contains(r.URL.Path, "/exec/") {
				procNetDev := "  tun0: 1000 10 0 0 0 0 0 0 2000 20 0 0 0 0 0 0\n"
				_, _ = w.Write(makeDemuxFrame(1, []byte(procNetDev)))
			}
		}
	}
	client := newTestClient(t, handler)

	output, err := client.ExecProcNetDev(context.Background(), "vpn-ovpn")
	if err != nil {
		t.Fatalf("ExecProcNetDev: %v", err)
	}
	if !strings.Contains(output, "tun0") {
		t.Errorf("output = %q, expected tun0", output)
	}
}

func TestExecProcNetDev_Error(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}
	client := newTestClient(t, handler)

	_, err := client.ExecProcNetDev(context.Background(), "vpn-ovpn")
	if err == nil {
		t.Error("expected error on 500")
	}
}

func TestEvents(t *testing.T) {
	done := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprintf(w, `{"Action":"start","Actor":{"Attributes":{"name":"vpn-ovpn"}}}`+"\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-done
	})
	client := newTestClient(t, handler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	msgCh, errCh := client.Events(ctx, []string{"vpn-ovpn", "vpn-socks"})

	msg, ok := <-msgCh
	if !ok {
		t.Fatal("message channel closed early")
	}
	if msg.Action != "start" {
		t.Errorf("Action = %q, want start", msg.Action)
	}
	if msg.Actor.Attributes["name"] != "vpn-ovpn" {
		t.Errorf("name = %q, want vpn-ovpn", msg.Actor.Attributes["name"])
	}

	close(done)
	cancel()

	for range msgCh {
	}
	for range errCh {
	}
}

func TestEvents_FiltersInQuery(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "filters=") {
			t.Errorf("query missing filters: %q", r.URL.RawQuery)
		}
		if !strings.Contains(r.URL.RawQuery, "type=container") {
			t.Errorf("query missing type=container: %q", r.URL.RawQuery)
		}
		w.WriteHeader(200)
	})
	client := newTestClient(t, handler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	msgCh, _ := client.Events(ctx, []string{"vpn-ovpn"})
	cancel()
	for range msgCh {
	}
}

func TestGet_ErrorStatus(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		fmt.Fprintf(w, "not found")
	})
	client := newTestClient(t, handler)

	_, err := client.get(context.Background(), "/test")
	if err == nil {
		t.Error("expected error on 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should contain 404: %v", err)
	}
}

func TestDo_WithBody(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte("test")) {
			t.Errorf("body = %q, expected to contain 'test'", string(body))
		}
		w.WriteHeader(200)
	})
	client := newTestClient(t, handler)

	resp, err := client.do(context.Background(), http.MethodPost, "/test", bytes.NewReader([]byte(`{"test":true}`)))
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
}

func TestInspectContainer_PortsAndNetworkMode(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"Name": "/vpn-ovpn",
			"State": {"Status": "running"},
			"HostConfig": {"NetworkMode": "container:abc"},
			"NetworkSettings": {"Ports": {
				"1080/tcp": [{"HostIp": "127.0.0.1", "HostPort": "1080"}],
				"9000/tcp": null,
				"53/udp": [{"HostIp": "0.0.0.0", "HostPort": "5353"}, {"HostIp": "::", "HostPort": "5353"}]
			}}
		}`)
	}
	client := newTestClient(t, handler)

	info, err := client.InspectContainer(context.Background(), "vpn-ovpn")
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	if info.NetworkMode != "container:abc" {
		t.Errorf("NetworkMode = %q", info.NetworkMode)
	}
	want := []PortBinding{
		{ContainerPort: "1080", Proto: "tcp", HostIP: "127.0.0.1", HostPort: "1080"},
		{ContainerPort: "53", Proto: "udp", HostIP: "0.0.0.0", HostPort: "5353"},
		{ContainerPort: "53", Proto: "udp", HostIP: "::", HostPort: "5353"},
	}
	if fmt.Sprint(info.Ports) != fmt.Sprint(want) {
		t.Errorf("Ports = %+v, want %+v", info.Ports, want)
	}
}

func TestExec_SendsCmd(t *testing.T) {
	var gotBody string
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/containers/vpn-ovpn/exec"):
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			fmt.Fprint(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
			_, _ = w.Write(makeDemuxFrame(1, []byte("hello\n")))
		}
	}
	client := newTestClient(t, handler)

	out, err := client.Exec(context.Background(), "vpn-ovpn", []string{"echo", "hello"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "hello\n" {
		t.Errorf("out = %q", out)
	}
	if !strings.Contains(gotBody, `"Cmd":["echo","hello"]`) {
		t.Errorf("exec create body = %s", gotBody)
	}
}
