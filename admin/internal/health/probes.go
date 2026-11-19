package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"github.com/pom6ac/vpn-socks/admin/internal/config"
)

// Default probe schedule.
const (
	DefaultTunnelInterval = 5 * time.Second
	DefaultDNSInterval    = 10 * time.Second
	DefaultHTTPInterval   = 30 * time.Second
	// DefaultEgressInterval keeps the external "what is my IP" lookup at about
	// one request per minute to stay polite to the lookup service.
	DefaultEgressInterval = 60 * time.Second

	tunnelTimeout = 5 * time.Second
	httpTimeout   = 10 * time.Second

	// exitIPBodyLimit caps how much of the exit IP service response is read;
	// a textual IPv6 address is at most 45 bytes.
	exitIPBodyLimit = 256
	// exitIPLogLimit caps how much of an unexpected response body is logged.
	exitIPLogLimit = 64
)

// Probe names as reported in ProbeResult.Name.
const (
	ProbeTunnel = "tunnel_ping"
	ProbeDNS    = "dns_resolve"
	ProbeHTTP   = "http_check"
	ProbeEgress = "egress_ip"
)

// errExitIPResponse is reported (instead of the raw response) when the exit IP
// service answers with something other than 200 and a valid IP address.
var errExitIPResponse = errors.New("unexpected response from exit IP service")

// ProbeResult holds the outcome of a single health probe.
type ProbeResult struct {
	Name    string  `json:"name"`
	OK      bool    `json:"ok"`
	Latency float64 `json:"latency_ms"`
	Error   string  `json:"error,omitempty"`
	// Detail is extra, non-error information (egress_ip: the observed exit IP).
	Detail string `json:"detail,omitempty"`
	// Leak marks a critical failure: traffic left through an unexpected exit
	// IP. Any leaking probe forces the overall status to down.
	Leak    bool   `json:"leak,omitempty"`
	LastRun string `json:"last_run"`
}

// Status represents the aggregated health status.
type Status string

// Aggregated health states.
const (
	StatusHealthy  Status = "healthy"
	StatusDegraded Status = "degraded"
	StatusDown     Status = "down"
)

// Report aggregates all probe results.
type Report struct {
	Overall Status        `json:"overall"`
	Probes  []ProbeResult `json:"probes"`
}

// Prober runs health probes through a SOCKS5 proxy on a schedule.
type Prober struct {
	tunnelTarget  string
	tunnelPort    string
	dnsCheckURL   string
	httpCheckURLs []string
	exitIPURL     string
	expectedExit  []net.IP

	// Probe intervals; may be changed before Run (tests use short ones).
	TunnelInterval time.Duration
	DNSInterval    time.Duration
	HTTPInterval   time.Duration
	EgressInterval time.Duration

	dialer  proxy.ContextDialer
	dialErr error
	client  *http.Client

	wg sync.WaitGroup

	mu         sync.RWMutex
	report     Report
	tunnelLast ProbeResult
	dnsLast    ProbeResult
	httpLast   ProbeResult
	egressLast ProbeResult
	// exitIP is the last exit IP observed by the egress_ip probe ("" when
	// the last attempt failed).
	exitIP string
}

// NewProber creates a Prober from the given config values. All probes share
// one SOCKS5 dialer and one HTTP transport. auth may be nil (no SOCKS5
// authentication). exitIPURL is a plain-text "what is my IP" service; empty
// disables the egress_ip probe. expectedExitIPs lists the acceptable VPN exit
// addresses; empty means the observed IP is reported but not compared.
func NewProber(socks5Addr string, auth *proxy.Auth, tunnelTarget, tunnelPort, dnsCheckURL string, httpCheckURLs []string, exitIPURL string, expectedExitIPs []net.IP) *Prober {
	p := &Prober{
		tunnelTarget:   tunnelTarget,
		tunnelPort:     tunnelPort,
		dnsCheckURL:    dnsCheckURL,
		httpCheckURLs:  httpCheckURLs,
		exitIPURL:      exitIPURL,
		expectedExit:   append([]net.IP(nil), expectedExitIPs...),
		TunnelInterval: DefaultTunnelInterval,
		DNSInterval:    DefaultDNSInterval,
		HTTPInterval:   DefaultHTTPInterval,
		EgressInterval: DefaultEgressInterval,
		report:         Report{Overall: StatusDown},
	}
	p.dialer, p.dialErr = config.SOCKS5Dialer(socks5Addr, auth)
	tr := &http.Transport{
		DialContext:           p.dialContext,
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: httpTimeout,
		TLSHandshakeTimeout:   httpTimeout,
	}
	p.client = &http.Client{Transport: tr, Timeout: httpTimeout}
	return p
}

