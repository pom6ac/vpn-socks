package health

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/net/proxy"

	"github.com/pom6ac/vpn-socks/admin/internal/socks5test"
)

func startSOCKS5Server(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSOCKS5(conn)
		}
	}()

	return ln.Addr().String()
}

func handleSOCKS5(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 512)

	if _, err := conn.Read(buf); err != nil {
		return
	}
	_, _ = conn.Write([]byte{0x05, 0x00})

	n, err := conn.Read(buf)
	if err != nil || n < 7 {
		return
	}
	if buf[0] != 0x05 || buf[1] != 0x01 {
		return
	}

	addrType := buf[3]
	var target string
	switch addrType {
	case 0x01:
		if n < 10 {
			return
		}
		ip := net.IP(buf[4:8])
		port := binary.BigEndian.Uint16(buf[8:10])
		target = fmt.Sprintf("%s:%d", ip, port)
	case 0x03:
		if n < 8 {
			return
		}
		alen := int(buf[4])
		if n < 5+alen+2 {
			return
		}
		domain := string(buf[5 : 5+alen])
		port := binary.BigEndian.Uint16(buf[5+alen : 7+alen])
		target = fmt.Sprintf("%s:%d", domain, port)
	default:
		_, _ = conn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	targetConn, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer targetConn.Close()

	_, _ = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(targetConn, conn)
		close(done)
	}()
	_, _ = io.Copy(conn, targetConn)
	<-done
}

func TestNewProber(t *testing.T) {
	p := NewProber("127.0.0.1:1080", nil, "10.0.0.1", "443", "http://example.com", []string{"https://ifconfig.me"}, "", nil)
	if p.dialErr != nil || p.dialer == nil {
		t.Errorf("dialer = %v, err = %v", p.dialer, p.dialErr)
	}
	if p.TunnelInterval != DefaultTunnelInterval || p.DNSInterval != DefaultDNSInterval || p.HTTPInterval != DefaultHTTPInterval {
		t.Error("unexpected default intervals")
	}
	if p.tunnelTarget != "10.0.0.1" {
		t.Errorf("tunnelTarget = %q", p.tunnelTarget)
	}
	if p.tunnelPort != "443" {
		t.Errorf("tunnelPort = %q", p.tunnelPort)
	}
	if p.dnsCheckURL != "http://example.com" {
		t.Errorf("dnsCheckURL = %q", p.dnsCheckURL)
	}
	if len(p.httpCheckURLs) != 1 {
		t.Errorf("httpCheckURLs len = %d", len(p.httpCheckURLs))
	}
}

func TestGetReport_Initial(t *testing.T) {
	p := NewProber("", nil, "", "", "", nil, "", nil)
	r := p.GetReport()
	if r.Overall != StatusDown {
		t.Errorf("initial overall = %q, want %q", r.Overall, StatusDown)
	}
	if len(r.Probes) != 0 {
		t.Errorf("initial probes len = %d, want 0", len(r.Probes))
	}
}

func TestAggregate_NoProbes(t *testing.T) {
	p := NewProber("", nil, "", "", "", nil, "", nil)
	p.aggregateLocked()
	r := p.GetReport()
	if r.Overall != StatusDown {
		t.Errorf("overall = %q, want %q", r.Overall, StatusDown)
	}
}

func TestAggregate_AllHealthy(t *testing.T) {
	p := NewProber("", nil, "", "", "", nil, "", nil)
	now := time.Now().UTC().Format(time.RFC3339)
	p.tunnelLast = ProbeResult{Name: "tunnel_ping", OK: true, LastRun: now}
	p.dnsLast = ProbeResult{Name: "dns_resolve", OK: true, LastRun: now}
	p.httpLast = ProbeResult{Name: "http_check", OK: true, LastRun: now}
	p.aggregateLocked()
	r := p.GetReport()
	if r.Overall != StatusHealthy {
		t.Errorf("overall = %q, want %q", r.Overall, StatusHealthy)
	}
}

