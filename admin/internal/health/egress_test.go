package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// findProbe returns the probe called name from a report.
func findProbe(r Report, name string) (ProbeResult, bool) {
	for _, pr := range r.Probes {
		if pr.Name == name {
			return pr, true
		}
	}
	return ProbeResult{}, false
}

// exitIPServer answers every request with status and body.
func exitIPServer(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func ips(t *testing.T, s ...string) []net.IP {
	t.Helper()
	out := make([]net.IP, 0, len(s))
	for _, v := range s {
		ip := net.ParseIP(v)
		if ip == nil {
			t.Fatalf("bad test IP %q", v)
		}
		out = append(out, ip)
	}
	return out
}

func egressOnly(t *testing.T, url string, expected []net.IP) *Prober {
	t.Helper()
	return NewProber(startSOCKS5Server(t), nil, "", "", "", nil, url, expected)
}

func TestProbeEgress_Match(t *testing.T) {
	p := egressOnly(t, exitIPServer(t, http.StatusOK, "203.0.113.7\n"), ips(t, "203.0.113.7"))
	p.probeEgress(context.Background())

	r := p.GetReport()
	pr, ok := findProbe(r, ProbeEgress)
	if !ok || !pr.OK || pr.Leak || pr.Error != "" {
		t.Fatalf("egress = %+v", pr)
	}
	if pr.Detail != "exit ip 203.0.113.7" {
		t.Errorf("detail = %q", pr.Detail)
	}
	if r.Overall != StatusHealthy {
		t.Errorf("overall = %q", r.Overall)
	}
	if got := p.ExitIP(); got != "203.0.113.7" {
		t.Errorf("ExitIP = %q", got)
	}
}

func TestProbeEgress_NoExpected(t *testing.T) {
	p := egressOnly(t, exitIPServer(t, http.StatusOK, " 198.51.100.42 "), nil)
	p.probeEgress(context.Background())

	pr, _ := findProbe(p.GetReport(), ProbeEgress)
	if !pr.OK || pr.Detail != "exit ip 198.51.100.42" {
		t.Errorf("egress = %+v", pr)
	}
}

// A leak is critical: three healthy probes plus an exit IP mismatch is down.
func TestProbeEgress_LeakForcesDown(t *testing.T) {
	socks := startSOCKS5Server(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())

	p := NewProber(socks, nil, host, port, target.URL, []string{target.URL},
		exitIPServer(t, http.StatusOK, "198.51.100.99"), ips(t, "203.0.113.7"))
	p.runOnce(context.Background())

	r := p.GetReport()
	for _, name := range []string{ProbeTunnel, ProbeDNS, ProbeHTTP} {
		if pr, ok := findProbe(r, name); !ok || !pr.OK {
			t.Fatalf("%s = %+v, want ok", name, pr)
		}
	}
	pr, _ := findProbe(r, ProbeEgress)
	if pr.OK || !pr.Leak || !strings.Contains(pr.Error, "LEAK") {
		t.Errorf("egress = %+v, want leak", pr)
	}
	if pr.Detail != "exit ip 198.51.100.99" {
		t.Errorf("detail = %q", pr.Detail)
	}
	if r.Overall != StatusDown {
		t.Errorf("overall = %q, want %q", r.Overall, StatusDown)
	}
	// The observed (leaking) IP is still what the topology view shows.
	if got := p.ExitIP(); got != "198.51.100.99" {
		t.Errorf("ExitIP = %q", got)
	}
}

func TestProbeEgress_ExpectedList(t *testing.T) {
	p := egressOnly(t, exitIPServer(t, http.StatusOK, "203.0.113.8"), ips(t, "203.0.113.7", "203.0.113.8"))
	p.probeEgress(context.Background())
	if pr, _ := findProbe(p.GetReport(), ProbeEgress); !pr.OK {
		t.Errorf("egress = %+v, want ok for second expected IP", pr)
	}
}

func TestProbeEgress_IPNormalization(t *testing.T) {
	cases := []struct{ body, expected, detail string }{
		{"2001:0db8:0000:0000:0000:0000:0000:0001", "2001:db8::1", "exit ip 2001:db8::1"},
		{"2001:DB8::1", "2001:db8::1", "exit ip 2001:db8::1"},
		{"::ffff:203.0.113.7", "203.0.113.7", "exit ip 203.0.113.7"},
		{"203.0.113.7", "::ffff:203.0.113.7", "exit ip 203.0.113.7"},
	}
	for _, c := range cases {
		p := egressOnly(t, exitIPServer(t, http.StatusOK, c.body), ips(t, c.expected))
		p.probeEgress(context.Background())
		pr, _ := findProbe(p.GetReport(), ProbeEgress)
		if !pr.OK || pr.Detail != c.detail {
			t.Errorf("body %q expected %q: egress = %+v", c.body, c.expected, pr)
		}
	}
}

func TestProbeEgress_UnexpectedResponses(t *testing.T) {
	const page = "<html>captive portal</html>"
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"non-IP body", http.StatusOK, page},
		{"empty body", http.StatusOK, ""},
		{"status 500 with IP", http.StatusInternalServerError, "203.0.113.7"},
		{"status 403 with HTML", http.StatusForbidden, page},
		{"oversized body", http.StatusOK, strings.Repeat("9", 4096)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := egressOnly(t, exitIPServer(t, c.status, c.body), nil)
			p.probeEgress(context.Background())
			r := p.GetReport()
			pr, _ := findProbe(r, ProbeEgress)
			if pr.OK || pr.Leak {
				t.Fatalf("egress = %+v, want failure without leak", pr)
			}
			if pr.Error != errExitIPResponse.Error() {
				t.Errorf("error = %q, want generic text", pr.Error)
			}
			if pr.Detail != "" || strings.Contains(pr.Error, "html") {
				t.Errorf("response leaked into result: %+v", pr)
			}
			if r.Overall != StatusDown {
				t.Errorf("overall = %q", r.Overall)
			}
			if p.ExitIP() != "" {
				t.Errorf("ExitIP = %q, want empty", p.ExitIP())
			}
		})
	}
}

