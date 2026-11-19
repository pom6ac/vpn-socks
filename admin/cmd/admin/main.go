package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/pom6ac/vpn-socks/admin/internal/config"
	"github.com/pom6ac/vpn-socks/admin/internal/dockerapi"
	"github.com/pom6ac/vpn-socks/admin/internal/health"
	"github.com/pom6ac/vpn-socks/admin/internal/server"
	"github.com/pom6ac/vpn-socks/admin/internal/storage"
	"github.com/pom6ac/vpn-socks/admin/internal/topology"
	"github.com/pom6ac/vpn-socks/admin/internal/traffic"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("admin: %v", err)
	}
}

func run() error {
	cfg := config.Load()
	log.Printf("admin starting on %s", cfg.ListenAddr)
	socksAuth, err := cfg.SOCKS5Auth()
	if err != nil {
		return err
	}
	expectedExitIPs, err := cfg.ExpectedExitIPs()
	if err != nil {
		return err
	}

	store, err := storage.New(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	log.Printf("sqlite opened at %s", cfg.DBPath)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	dockerCli := dockerapi.NewClient()

	prober := health.NewProber(cfg.SOCKS5Proxy, socksAuth, cfg.TunnelTarget, cfg.TunnelPort, cfg.DNSCheckURL, cfg.HTTPCheckURLs, cfg.ExitIPURL, expectedExitIPs)
	prober.Run(ctx)

	trafficReader := traffic.NewReader(dockerCli, cfg.OVPNContainer)
	go trafficReader.Run(ctx)

	go store.PruneLoop(ctx, time.Hour)
	metricsTick := time.NewTicker(time.Second)
	defer metricsTick.Stop()
	go recordMetrics(ctx, store, prober, trafficReader, metricsTick.C)
	go recordContainerEvents(ctx, store, dockerCli, cfg.ContainerNames)

	topo := topology.NewCollector(dockerCli, cfg.OVPNContainer, cfg.OVPNConfigPath, cfg.SOCKS5Proxy, prober.ExitIP)
	go topo.Run(ctx)

	srv, err := server.New(cfg, dockerCli, prober, trafficReader, store, topo)
	if err != nil {
		return err
	}
	httpSrv := newHTTPServer(ctx, cfg.ListenAddr, srv.Routes())
	return serve(ctx, cancel, httpSrv)
}

func newHTTPServer(ctx context.Context, addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// SSE log streams lift this per request via ResponseController.
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

// serve runs httpSrv until ctx is done or the listener fails, then shuts it
// down. cancel is called before Shutdown so that long-lived requests (SSE),
// whose contexts derive from ctx via BaseContext, end promptly.
func serve(ctx context.Context, cancel context.CancelFunc, httpSrv *http.Server) error {
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	log.Printf("admin listening on %s", httpSrv.Addr)

	var serveErr error
	select {
	case <-ctx.Done():
		log.Printf("admin shutting down...")
	case serveErr = <-errCh:
	}
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}

// rateLogger logs at most once per interval, counting suppressed messages.
type rateLogger struct {
	interval   time.Duration
	last       time.Time
	suppressed int
}

func (l *rateLogger) Printf(format string, args ...any) {
	now := time.Now()
	if !l.last.IsZero() && now.Sub(l.last) < l.interval {
		l.suppressed++
		return
	}
	if l.suppressed > 0 {
		log.Printf("(%d similar messages suppressed)", l.suppressed)
		l.suppressed = 0
	}
	l.last = now
	log.Printf(format, args...)
}

// metricWriter is the part of the store recordMetrics needs.
type metricWriter interface {
	WriteMetric(kind string, value float64) error
}

// eventWriter is the part of the store recordContainerEvents needs.
type eventWriter interface {
	WriteContainerEvent(container, event string) error
}

// recordMetrics writes traffic on every tick and each probe result once (when
// its LastRun changes) to the store.
func recordMetrics(ctx context.Context, store metricWriter, prober *health.Prober, tr *traffic.Reader, ticks <-chan time.Time) {
	errLog := &rateLogger{interval: time.Minute}
	write := func(kind string, v float64) {
		if err := store.WriteMetric(kind, v); err != nil {
			errLog.Printf("write metric %s failed: %v", kind, err)
		}
	}
	lastRun := map[string]string{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		if tr != nil {
			if s := tr.Current(); s.OK {
				write("rx_bytes", float64(s.RXbps))
				write("tx_bytes", float64(s.TXbps))
			}
		}
		for _, p := range prober.GetReport().Probes {
			if p.LastRun == "" || lastRun[p.Name] == p.LastRun {
				continue
			}
			lastRun[p.Name] = p.LastRun
			val := -1.0
			if p.OK {
				val = p.Latency
			}
			write(p.Name, val)
		}
	}
}

// eventSource is the part of the Docker client recordContainerEvents needs.
type eventSource interface {
	Events(ctx context.Context, names []string) (<-chan dockerapi.EventMessage, <-chan error)
}

// recordContainerEvents subscribes to Docker events and records them. The
// subscription reconnects (with backoff) inside Events; errors are logged.
func recordContainerEvents(ctx context.Context, store eventWriter, src eventSource, names []string) {
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}
	msgs, errCh := src.Events(ctx, names)
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-msgs:
			if !ok {
				return
			}
			name := msg.Actor.Attributes["name"]
			if !nameSet[name] {
				continue
			}
			if err := store.WriteContainerEvent(name, msg.Action); err != nil {
				log.Printf("write container event failed: %v", err)
			}
		case err, ok := <-errCh:
			if !ok {
				errCh = nil
				continue
			}
			log.Printf("docker events error: %v; reconnecting", err)
		}
	}
}
