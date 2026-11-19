package dockerapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	maxRetries     = 3
	backoffInitial = 500 * time.Millisecond
	execTimeout    = 10 * time.Second
	// requestTimeout bounds every non-streaming Docker API call.
	requestTimeout = 30 * time.Second
	// eventsBackoff is the pause before re-subscribing to /events after the
	// stream ends for any reason.
	eventsBackoff = 5 * time.Second
	socketPath    = "/var/run/docker.sock"
)

// ContainerInfo holds the relevant fields extracted from Docker inspect.
type ContainerInfo struct {
	Name         string        `json:"name"`
	State        string        `json:"state"`
	StartedAt    string        `json:"started_at"`
	RestartCount int           `json:"restart_count"`
	ExitCode     int           `json:"exit_code"`
	Status       string        `json:"status"`
	NetworkMode  string        `json:"network_mode,omitempty"`
	Ports        []PortBinding `json:"ports,omitempty"`
}

// PortBinding is a container port published on the host.
type PortBinding struct {
	ContainerPort string `json:"container_port"`
	Proto         string `json:"proto"`
	HostIP        string `json:"host_ip"`
	HostPort      string `json:"host_port"`
}

// Client is a minimal Docker Engine API client over a unix socket.
type Client struct {
	// http is used for request/response calls and has an overall timeout.
	http *http.Client
	// stream is used for long-lived streams (logs, events) and has none.
	stream *http.Client

	retryBackoff  time.Duration
	eventsBackoff time.Duration
}

// NewClient creates a Docker API client backed by the local docker socket.
// The socket is dialled lazily, so construction never fails.
func NewClient() *Client {
	return NewClientWithTransport(&http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	})
}

// NewClientWithTransport creates a client that sends requests through rt.
// It is mainly useful for tests that point the client at a fake daemon.
func NewClientWithTransport(rt http.RoundTripper) *Client {
	return &Client{
		http:          &http.Client{Transport: rt, Timeout: requestTimeout},
		stream:        &http.Client{Transport: rt},
		retryBackoff:  backoffInitial,
		eventsBackoff: eventsBackoff,
	}
}

// ListContainers returns info for the given container names.
func (c *Client) ListContainers(ctx context.Context, names []string) ([]ContainerInfo, error) {
	result := make([]ContainerInfo, 0, len(names))
	for _, n := range names {
		info, err := c.InspectContainer(ctx, n)
		if err != nil {
			return nil, err
		}
		result = append(result, info)
	}
	return result, nil
}

