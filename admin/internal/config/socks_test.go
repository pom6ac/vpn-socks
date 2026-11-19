package config

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pom6ac/vpn-socks/admin/internal/socks5test"
)

func TestSOCKS5Auth_EmptyMeansNone(t *testing.T) {
	t.Setenv("PROXY_USER", "")
	t.Setenv("PROXY_PASSWORD", "ignored")
	t.Setenv("PROXY_PASSWORD_FILE", "")
	a, err := Load().SOCKS5Auth()
	if err != nil || a != nil {
		t.Fatalf("auth = %+v, err = %v; want nil without PROXY_USER", a, err)
	}
}

func TestSOCKS5Auth_FromEnv(t *testing.T) {
	t.Setenv("PROXY_USER", "alice")
	t.Setenv("PROXY_PASSWORD", "s3cret")
	t.Setenv("PROXY_PASSWORD_FILE", "")
	a, err := Load().SOCKS5Auth()
	if err != nil || a == nil || a.User != "alice" || a.Password != "s3cret" {
		t.Fatalf("auth = %+v, err = %v", a, err)
	}
}

func TestSOCKS5Auth_FileWins(t *testing.T) {
	f := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(f, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXY_USER", "alice")
	t.Setenv("PROXY_PASSWORD", "from-env")
	t.Setenv("PROXY_PASSWORD_FILE", f)
	a, err := Load().SOCKS5Auth()
	if err != nil || a.Password != "from-file" {
		t.Fatalf("auth = %+v, err = %v", a, err)
	}
}

func TestSOCKS5Auth_FileError(t *testing.T) {
	c := Config{SOCKS5User: "alice", SOCKS5PasswordFile: filepath.Join(t.TempDir(), "missing")}
	if _, err := c.SOCKS5Auth(); err == nil {
		t.Fatal("expected error for missing password file")
	}
}

func TestSOCKS5Dialer_EmptyAddr(t *testing.T) {
	if _, err := SOCKS5Dialer("", nil); err == nil {
		t.Fatal("expected error")
	}
}

// echoServer accepts one connection and echoes what it reads.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				buf := make([]byte, 64)
				n, _ := c.Read(buf)
				_, _ = c.Write(buf[:n])
			}()
		}
	}()
	return ln.Addr().String()
}

func TestSOCKS5Dialer_RFC1929(t *testing.T) {
	srv := socks5test.Start(t, "alice", "s3cret")
	target := echoServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := Config{SOCKS5User: "alice", SOCKS5Password: "s3cret"}
	auth, err := cfg.SOCKS5Auth()
	if err != nil {
		t.Fatal(err)
	}
	d, err := SOCKS5Dialer(srv.Addr, auth)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		t.Fatalf("dial with credentials: %v", err)
	}
	_, _ = conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := conn.Read(buf); err != nil || string(buf) != "ping" {
		t.Errorf("echo = %q, %v", buf, err)
	}
	_ = conn.Close()

	for name, a := range map[string]*Config{
		"no credentials": {},
		"wrong password": {SOCKS5User: "alice", SOCKS5Password: "nope"},
	} {
		auth, _ := a.SOCKS5Auth()
		d, err := SOCKS5Dialer(srv.Addr, auth)
		if err != nil {
			t.Fatal(err)
		}
		if c, err := d.DialContext(ctx, "tcp", target); err == nil {
			_ = c.Close()
			t.Errorf("%s: dial must fail on an auth-only proxy", name)
		}
	}
	if srv.AuthFailures() != 2 || srv.Connects() != 1 {
		t.Errorf("auth failures = %d, connects = %d", srv.AuthFailures(), srv.Connects())
	}
}
