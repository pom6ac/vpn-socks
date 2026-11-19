package traffic

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sample holds a traffic sample (bytes per second).
type Sample struct {
	RXbps int64 `json:"rx_bps"`
	TXbps int64 `json:"tx_bps"`
	OK    bool  `json:"ok"`
}

// Execer reads /proc/net/dev inside a container.
type Execer interface {
	ExecProcNetDev(ctx context.Context, name string) (string, error)
}

// Reader periodically reads tun0 traffic from the ovpn container via docker exec.
type Reader struct {
	client    Execer
	container string
	interval  time.Duration
	now       func() time.Time

	mu       sync.RWMutex
	lastRX   int64
	lastTX   int64
	lastTime time.Time
	current  Sample
}

// NewReader creates a traffic Reader.
func NewReader(client Execer, container string) *Reader {
	return &Reader{client: client, container: container, interval: time.Second, now: time.Now}
}

// Run polls once per second and blocks until ctx is cancelled.
func (r *Reader) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	r.sample(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sample(ctx)
		}
	}
}

// Current returns the latest traffic sample.
func (r *Reader) Current() Sample {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current
}

func (r *Reader) sample(ctx context.Context) {
	if r.client == nil {
		r.fail()
		return
	}
	out, err := r.client.ExecProcNetDev(ctx, r.container)
	if err != nil {
		log.Printf("traffic exec failed: %v", err)
		r.fail()
		return
	}
	rx, tx, ok := parseTun0(out)
	if !ok {
		r.fail()
		return
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.lastTime.IsZero():
		r.current = Sample{OK: true}
	case rx < r.lastRX || tx < r.lastTX:
		// Counters went backwards: tun0 was recreated (reconnect). Treat it
		// as a reset and start a new baseline instead of reporting
		// negative rates.
		r.current = Sample{OK: true}
	default:
		if elapsed := now.Sub(r.lastTime).Seconds(); elapsed > 0 {
			r.current = Sample{
				RXbps: int64(float64(rx-r.lastRX) / elapsed),
				TXbps: int64(float64(tx-r.lastTX) / elapsed),
				OK:    true,
			}
		}
	}
	r.lastRX = rx
	r.lastTX = tx
	r.lastTime = now
}

// fail marks traffic as unavailable and drops the baseline so the next good
// sample does not compute a rate across the gap.
func (r *Reader) fail() {
	r.mu.Lock()
	r.current = Sample{OK: false}
	r.lastTime = time.Time{}
	r.mu.Unlock()
}

// parseTun0 parses /proc/net/dev output and returns rx_bytes, tx_bytes for tun0.
func parseTun0(output string) (rx, tx int64, ok bool) {
	for _, line := range strings.Split(output, "\n") {
		// /proc/net/dev format: "interfacename: rx_bytes rx_packets ... tx_bytes ..."
		name, rest, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found || strings.TrimSpace(name) != "tun0" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 16 {
			continue
		}
		rxVal, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		txVal, err := strconv.ParseInt(fields[8], 10, 64)
		if err != nil {
			continue
		}
		return rxVal, txVal, true
	}
	return 0, 0, false
}