func TestAggregate_AllFailed(t *testing.T) {
	p := NewProber("", nil, "", "", "", nil, "", nil)
	now := time.Now().UTC().Format(time.RFC3339)
	p.tunnelLast = ProbeResult{Name: "tunnel_ping", OK: false, LastRun: now}
	p.dnsLast = ProbeResult{Name: "dns_resolve", OK: false, LastRun: now}
	p.httpLast = ProbeResult{Name: "http_check", OK: false, LastRun: now}
	p.aggregateLocked()
	r := p.GetReport()
	if r.Overall != StatusDown {
		t.Errorf("overall = %q, want %q", r.Overall, StatusDown)
	}
}

func TestAggregate_Degraded_OneFailed(t *testing.T) {
	p := NewProber("", nil, "", "", "", nil, "", nil)
	now := time.Now().UTC().Format(time.RFC3339)
	p.tunnelLast = ProbeResult{Name: "tunnel_ping", OK: true, LastRun: now}
	p.dnsLast = ProbeResult{Name: "dns_resolve", OK: false, LastRun: now}
	p.httpLast = ProbeResult{Name: "http_check", OK: true, LastRun: now}
	p.aggregateLocked()
	r := p.GetReport()
	if r.Overall != StatusDegraded {
		t.Errorf("overall = %q, want %q", r.Overall, StatusDegraded)
	}
}

func TestAggregate_Degraded_TwoFailed(t *testing.T) {
	p := NewProber("", nil, "", "", "", nil, "", nil)
	now := time.Now().UTC().Format(time.RFC3339)
	p.tunnelLast = ProbeResult{Name: "tunnel_ping", OK: false, LastRun: now}
	p.dnsLast = ProbeResult{Name: "dns_resolve", OK: false, LastRun: now}
	p.httpLast = ProbeResult{Name: "http_check", OK: true, LastRun: now}
	p.aggregateLocked()
	r := p.GetReport()
	if r.Overall != StatusDegraded {
		t.Errorf("overall = %q, want %q", r.Overall, StatusDegraded)
	}
}

func TestAggregate_PartialProbes(t *testing.T) {
	p := NewProber("", nil, "", "", "", nil, "", nil)
	now := time.Now().UTC().Format(time.RFC3339)
	p.tunnelLast = ProbeResult{Name: "tunnel_ping", OK: true, LastRun: now}
	p.aggregateLocked()
	r := p.GetReport()
	if r.Overall != StatusHealthy {
		t.Errorf("overall = %q, want %q", r.Overall, StatusHealthy)
	}
	// Probes that have not run yet are left out of the report.
	if len(r.Probes) != 1 || r.Probes[0].Name != ProbeTunnel {
		t.Errorf("probes = %+v, want only %s", r.Probes, ProbeTunnel)
	}
}