// inspectResponse is the subset of the Docker inspect JSON we need.
type inspectResponse struct {
	Name         string `json:"Name"`
	RestartCount int    `json:"RestartCount"`
	State        struct {
		Status    string `json:"Status"`
		StartedAt string `json:"StartedAt"`
		ExitCode  int    `json:"ExitCode"`
	} `json:"State"`
	HostConfig struct {
		NetworkMode string `json:"NetworkMode"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
}

// portBindings flattens Docker's "1080/tcp" -> [{HostIp, HostPort}] map into
// a sorted list, skipping exposed-but-unpublished ports.
func (r inspectResponse) portBindings() []PortBinding {
	var out []PortBinding
	for key, binds := range r.NetworkSettings.Ports {
		port, proto, _ := strings.Cut(key, "/")
		for _, b := range binds {
			out = append(out, PortBinding{ContainerPort: port, Proto: proto, HostIP: b.HostIP, HostPort: b.HostPort})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HostPort != out[j].HostPort {
			return out[i].HostPort < out[j].HostPort
		}
		return out[i].HostIP < out[j].HostIP
	})
	return out
}

// InspectContainer returns info for a single container by name.
func (c *Client) InspectContainer(ctx context.Context, name string) (ContainerInfo, error) {
	var info ContainerInfo
	err := c.withRetry(ctx, func() error {
		body, err := c.get(ctx, "/containers/"+url.PathEscape(name)+"/json")
		if err != nil {
			return err
		}
		var resp inspectResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return err
		}
		info = ContainerInfo{
			Name:         strings.TrimPrefix(resp.Name, "/"),
			State:        resp.State.Status,
			StartedAt:    resp.State.StartedAt,
			RestartCount: resp.RestartCount,
			ExitCode:     resp.State.ExitCode,
			Status:       resp.State.Status,
			NetworkMode:  resp.HostConfig.NetworkMode,
			Ports:        resp.portBindings(),
		}
		return nil
	})
	return info, err
}

// RestartContainer restarts a container by name.
func (c *Client) RestartContainer(ctx context.Context, name string) error {
	return c.withRetry(ctx, func() error {
		return c.post(ctx, "/containers/"+url.PathEscape(name)+"/restart?t=10")
	})
}

// SignalContainer sends a signal (e.g. SIGUSR1) to a container by name.
func (c *Client) SignalContainer(ctx context.Context, name, signal string) error {
	return c.withRetry(ctx, func() error {
		return c.post(ctx, "/containers/"+url.PathEscape(name)+"/kill?signal="+url.QueryEscape(signal))
	})
}

// logsPath builds the follow-logs URL. With a since cursor the stream resumes
// from that instant (nanosecond precision) instead of replaying the tail.
func logsPath(name string, since time.Time) string {
	path := "/containers/" + url.PathEscape(name) + "/logs?follow=true&stdout=true&stderr=true&timestamps=true"
	if since.IsZero() {
		return path + "&tail=100"
	}
	return path + fmt.Sprintf("&since=%d.%09d", since.Unix(), since.Nanosecond())
}

// LogsStream returns a follow-stream of container logs. The caller must close
// the returned reader when done.
func (c *Client) LogsStream(ctx context.Context, name string, since time.Time) (io.ReadCloser, error) {
	path := logsPath(name, since)
	var rc io.ReadCloser
	err := c.withRetry(ctx, func() error {
		resp, err := c.doWith(ctx, c.stream, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		if err := checkStatus(resp, "logs "+name); err != nil {
			return err
		}
		rc = resp.Body
		return nil
	})
	return rc, err
}

// execCreateResponse is the response from the exec create endpoint.
type execCreateResponse struct {
	ID string `json:"Id"`
}

// ExecProcNetDev runs `cat /proc/net/dev` inside the given container and
// returns the raw output (demuxed from the hijacked stream).
func (c *Client) ExecProcNetDev(ctx context.Context, name string) (string, error) {
	return c.Exec(ctx, name, []string{"cat", "/proc/net/dev"})
}

// Exec runs cmd inside the given container and returns its combined
// stdout/stderr (demuxed from the hijacked stream). The exit code is ignored.
func (c *Client) Exec(ctx context.Context, name string, cmd []string) (string, error) {
	var output string
	err := c.withRetry(ctx, func() error {
		execCtx, cancel := context.WithTimeout(ctx, execTimeout)
		defer cancel()

		createBody, err := json.Marshal(map[string]any{
			"AttachStdout": true,
			"AttachStderr": true,
			"Cmd":          cmd,
		})
		if err != nil {
			return err
		}
		resp, err := c.do(execCtx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/exec", bytes.NewReader(createBody))
		if err != nil {
			return err
		}
		if err := checkStatus(resp, "exec create "+name); err != nil {
			return err
		}
		var execResp execCreateResponse
		err = json.NewDecoder(resp.Body).Decode(&execResp)
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("exec create %s: decode: %w", name, err)
		}
		if execResp.ID == "" {
			return fmt.Errorf("exec create %s: empty exec id", name)
		}

		startResp, err := c.do(execCtx, http.MethodPost, "/exec/"+url.PathEscape(execResp.ID)+"/start",
			strings.NewReader(`{"Detach":false,"Tty":false}`))
		if err != nil {
			return err
		}
		if err := checkStatus(startResp, "exec start "+name); err != nil {
			return err
		}
		defer func() { _ = startResp.Body.Close() }()

		raw, err := io.ReadAll(startResp.Body)
		if err != nil {
			return err
		}
		output = demuxDockerStream(raw)
		return nil
	})
	return output, err
}

// EventMessage is a minimal Docker event message.
type EventMessage struct {
	Action string     `json:"Action"`
	Actor  EventActor `json:"Actor"`
}

// EventActor holds the actor attributes in a Docker event.
type EventActor struct {
	Attributes map[string]string `json:"Attributes"`
}

// Events subscribes to Docker container events filtered to the given names.
// The subscription is re-established automatically: after the stream ends
// for any reason (dial error, non-200 status, decode error or clean EOF) the
// error is reported on the error channel and the client waits for a backoff
// before reconnecting. Both channels are closed once ctx is cancelled.
func (c *Client) Events(ctx context.Context, names []string) (<-chan EventMessage, <-chan error) {
	msgCh := make(chan EventMessage, 16)
	errCh := make(chan error)

	filters := map[string]map[string]bool{"container": {}}
	for _, n := range names {
		filters["container"][n] = true
	}
	filtersJSON, _ := json.Marshal(filters) // map[string]map[string]bool cannot fail
	path := "/events?type=container&filters=" + url.QueryEscape(string(filtersJSON))

	go func() {
		defer close(msgCh)
		defer close(errCh)
		for {
			err := c.streamEvents(ctx, path, msgCh)
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				err = errors.New("docker events stream closed")
			}
			select {
			case errCh <- err:
			case <-ctx.Done():
				return
			}
			select {
			case <-time.After(c.eventsBackoff):
			case <-ctx.Done():
				return
			}
		}
	}()

	return msgCh, errCh
}

// streamEvents reads one /events subscription until it ends. A nil return
// means the daemon closed the stream cleanly.
func (c *Client) streamEvents(ctx context.Context, path string, msgCh chan<- EventMessage) error {
	resp, err := c.doWith(ctx, c.stream, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if err := checkStatus(resp, "events"); err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	dec := json.NewDecoder(resp.Body)
	for {
		var msg EventMessage
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		select {
		case msgCh <- msg:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// --- internal helpers ---

// checkStatus returns an error (and closes the body) for HTTP status >= 300.
func checkStatus(resp *http.Response, what string) error {
	if resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	_ = resp.Body.Close()
	return fmt.Errorf("%s: HTTP %d: %s", what, resp.StatusCode, strings.TrimSpace(string(body)))
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if err := checkStatus(resp, "GET "+path); err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}

func (c *Client) post(ctx context.Context, path string) error {
	resp, err := c.do(ctx, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	if err := checkStatus(resp, "POST "+path); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	return c.doWith(ctx, c.http, method, path, body)
}

func (c *Client) doWith(ctx context.Context, hc *http.Client, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return hc.Do(req)
}

// withRetry retries fn with exponential backoff, but only when the Docker
// socket could not be dialled: once a request reached the daemon, repeating a
// non-idempotent call (restart, exec) is not safe.
func (c *Client) withRetry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if err = fn(); err == nil || !isRetryable(err) || attempt == maxRetries-1 {
			return err
		}
		backoff := c.retryBackoff * (1 << attempt)
		log.Printf("docker api dial error (attempt %d/%d): %v; retrying in %v", attempt+1, maxRetries, err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

// isRetryable reports whether err is a failure to dial the Docker socket.
func isRetryable(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// demuxDockerStream parses the Docker multiplexed stream format and returns
// the concatenated payload as a string.
func demuxDockerStream(raw []byte) string {
	var buf bytes.Buffer
	i := 0
	for i+8 <= len(raw) {
		payloadLen := int(binary.BigEndian.Uint32(raw[i+4 : i+8]))
		i += 8
		if i+payloadLen > len(raw) {
			buf.Write(raw[i:])
			return buf.String()
		}
		buf.Write(raw[i : i+payloadLen])
		i += payloadLen
	}
	buf.Write(raw[i:])
	return buf.String()
}
