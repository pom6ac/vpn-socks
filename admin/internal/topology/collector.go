package topology

import (
	"context"
	"log"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
)

const refreshInterval = 15 * time.Second

// Execer runs a command inside a container and returns its output.
type Execer interface {
	Exec(ctx context.Context, name string, cmd []string) (string, error)
}

// Endpoint is an OpenVPN remote taken from the client config.
type Endpoint struct {
	Host  string `json:"host"`
	Port  string `json:"port"`
	Proto string `json:"proto"`
}

// Snapshot is the slowly-changing network facts gathered by the Collector.
type Snapshot struct {
	Remotes         []Endpoint `json:"remotes"`
	TunnelAddr      string     `json:"tunnel_addr"`
	ExitIP          string     `json:"exit_ip"`
	ListenerChecked bool       `json:"listener_checked"`
	ListenerOK      bool       `json:"listener_ok"`
	ListenerErr     string     `json:"listener_error,omitempty"`
	UpdatedAt       string     `json:"updated_at"`
}

// Collector periodically gathers the VPN server address, tunnel address,
// exit IP and SOCKS5 listener reachability.
type Collector struct {
	exec       Execer
	container  string
	ovpnConfig string
	socksAddr  string
	exitIP     func() string

	dial func(ctx context.Context, addr string) error

	mu   sync.RWMutex
	snap Snapshot
}

// NewCollector creates a Collector. container is the OpenVPN container name,
// ovpnConfig the config path inside it, socksAddr the SOCKS5 listener as
// reachable from admin (ovpn:1080 over the compose network). exitIP returns
// the last exit IP observed through the tunnel (health.Prober.ExitIP, which
// owns the only request to the exit IP service); it may be nil.
func NewCollector(exec Execer, container, ovpnConfig, socksAddr string, exitIP func() string) *Collector {
	if exitIP == nil {
		exitIP = func() string { return "" }
	}
	c := &Collector{
		exec:       exec,
		container:  container,
		ovpnConfig: ovpnConfig,
		socksAddr:  socksAddr,
		exitIP:     exitIP,
	}
	c.dial = dialTCP
	return c
}

// Snapshot returns the latest gathered facts.
func (c *Collector) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snap
}

// Run refreshes the snapshot on a schedule until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(refreshInterval)
	defer t.Stop()
	for {
		c.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Collector) refresh(ctx context.Context) {
	prev := c.Snapshot()
	s := Snapshot{
		Remotes:   prev.Remotes,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if c.exec != nil {
		// The config is read without a shell and immediately reduced to its
		// remote/proto/port directives: it also holds keys and certificates,
		// which are never logged, stored or returned.
		if out, err := c.exec.Exec(ctx, c.container, []string{"cat", c.ovpnConfig}); err != nil {
			log.Printf("topology: read ovpn remotes: %v", err)
		} else if r := ParseRemotes(FilterRemoteDirectives(out)); len(r) > 0 {
			s.Remotes = r
		}
		if out, err := c.exec.Exec(ctx, c.container, []string{"ip", "-o", "addr", "show", "dev", "tun0"}); err == nil {
			s.TunnelAddr = ParseTunAddr(out)
		}
	}

	if err := c.dial(ctx, c.socksAddr); err != nil {
		s.ListenerErr = err.Error()
	} else {
		s.ListenerOK = true
	}
	s.ListenerChecked = true

	// Without a tunnel there is no VPN exit to show.
	if s.TunnelAddr != "" {
		s.ExitIP = c.exitIP()
	}

	c.mu.Lock()
	c.snap = s
	c.mu.Unlock()
}

func dialTCP(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// FilterRemoteDirectives keeps only the remote/proto/port lines of an
// OpenVPN config, dropping everything else (inline keys, certificates).
func FilterRemoteDirectives(cfg string) string {
	var b strings.Builder
	for _, line := range strings.Split(cfg, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch strings.ToLower(f[0]) {
		case "remote", "proto", "port":
			b.WriteString(strings.Join(f, " "))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// ParseRemotes extracts remotes from OpenVPN config lines. Port and proto
// fall back to the global `port`/`proto` directives, then to 1194/udp.
func ParseRemotes(cfg string) []Endpoint {
	defPort, defProto := "1194", "udp"
	type raw struct{ host, port, proto string }
	var remotes []raw
	for _, line := range strings.Split(cfg, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch strings.ToLower(f[0]) {
		case "remote":
			r := raw{host: f[1]}
			if len(f) > 2 {
				r.port = f[2]
			}
			if len(f) > 3 {
				r.proto = f[3]
			}
			remotes = append(remotes, r)
		case "port":
			defPort = f[1]
		case "proto":
			defProto = f[1]
		}
	}
	out := make([]Endpoint, 0, len(remotes))
	for _, r := range remotes {
		e := Endpoint{Host: r.host, Port: r.port, Proto: r.proto}
		if e.Port == "" {
			e.Port = defPort
		}
		if e.Proto == "" {
			e.Proto = defProto
		}
		e.Proto = normalizeProto(e.Proto)
		out = append(out, e)
	}
	return out
}

// normalizeProto maps OpenVPN proto variants (tcp-client, udp4, ...) to tcp/udp.
func normalizeProto(p string) string {
	p = strings.ToLower(p)
	switch {
	case strings.HasPrefix(p, "tcp"):
		return "tcp"
	case strings.HasPrefix(p, "udp"):
		return "udp"
	}
	return p
}

var inetRe = regexp.MustCompile(`\binet (\S+)`)

// ParseTunAddr returns the IPv4 address (with prefix when present) from
// `ip -o addr show dev tun0` output, or "" if the interface has none.
func ParseTunAddr(out string) string {
	m := inetRe.FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return m[1]
}