func (p *Prober) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if p.dialErr != nil {
		return nil, p.dialErr
	}
	return p.dialer.DialContext(ctx, network, addr)
}

// Run starts the probes in background goroutines and returns immediately.
// The first round runs right away; the goroutines exit when ctx is cancelled.
// Wait blocks until those goroutines have exited.
func (p *Prober) Run(ctx context.Context) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.runOnce(ctx)
		p.wg.Add(3)
		go p.loop(ctx, p.TunnelInterval, p.probeTunnel)
		go p.loop(ctx, p.DNSInterval, p.probeDNS)
		go p.loop(ctx, p.HTTPInterval, p.probeHTTP)
		if p.exitIPURL != "" {
			p.wg.Add(1)
			go p.loop(ctx, p.EgressInterval, p.probeEgress)
		}
	}()
}

// Wait blocks until every goroutine started by Run has returned (after its
// context is cancelled).
func (p *Prober) Wait() { p.wg.Wait() }

func (p *Prober) loop(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	defer p.wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn(ctx)
		}
	}
}

func (p *Prober) runOnce(ctx context.Context) {
	p.probeTunnel(ctx)
	p.probeDNS(ctx)
	p.probeHTTP(ctx)
	if p.exitIPURL != "" {
		p.probeEgress(ctx)
	}
}

// GetReport returns a snapshot of the current health report.
func (p *Prober) GetReport() Report {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r := p.report
	r.Probes = append([]ProbeResult(nil), p.report.Probes...)
	return r
}

// ExitIP returns the exit IP observed by the last egress_ip probe, or "" if
// the probe is disabled, has not run yet or its last attempt failed. It is the
// single source of the exit IP for the rest of the panel (topology), so the
// exit IP service is queried once per EgressInterval.
func (p *Prober) ExitIP() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.exitIP
}

func newResult(name string) ProbeResult {
	return ProbeResult{Name: name, LastRun: time.Now().UTC().Format(time.RFC3339Nano)}
}

// probeTunnel does a TCP connect through SOCKS5 (which rides the tunnel's
// network namespace). The target must be an always-reachable endpoint, not
// the VPN server itself: the VPN endpoint is unreachable from inside the
// tunnel even when the tunnel is up.
func (p *Prober) probeTunnel(ctx context.Context) {
	result := newResult(ProbeTunnel)
	dialCtx, cancel := context.WithTimeout(ctx, tunnelTimeout)
	defer cancel()
	start := time.Now()
	conn, err := p.dialContext(dialCtx, "tcp", net.JoinHostPort(p.tunnelTarget, p.tunnelPort))
	if err != nil {
		result.Error = err.Error()
		log.Printf("probe %s failed: %v", ProbeTunnel, err)
	} else {
		_ = conn.Close()
		result.OK = true
		result.Latency = msSince(start)
	}
	p.setResult(result)
}