func TestRunOnce_BadProxy(t *testing.T) {
	p := NewProber("127.0.0.1:1", nil, "127.0.0.1", "1", "http://127.0.0.1:1", []string{"http://127.0.0.1:1"}, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.runOnce(ctx)

	r := p.GetReport()
	if r.Overall != StatusDown {
		t.Errorf("overall = %q, want %q", r.Overall, StatusDown)
	}

	for _, probe := range r.Probes {
		if probe.LastRun == "" {
			t.Errorf("probe %s has empty LastRun", probe.Name)
		}
		if probe.OK {
			t.Errorf("probe %s should not be OK with bad proxy", probe.Name)
		}
	}
}

func TestRun_CancelledContext(t *testing.T) {
	p := NewProber("127.0.0.1:1", nil, "127.0.0.1", "1", "http://127.0.0.1:1", []string{"http://127.0.0.1:1"}, "", nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p.Run(ctx)

	r := p.GetReport()
	if r.Overall != StatusDown {
		t.Errorf("overall = %q, want %q", r.Overall, StatusDown)
	}
}

func TestProbeTunnel_Success(t *testing.T) {
	socks5Addr := startSOCKS5Server(t)
	targetListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer targetListener.Close()

	targetAddr := targetListener.Addr().String()
	host, port, _ := net.SplitHostPort(targetAddr)

	p := NewProber(socks5Addr, nil, host, port, "", nil, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.probeTunnel(ctx)

	if !p.tunnelLast.OK {
		t.Errorf("tunnel probe should succeed, error: %s", p.tunnelLast.Error)
	}
	if p.tunnelLast.Latency <= 0 {
		t.Error("expected positive latency")
	}
}

func TestProbeDNS_Success(t *testing.T) {
	socks5Addr := startSOCKS5Server(t)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer httpServer.Close()

	p := NewProber(socks5Addr, nil, "", "", httpServer.URL, nil, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.probeDNS(ctx)

	if !p.dnsLast.OK {
		t.Errorf("dns probe should succeed, error: %s", p.dnsLast.Error)
	}
}

func TestProbeDNS_Non200(t *testing.T) {
	socks5Addr := startSOCKS5Server(t)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer httpServer.Close()

	p := NewProber(socks5Addr, nil, "", "", httpServer.URL, nil, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.probeDNS(ctx)

	if p.dnsLast.OK {
		t.Error("dns probe should fail with 403")
	}
	if p.dnsLast.Error == "" {
		t.Error("expected error message for 403")
	}
}

func TestProbeHTTP_Success(t *testing.T) {
	socks5Addr := startSOCKS5Server(t)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer httpServer.Close()

	p := NewProber(socks5Addr, nil, "", "", "", []string{httpServer.URL}, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.probeHTTP(ctx)

	if !p.httpLast.OK {
		t.Errorf("http probe should succeed, error: %s", p.httpLast.Error)
	}
}

func TestProbeHTTP_PartialSuccess(t *testing.T) {
	socks5Addr := startSOCKS5Server(t)
	okServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer okServer.Close()
	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer badServer.Close()

	p := NewProber(socks5Addr, nil, "", "", "", []string{okServer.URL, badServer.URL}, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.probeHTTP(ctx)

	if !p.httpLast.OK {
		t.Error("http probe should be OK with 1/2 success")
	}
	if p.httpLast.Error == "" {
		t.Error("expected partial error message")
	}
}

func TestProbeHTTP_AllFail(t *testing.T) {
	socks5Addr := startSOCKS5Server(t)
	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer badServer.Close()

	p := NewProber(socks5Addr, nil, "", "", "", []string{badServer.URL}, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.probeHTTP(ctx)

	if p.httpLast.OK {
		t.Error("http probe should fail when all URLs return 500")
	}
}

func TestRunOnce_AllSuccess(t *testing.T) {
	socks5Addr := startSOCKS5Server(t)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer httpServer.Close()

	targetListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer targetListener.Close()
	host, port, _ := net.SplitHostPort(targetListener.Addr().String())

	p := NewProber(socks5Addr, nil, host, port, httpServer.URL, []string{httpServer.URL}, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.runOnce(ctx)

	r := p.GetReport()
	if r.Overall != StatusHealthy {
		t.Errorf("overall = %q, want %q", r.Overall, StatusHealthy)
	}
	for _, probe := range r.Probes {
		if !probe.OK {
			t.Errorf("probe %s should be OK, error: %s", probe.Name, probe.Error)
		}
	}
}

func TestRunOnce_Degraded(t *testing.T) {
	socks5Addr := startSOCKS5Server(t)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer httpServer.Close()

	p := NewProber(socks5Addr, nil, "127.0.0.1", "1", httpServer.URL, []string{httpServer.URL}, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.runOnce(ctx)

	r := p.GetReport()
	if r.Overall != StatusDegraded {
		t.Errorf("overall = %q, want %q (tunnel should fail, dns+http succeed)", r.Overall, StatusDegraded)
	}
}

// TestProber_SOCKS5Auth runs all probes against a proxy that requires
// RFC 1929 username/password authentication.
func TestProber_SOCKS5Auth(t *testing.T) {
	srv := socks5test.Start(t, "alice", "s3cret")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	host, port, _ := net.SplitHostPort(target.Listener.Addr().String())

	ok := NewProber(srv.Addr, &proxy.Auth{User: "alice", Password: "s3cret"}, host, port, target.URL, []string{target.URL}, "", nil)
	ok.runOnce(context.Background())
	if r := ok.GetReport(); r.Overall != StatusHealthy {
		t.Fatalf("with credentials: overall = %q, probes = %+v", r.Overall, r.Probes)
	}

	noAuth := NewProber(srv.Addr, nil, host, port, target.URL, []string{target.URL}, "", nil)
	noAuth.runOnce(context.Background())
	if r := noAuth.GetReport(); r.Overall != StatusDown {
		t.Fatalf("without credentials: overall = %q, want down", r.Overall)
	}
}