func TestProbeEgress_TransportErrors(t *testing.T) {
	p := NewProber("127.0.0.1:1", nil, "", "", "", nil, "http://203.0.113.1/ip", nil)
	p.probeEgress(context.Background())
	if pr, _ := findProbe(p.GetReport(), ProbeEgress); pr.OK || pr.Error == "" {
		t.Errorf("unreachable proxy: egress = %+v", pr)
	}

	p = egressOnly(t, "://bad", nil)
	p.probeEgress(context.Background())
	if pr, _ := findProbe(p.GetReport(), ProbeEgress); pr.OK || pr.Error == "" {
		t.Errorf("bad URL: egress = %+v", pr)
	}
}

func TestProbeEgress_FailureClearsExitIP(t *testing.T) {
	body := "203.0.113.7"
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	p := egressOnly(t, srv.URL, nil)
	p.probeEgress(context.Background())
	if p.ExitIP() != "203.0.113.7" {
		t.Fatalf("ExitIP = %q", p.ExitIP())
	}
	mu.Lock()
	body = "oops"
	mu.Unlock()
	p.probeEgress(context.Background())
	if p.ExitIP() != "" {
		t.Errorf("ExitIP = %q, want cleared after a failed probe", p.ExitIP())
	}
}

func TestProbeEgress_DisabledWithoutURL(t *testing.T) {
	p := NewProber("127.0.0.1:1", nil, "127.0.0.1", "1", "http://127.0.0.1:1", nil, "", nil)
	p.runOnce(context.Background())
	if _, ok := findProbe(p.GetReport(), ProbeEgress); ok {
		t.Error("egress_ip must not run without EXIT_IP_URL")
	}
}

// Run with all probes, including egress_ip, while readers hammer the report;
// meant for -race.
func TestRun_EgressConcurrent(t *testing.T) {
	socks := startSOCKS5Server(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("203.0.113.7"))
	}))
	defer target.Close()
	p := NewProber(socks, nil, "127.0.0.1", "1", target.URL, []string{target.URL}, target.URL, ips(t, "203.0.113.7"))
	p.TunnelInterval = 5 * time.Millisecond
	p.DNSInterval = 5 * time.Millisecond
	p.HTTPInterval = 5 * time.Millisecond
	p.EgressInterval = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	p.Run(ctx)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = p.GetReport()
				_ = p.ExitIP()
			}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if pr, ok := findProbe(p.GetReport(), ProbeEgress); ok && pr.OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("egress probe never succeeded: %+v", p.GetReport())
		}
		time.Sleep(5 * time.Millisecond)
	}
	wg.Wait()
	cancel()
	p.Wait()
	if got := p.ExitIP(); got != "203.0.113.7" {
		t.Errorf("ExitIP = %q", got)
	}
}
