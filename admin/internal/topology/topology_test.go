package topology

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/pom6ac/vpn-socks/admin/internal/dockerapi"
	"github.com/pom6ac/vpn-socks/admin/internal/health"
)

func TestParseRemotes(t *testing.T) {
	cfg := `
proto tcp-client
port 443
remote vpn1.example.com
remote 203.0.113.10 1194 udp4
  remote vpn2.example.com 8443
remote-cert-tls server
`
	got := ParseRemotes(cfg)
	want := []Endpoint{
		{Host: "vpn1.example.com", Port: "443", Proto: "tcp"},
		{Host: "203.0.113.10", Port: "1194", Proto: "udp"},
		{Host: "vpn2.example.com", Port: "8443", Proto: "tcp"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseRemotes = %+v, want %+v", got, want)
	}
}

func TestParseRemotes_Defaults(t *testing.T) {
	got := ParseRemotes("remote 1.2.3.4\n")
	want := []Endpoint{{Host: "1.2.3.4", Port: "1194", Proto: "udp"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseRemotes = %+v, want %+v", got, want)
	}
	if got := ParseRemotes(""); len(got) != 0 {
		t.Errorf("empty config: %+v", got)
	}
}

func TestParseTunAddr(t *testing.T) {
	cases := map[string]string{
		"5: tun0    inet 10.8.0.6/24 brd 10.8.0.255 scope global tun0\\       valid_lft forever": "10.8.0.6/24",
		"5: tun0    inet 10.8.0.6 peer 10.8.0.5/32 scope global tun0":                            "10.8.0.6",
		"5: tun0    inet6 fe80::1/64 scope link":                                                 "",
		`Device "tun0" does not exist.`:                                                          "",
	}
	for in, want := range cases {
		if got := ParseTunAddr(in); got != want {
			t.Errorf("ParseTunAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

type fakeExec struct {
	out map[string]string
	err error
}

func (f fakeExec) Exec(_ context.Context, _ string, cmd []string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.out[cmd[0]], nil
}

func newTestCollector(exec Execer) *Collector {
	c := NewCollector(exec, "vpn-ovpn", "/vpn/config.ovpn", "127.0.0.1:1", func() string { return "203.0.113.7" })
	c.dial = func(context.Context, string) error { return nil }
	return c
}

func TestCollectorRefresh(t *testing.T) {
	c := newTestCollector(fakeExec{out: map[string]string{
		"cat": "remote 203.0.113.10 1194\n",
		"ip":  "5: tun0    inet 10.8.0.6/24 scope global tun0",
	}})
	c.refresh(context.Background())

	s := c.Snapshot()
	if len(s.Remotes) != 1 || s.Remotes[0].Host != "203.0.113.10" {
		t.Errorf("Remotes = %+v", s.Remotes)
	}
	if s.TunnelAddr != "10.8.0.6/24" {
		t.Errorf("TunnelAddr = %q", s.TunnelAddr)
	}
	if s.ExitIP != "203.0.113.7" {
		t.Errorf("ExitIP = %q", s.ExitIP)
	}
	if !s.ListenerChecked || !s.ListenerOK {
		t.Errorf("listener = %+v", s)
	}
	if s.UpdatedAt == "" {
		t.Error("UpdatedAt empty")
	}
}

func TestCollectorRefresh_Failures(t *testing.T) {
	c := newTestCollector(fakeExec{out: map[string]string{
		"cat": "remote 1.2.3.4\n",
		"ip":  "5: tun0    inet 10.8.0.6/24",
	}})
	c.refresh(context.Background())

	// Exec now fails and the tunnel is gone: remotes are kept from the last
	// good read, exit IP is dropped, listener error is reported.
	c.exec = fakeExec{err: errors.New("boom")}
	c.dial = func(context.Context, string) error { return errors.New("refused") }
	c.refresh(context.Background())

	s := c.Snapshot()
	if len(s.Remotes) != 1 {
		t.Errorf("Remotes should be kept, got %+v", s.Remotes)
	}
	if s.TunnelAddr != "" || s.ExitIP != "" {
		t.Errorf("TunnelAddr=%q ExitIP=%q, want empty", s.TunnelAddr, s.ExitIP)
	}
	if s.ListenerOK || s.ListenerErr != "refused" {
		t.Errorf("listener = %+v", s)
	}
}

func TestCollectorRefresh_ExitIPFromSource(t *testing.T) {
	ip := "203.0.113.7"
	c := NewCollector(fakeExec{out: map[string]string{"ip": "inet 10.8.0.6/24"}}, "vpn-ovpn", "/x", "127.0.0.1:1",
		func() string { return ip })
	c.dial = func(context.Context, string) error { return nil }
	c.refresh(context.Background())
	if got := c.Snapshot().ExitIP; got != "203.0.113.7" {
		t.Errorf("ExitIP = %q", got)
	}
	ip = "198.51.100.1"
	c.refresh(context.Background())
	if got := c.Snapshot().ExitIP; got != "198.51.100.1" {
		t.Errorf("ExitIP = %q, want the latest value from the source", got)
	}
}

func TestCollectorRefresh_NilExitIPSource(t *testing.T) {
	c := NewCollector(fakeExec{out: map[string]string{"ip": "inet 10.8.0.6/24"}}, "vpn-ovpn", "/x", "127.0.0.1:1", nil)
	c.dial = func(context.Context, string) error { return nil }
	c.refresh(context.Background())
	if got := c.Snapshot().ExitIP; got != "" {
		t.Errorf("ExitIP = %q, want empty", got)
	}
}

func TestCollectorRun_StopsOnCancel(t *testing.T) {
	c := newTestCollector(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Run(ctx)
	if !c.Snapshot().ListenerChecked {
		t.Error("expected one refresh before stopping")
	}
}

func TestDialTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := dialTCP(context.Background(), addr); err != nil {
		t.Errorf("dialTCP open port: %v", err)
	}
	ln.Close()
	if err := dialTCP(context.Background(), addr); err == nil {
		t.Error("dialTCP closed port: expected error")
	}
}

func probe(name string, ok bool) health.ProbeResult {
	return health.ProbeResult{Name: name, OK: ok, LastRun: "2026-01-01T00:00:00Z"}
}

func healthyInputs() Inputs {
	return Inputs{
		OVPN: "vpn-ovpn", SOCKS: "vpn-socks", Admin: "vpn-admin", AdminListen: ":8080",
		Containers: map[string]dockerapi.ContainerInfo{
			"vpn-ovpn": {State: "running", Ports: []dockerapi.PortBinding{
				{ContainerPort: "1080", Proto: "tcp", HostIP: "127.0.0.1", HostPort: "1080"},
			}},
			"vpn-socks": {State: "running", NetworkMode: "container:abc"},
			"vpn-admin": {State: "running", Ports: []dockerapi.PortBinding{
				{ContainerPort: "8080", Proto: "tcp", HostIP: "127.0.0.1", HostPort: "8090"},
			}},
		},
		Snapshot: Snapshot{
			Remotes:         []Endpoint{{Host: "203.0.113.10", Port: "1194", Proto: "udp"}},
			TunnelAddr:      "10.8.0.6/24",
			ExitIP:          "203.0.113.7",
			ListenerChecked: true,
			ListenerOK:      true,
		},
		Health: health.Report{Probes: []health.ProbeResult{
			probe("tunnel_ping", true), probe("dns_resolve", true), probe("http_check", true),
		}},
	}
}

func TestBuild_Healthy(t *testing.T) {
	v := Build(healthyInputs())
	if len(v.Paths) != 2 {
		t.Fatalf("paths = %d", len(v.Paths))
	}
	p := v.Paths[0]
	if len(p.Links) != len(p.Nodes)-1 {
		t.Fatalf("links = %d, nodes = %d", len(p.Links), len(p.Nodes))
	}
	for _, n := range p.Nodes {
		if n.Status != Up {
			t.Errorf("node %s = %s, want up", n.ID, n.Status)
		}
	}
	for i, l := range p.Links {
		if l.Status != Up {
			t.Errorf("link %d = %s, want up", i, l.Status)
		}
	}
	lines := func(i int) string { return strings.Join(p.Nodes[i].Lines, "|") }
	if got := lines(0); got != "127.0.0.1:1080" {
		t.Errorf("host lines = %q", got)
	}
	if got := lines(1); got != "SOCKS5 :1080|netns vpn-ovpn" {
		t.Errorf("socks lines = %q", got)
	}
	if got := lines(2); got != "tun0 10.8.0.6/24" {
		t.Errorf("ovpn lines = %q", got)
	}
	if got := lines(3); got != "203.0.113.10:1194" {
		t.Errorf("server lines = %q", got)
	}
	if got := lines(4); got != "exit 203.0.113.7" {
		t.Errorf("internet lines = %q", got)
	}
	if p.Links[2].Label != "udp/1194" {
		t.Errorf("server link label = %q", p.Links[2].Label)
	}

	a := v.Paths[1]
	if a.Nodes[0].Lines[0] != "127.0.0.1:8090" || a.Nodes[1].Lines[0] != ":8080" {
		t.Errorf("admin path = %+v", a)
	}
}

func TestBuild_TunnelDown(t *testing.T) {
	in := healthyInputs()
	in.Snapshot.TunnelAddr = ""
	in.Snapshot.ExitIP = ""
	in.Health.Probes = []health.ProbeResult{
		probe("tunnel_ping", false), probe("dns_resolve", false), probe("http_check", true),
	}
	p := Build(in).Paths[0]
	want := []Status{Up, Up, Degraded, Down, Degraded}
	for i, n := range p.Nodes {
		if n.Status != want[i] {
			t.Errorf("node %s = %s, want %s", n.ID, n.Status, want[i])
		}
	}
	if p.Links[1].Status != Degraded || p.Links[2].Status != Down {
		t.Errorf("links = %+v", p.Links)
	}
	if p.Nodes[2].Lines[0] != "tun0 —" || p.Nodes[4].Lines[0] != "exit —" {
		t.Errorf("lines = %v %v", p.Nodes[2].Lines, p.Nodes[4].Lines)
	}
}

func TestBuild_DockerUnavailable(t *testing.T) {
	in := Inputs{OVPN: "vpn-ovpn", SOCKS: "vpn-socks", Admin: "vpn-admin", AdminListen: ":8080"}
	v := Build(in)
	p := v.Paths[0]
	for _, n := range p.Nodes {
		if n.Status != Unknown {
			t.Errorf("node %s = %s, want unknown", n.ID, n.Status)
		}
	}
	if p.Nodes[0].Lines[0] != "—" || p.Nodes[3].Lines[0] != "—" {
		t.Errorf("expected dashes, got %v %v", p.Nodes[0].Lines, p.Nodes[3].Lines)
	}
	if got := v.Paths[1].Nodes[1].Lines[0]; got != ":8080" {
		t.Errorf("admin fallback = %q", got)
	}
}

func TestBuild_ContainerStates(t *testing.T) {
	in := healthyInputs()
	in.Containers["vpn-socks"] = dockerapi.ContainerInfo{State: "exited"}
	in.Containers["vpn-ovpn"] = dockerapi.ContainerInfo{State: "restarting"}
	in.Snapshot.ListenerOK = false
	p := Build(in).Paths[0]
	if p.Nodes[0].Status != Down || p.Nodes[1].Status != Down || p.Nodes[2].Status != Degraded {
		t.Errorf("statuses = %s %s %s", p.Nodes[0].Status, p.Nodes[1].Status, p.Nodes[2].Status)
	}
}

func TestHostListeners(t *testing.T) {
	l, tg := hostListeners([]dockerapi.PortBinding{
		{ContainerPort: "53", Proto: "udp", HostPort: "5353"},
		{ContainerPort: "53", Proto: "udp", HostIP: "::", HostPort: "5353"},
	})
	if strings.Join(l, ",") != "0.0.0.0:5353,[::]:5353" || strings.Join(tg, ",") != ":53/udp" {
		t.Errorf("listeners=%v targets=%v", l, tg)
	}
}

func TestCombineAndWorst(t *testing.T) {
	cases := []struct {
		in   []Status
		want Status
	}{
		{nil, Unknown},
		{[]Status{Unknown, Up}, Up},
		{[]Status{Down, Unknown}, Down},
		{[]Status{Up, Down}, Degraded},
		{[]Status{Degraded, Up}, Degraded},
	}
	for _, c := range cases {
		if got := combine(c.in...); got != c.want {
			t.Errorf("combine(%v) = %s, want %s", c.in, got, c.want)
		}
	}
	if worst(Up, Unknown) != Unknown || worst(Down, Degraded) != Down || worst(Up, Up) != Up {
		t.Error("worst ordering broken")
	}
}

func TestFilterRemoteDirectives(t *testing.T) {
	cfg := "client\n  remote 203.0.113.10 1194 udp\nPROTO tcp\nport 443\n<key>\nFAKE-KEY-MATERIAL\nremotex\n</key>\nauth-user-pass\n"
	got := FilterRemoteDirectives(cfg)
	want := "remote 203.0.113.10 1194 udp\nPROTO tcp\nport 443\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCollectorRefresh_ReadsConfigWithoutShell(t *testing.T) {
	var cmds [][]string
	exec := execFunc(func(cmd []string) (string, error) {
		cmds = append(cmds, cmd)
		if cmd[0] == "cat" {
			return "remote 203.0.113.10 1194\n<key>\nSECRET\n</key>\n", nil
		}
		return "", nil
	})
	c := newTestCollector(exec)
	c.refresh(context.Background())
	if len(cmds) == 0 || len(cmds[0]) != 2 || cmds[0][0] != "cat" || cmds[0][1] != "/vpn/config.ovpn" {
		t.Fatalf("cmds = %q", cmds)
	}
	for _, cmd := range cmds {
		if cmd[0] == "sh" {
			t.Fatalf("shell used: %q", cmd)
		}
	}
	if s := c.Snapshot(); len(s.Remotes) != 1 || s.Remotes[0].Host != "203.0.113.10" {
		t.Errorf("remotes = %+v", s.Remotes)
	}
}

type execFunc func(cmd []string) (string, error)

func (f execFunc) Exec(_ context.Context, _ string, cmd []string) (string, error) { return f(cmd) }
