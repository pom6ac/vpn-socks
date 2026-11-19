package traffic

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeExec returns a scripted sequence of /proc/net/dev outputs or errors.
type fakeExec struct {
	steps []step
	i     int
}

type step struct {
	rx, tx int64
	err    error
	raw    string
}

func (f *fakeExec) ExecProcNetDev(context.Context, string) (string, error) {
	s := f.steps[f.i]
	if f.i < len(f.steps)-1 {
		f.i++
	}
	if s.err != nil {
		return "", s.err
	}
	if s.raw != "" {
		return s.raw, nil
	}
	return fmt.Sprintf("  tun0: %d 0 0 0 0 0 0 0 %d 0 0 0 0 0 0 0\n", s.rx, s.tx), nil
}

// newScripted returns a Reader whose clock advances one second per sample.
func newScripted(steps ...step) *Reader {
	r := NewReader(&fakeExec{steps: steps}, "vpn-ovpn")
	t0 := time.Unix(1_700_000_000, 0)
	n := 0
	r.now = func() time.Time { n++; return t0.Add(time.Duration(n) * time.Second) }
	return r
}

func TestSample_Rate(t *testing.T) {
	r := newScripted(step{rx: 1000, tx: 2000}, step{rx: 1500, tx: 2600})
	r.sample(context.Background())
	if s := r.Current(); !s.OK || s.RXbps != 0 {
		t.Fatalf("first sample = %+v, want OK with zero rate", s)
	}
	r.sample(context.Background())
	if s := r.Current(); !s.OK || s.RXbps != 500 || s.TXbps != 600 {
		t.Fatalf("second sample = %+v, want 500/600", s)
	}
}

func TestSample_CounterResetIsNotNegative(t *testing.T) {
	r := newScripted(
		step{rx: 1_000_000, tx: 2_000_000},
		step{rx: 100, tx: 200}, // tun0 recreated: counters restart
		step{rx: 400, tx: 1200},
	)
	r.sample(context.Background())
	r.sample(context.Background())
	if s := r.Current(); !s.OK || s.RXbps != 0 || s.TXbps != 0 {
		t.Fatalf("after reset = %+v, want OK with zero rate", s)
	}
	r.sample(context.Background())
	if s := r.Current(); s.RXbps != 300 || s.TXbps != 1000 {
		t.Fatalf("after new baseline = %+v, want 300/1000", s)
	}
}

func TestSample_OnlyTXReset(t *testing.T) {
	r := newScripted(step{rx: 100, tx: 5000}, step{rx: 200, tx: 10})
	r.sample(context.Background())
	r.sample(context.Background())
	if s := r.Current(); s.RXbps < 0 || s.TXbps < 0 {
		t.Fatalf("negative rate: %+v", s)
	}
}

func TestSample_ErrorResetsBaseline(t *testing.T) {
	r := newScripted(
		step{rx: 1000, tx: 1000},
		step{err: errors.New("exec failed")},
		step{rx: 9000, tx: 9000},
	)
	r.sample(context.Background())
	r.sample(context.Background())
	if s := r.Current(); s.OK {
		t.Fatalf("after error = %+v, want !OK", s)
	}
	r.mu.RLock()
	zero := r.lastTime.IsZero()
	r.mu.RUnlock()
	if !zero {
		t.Fatal("lastTime must be reset on error")
	}
	r.sample(context.Background())
	// No rate across the gap: the first good sample is a new baseline.
	if s := r.Current(); !s.OK || s.RXbps != 0 {
		t.Fatalf("after recovery = %+v, want OK with zero rate", s)
	}
}

func TestSample_ParseFailure(t *testing.T) {
	r := newScripted(step{rx: 1, tx: 1}, step{raw: "eth0: 1 2 3\n"})
	r.sample(context.Background())
	r.sample(context.Background())
	if s := r.Current(); s.OK {
		t.Fatalf("sample = %+v, want !OK", s)
	}
}

func TestSample_NilClient(t *testing.T) {
	r := NewReader(nil, "vpn-ovpn")
	r.sample(context.Background())
	if r.Current().OK {
		t.Fatal("want !OK with nil client")
	}
}

func TestRun_StopsOnCancel(t *testing.T) {
	r := newScripted(step{rx: 1, tx: 1})
	r.interval = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	r.Run(ctx)
	if !r.Current().OK {
		t.Fatal("expected a successful sample")
	}
}
