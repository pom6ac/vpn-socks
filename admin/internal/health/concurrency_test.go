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

// TestProber_ConcurrentRunAndRead runs all probes on very short intervals
// while readers poll the report; run with -race to catch unsynchronised
// access to the per-probe results.
func TestProber_ConcurrentRunAndRead(t *testing.T) {
	socks := startSOCKS5Server(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	host, port, _ := net.SplitHostPort(target.Listener.Addr().String())

	p := NewProber(socks, nil, host, port, target.URL, []string{target.URL, target.URL + "/x"}, "", nil)
	p.TunnelInterval = 2 * time.Millisecond
	p.DNSInterval = 3 * time.Millisecond
	p.HTTPInterval = 4 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Run(ctx)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				r := p.GetReport()
				for _, pr := range r.Probes {
					_ = pr.Name + pr.LastRun
				}
				time.Sleep(200 * time.Microsecond)
			}
		}()
	}
	wg.Wait()

	// Check a healthy snapshot while the probes are still running: after
	// cancel an in-flight probe may legitimately record "context canceled".
	var r Report
	deadline := time.Now().Add(5 * time.Second)
	for {
		r = p.GetReport()
		if r.Overall == StatusHealthy || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if r.Overall != StatusHealthy {
		t.Fatalf("overall = %q, probes = %+v", r.Overall, r.Probes)
	}
	if len(r.Probes) != 3 {
		t.Fatalf("probes = %d", len(r.Probes))
	}

	// Cancellation must stop every probe goroutine (join, not a sleep).
	cancel()
	joined := make(chan struct{})
	go func() { p.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("probe goroutines did not exit after cancel")
	}
}

// TestRun_DoesNotBlock checks that Run returns before the first (slow)
// probe round completes, so the HTTP server can start immediately.
func TestRun_DoesNotBlock(t *testing.T) {
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
			// Hold the connection open without speaking SOCKS.
			go func() { time.Sleep(2 * time.Second); _ = c.Close() }()
		}
	}()

	p := NewProber(ln.Addr().String(), nil, "127.0.0.1", "1", "http://127.0.0.1:1", nil, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	p.Run(ctx)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("Run blocked for %v", d)
	}
}

func TestGetReport_ReturnsCopy(t *testing.T) {
	p := NewProber("", nil, "", "", "", nil, "", nil)
	p.setResult(ProbeResult{Name: ProbeTunnel, OK: true, LastRun: "x"})
	r := p.GetReport()
	r.Probes[0].Name = "mutated"
	if p.GetReport().Probes[0].Name != ProbeTunnel {
		t.Fatal("GetReport must return a copy of the probes slice")
	}
}

func TestProbes_NoProxyConfigured(t *testing.T) {
	p := NewProber("", nil, "127.0.0.1", "1", "http://127.0.0.1:1", nil, "", nil)
	p.runOnce(context.Background())
	r := p.GetReport()
	if r.Overall != StatusDown {
		t.Errorf("overall = %q", r.Overall)
	}
	for _, pr := range r.Probes {
		if pr.OK || pr.Error == "" {
			t.Errorf("probe %+v should fail with an error", pr)
		}
	}
	if !strings.Contains(r.Probes[0].Error, "not configured") {
		t.Errorf("tunnel error = %q", r.Probes[0].Error)
	}
	if r.Probes[2].Error != "no check URLs configured" {
		t.Errorf("http error = %q", r.Probes[2].Error)
	}
}

func TestProbeDNS_BadURL(t *testing.T) {
	p := NewProber("127.0.0.1:1", nil, "", "", "://bad", nil, "", nil)
	p.probeDNS(context.Background())
	if pr, ok := findProbe(p.GetReport(), ProbeDNS); !ok || pr.OK || pr.Error == "" {
		t.Errorf("dns probe = %+v", pr)
	}
}

func TestSetResult_UnknownNameIgnored(t *testing.T) {
	p := NewProber("", nil, "", "", "", nil, "", nil)
	p.setResult(ProbeResult{Name: "other", OK: true, LastRun: "x"})
	if r := p.GetReport(); r.Overall != StatusDown {
		t.Errorf("overall = %q", r.Overall)
	}
}
