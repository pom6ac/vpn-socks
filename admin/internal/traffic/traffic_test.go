package traffic

import (
	"testing"
)

func TestParseTun0_Valid(t *testing.T) {
	output := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
  eth0: 12345678  1000    0    0    0     0          0         0 87654321  900    0    0    0     0       0          0
  tun0: 100000  200    0    0    0     0          0         0 200000  150    0    0    0     0       0          0
`
	rx, tx, ok := parseTun0(output)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if rx != 100000 {
		t.Errorf("rx = %d, want 100000", rx)
	}
	if tx != 200000 {
		t.Errorf("tx = %d, want 200000", tx)
	}
}

func TestParseTun0_NoTun0(t *testing.T) {
	output := `Inter-|   Receive
 face |bytes
  eth0: 12345678  1000
`
	_, _, ok := parseTun0(output)
	if ok {
		t.Error("expected ok=false for no tun0")
	}
}

func TestParseTun0_EmptyOutput(t *testing.T) {
	_, _, ok := parseTun0("")
	if ok {
		t.Error("expected ok=false for empty output")
	}
}

func TestParseTun0_InsufficientFields(t *testing.T) {
	output := "  tun0: 100 200 300\n"
	_, _, ok := parseTun0(output)
	if ok {
		t.Error("expected ok=false for insufficient fields")
	}
}

func TestParseTun0_NoColon(t *testing.T) {
	output := "  tun0 100 200\n"
	_, _, ok := parseTun0(output)
	if ok {
		t.Error("expected ok=false for missing colon")
	}
}

func TestParseTun0_InvalidNumber(t *testing.T) {
	output := "  tun0: abc def ghi jkl mno pqr stu vwx 100 200 300 400 500 600 700 800\n"
	_, _, ok := parseTun0(output)
	if ok {
		t.Error("expected ok=false for invalid rx number")
	}

	output2 := "  tun0: 100 200 300 400 500 600 700 800 abc 200 300 400 500 600 700 800\n"
	_, _, ok = parseTun0(output2)
	if ok {
		t.Error("expected ok=false for invalid tx number")
	}
}

func TestNewReader(t *testing.T) {
	r := NewReader(nil, "vpn-ovpn")
	if r.container != "vpn-ovpn" {
		t.Errorf("container = %q, want %q", r.container, "vpn-ovpn")
	}
}

func TestCurrent_ZeroValue(t *testing.T) {
	r := NewReader(nil, "vpn-ovpn")
	s := r.Current()
	if s.OK != false {
		t.Error("expected OK=false for new reader")
	}
	if s.RXbps != 0 || s.TXbps != 0 {
		t.Error("expected zero bps for new reader")
	}
}