// get performs a GET through the tunnel and reports whether it returned 200.
func (p *Prober) get(ctx context.Context, u string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// probeDNS does an HTTP GET through SOCKS5 to verify DNS resolution via tunnel.
func (p *Prober) probeDNS(ctx context.Context) {
	result := newResult(ProbeDNS)
	start := time.Now()
	if err := p.get(ctx, p.dnsCheckURL); err != nil {
		result.Error = err.Error()
		log.Printf("probe %s failed: %v", ProbeDNS, err)
	} else {
		result.OK = true
		result.Latency = msSince(start)
	}
	p.setResult(result)
}

// probeHTTP does HTTP GET to reference URLs through SOCKS5.
func (p *Prober) probeHTTP(ctx context.Context) {
	result := newResult(ProbeHTTP)
	var errs []string
	okCount := 0
	start := time.Now()
	for _, u := range p.httpCheckURLs {
		if err := p.get(ctx, u); err != nil {
			errs = append(errs, u+": "+err.Error())
			continue
		}
		okCount++
	}
	if okCount > 0 {
		result.OK = true
		result.Latency = msSince(start)
		if okCount < len(p.httpCheckURLs) {
			result.Error = fmt.Sprintf("%d/%d ok", okCount, len(p.httpCheckURLs))
		}
	} else {
		result.Error = strings.Join(errs, "; ")
		if result.Error == "" {
			result.Error = "no check URLs configured"
		}
		log.Printf("probe %s failed: %s", ProbeHTTP, result.Error)
	}
	p.setResult(result)
}

// probeEgress fetches the public exit IP through SOCKS5 and compares it with
// the expected VPN exit IPs. The other probes ride the same SOCKS5 path and
// stay green when traffic leaves through the container's own uplink instead of
// the tunnel; only the exit IP tells the two apart.
func (p *Prober) probeEgress(ctx context.Context) {
	result := newResult(ProbeEgress)
	start := time.Now()
	ip, err := p.fetchExitIP(ctx)
	if err != nil {
		result.Error = err.Error()
		log.Printf("probe %s failed: %v", ProbeEgress, err)
		p.setEgress(result, "")
		return
	}
	result.Latency = msSince(start)
	result.Detail = "exit ip " + ip.String()
	if len(p.expectedExit) > 0 && !containsIP(p.expectedExit, ip) {
		result.Leak = true
		result.Error = "LEAK: exit IP is not in EXPECTED_EXIT_IP"
		log.Printf("probe %s: %s", ProbeEgress, result.Error)
	} else {
		result.OK = true
	}
	p.setEgress(result, ip.String())
}

// fetchExitIP asks exitIPURL for the public address seen through the tunnel.
// The response body never ends up in the returned error (it is shown in the
// UI); a truncated, quoted copy goes to the log only.
func (p *Prober) fetchExitIP(ctx context.Context) (net.IP, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.exitIPURL, nil)
	if err != nil {
		return nil, err
	}
	// Plain-text IP services (ifconfig.me) answer curl-like clients with the
	// bare address instead of an HTML page.
	req.Header.Set("User-Agent", "curl/8")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, exitIPBodyLimit))
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(strings.TrimSpace(string(body)))
	if resp.StatusCode != http.StatusOK || ip == nil {
		if len(body) > exitIPLogLimit {
			body = body[:exitIPLogLimit]
		}
		log.Printf("probe %s: status %d, body %q", ProbeEgress, resp.StatusCode, body)
		return nil, errExitIPResponse
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip, nil
}

func containsIP(list []net.IP, ip net.IP) bool {
	for _, e := range list {
		if e.Equal(ip) {
			return true
		}
	}
	return false
}

func msSince(t time.Time) float64 {
	return time.Since(t).Seconds() * 1000
}

// setResult stores the latest result of one probe and recomputes the report.
func (p *Prober) setResult(r ProbeResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch r.Name {
	case ProbeTunnel:
		p.tunnelLast = r
	case ProbeDNS:
		p.dnsLast = r
	case ProbeHTTP:
		p.httpLast = r
	}
	p.aggregateLocked()
}

// setEgress stores the egress_ip result together with the observed exit IP.
func (p *Prober) setEgress(r ProbeResult, exitIP string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.egressLast = r
	p.exitIP = exitIP
	p.aggregateLocked()
}

// aggregateLocked computes the overall status from the latest probe results.
// Probes that have not run yet are left out of the report. A leak (exit IP
// mismatch) is critical: it forces StatusDown regardless of the other probes.
// The caller must hold p.mu.
func (p *Prober) aggregateLocked() {
	var probes []ProbeResult
	failed, leak := 0, false
	for _, pr := range []ProbeResult{p.tunnelLast, p.dnsLast, p.httpLast, p.egressLast} {
		if pr.LastRun == "" {
			continue
		}
		probes = append(probes, pr)
		if !pr.OK {
			failed++
		}
		leak = leak || pr.Leak
	}
	total := len(probes)
	var overall Status
	switch {
	case leak, total == 0, failed >= total:
		overall = StatusDown
	case failed == 0:
		overall = StatusHealthy
	default:
		overall = StatusDegraded
	}
	p.report = Report{Overall: overall, Probes: probes}
}
