// Package socks5test provides a minimal SOCKS5 (RFC 1928) server for tests,
// optionally requiring username/password authentication (RFC 1929). Only
// the CONNECT command is supported.
package socks5test

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	version      = 0x05
	methodNone   = 0x00
	methodPasswd = 0x02
	methodNoneOK = 0xFF
)

// Server is a running test SOCKS5 server.
type Server struct {
	Addr string

	user, password string
	ln             net.Listener
	wg             sync.WaitGroup

	authFailures atomic.Int64
	connects     atomic.Int64
}

// Start starts a SOCKS5 server on 127.0.0.1. When user is non-empty, clients
// must authenticate with user/password; otherwise no authentication is
// required. The server is closed by t.Cleanup.
func Start(t testing.TB, user, password string) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socks5test: listen: %v", err)
	}
	s := &Server{Addr: ln.Addr().String(), user: user, password: password, ln: ln}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(s.Close)
	return s
}

// Close stops the listener and waits for the accept loop to exit.
func (s *Server) Close() {
	_ = s.ln.Close()
	s.wg.Wait()
}

// AuthFailures is the number of rejected handshakes (wrong or missing
// credentials).
func (s *Server) AuthFailures() int64 { return s.authFailures.Load() }

// Connects is the number of successfully authenticated CONNECT requests.
func (s *Server) Connects() int64 { return s.connects.Load() }

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = c.Close() }()
			_ = s.handle(c)
		}()
	}
}

func (s *Server) handle(c net.Conn) error {
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return err
	}
	if hdr[0] != version {
		return errors.New("bad version")
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return err
	}
	want := byte(methodNone)
	if s.user != "" {
		want = methodPasswd
	}
	offered := false
	for _, m := range methods {
		offered = offered || m == want
	}
	if !offered {
		s.authFailures.Add(1)
		_, _ = c.Write([]byte{version, methodNoneOK})
		return errors.New("no acceptable method")
	}
	if _, err := c.Write([]byte{version, want}); err != nil {
		return err
	}
	if want == methodPasswd {
		if err := s.checkPassword(c); err != nil {
			return err
		}
	}
	return s.connect(c)
}

// checkPassword handles the RFC 1929 sub-negotiation.
func (s *Server) checkPassword(c net.Conn) error {
	readField := func() (string, error) {
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return "", err
		}
		b := make([]byte, l[0])
		_, err := io.ReadFull(c, b)
		return string(b), err
	}
	var ver [1]byte
	if _, err := io.ReadFull(c, ver[:]); err != nil {
		return err
	}
	user, err := readField()
	if err != nil {
		return err
	}
	pass, err := readField()
	if err != nil {
		return err
	}
	if ver[0] != 0x01 || user != s.user || pass != s.password {
		s.authFailures.Add(1)
		_, _ = c.Write([]byte{0x01, 0x01})
		return errors.New("bad credentials")
	}
	_, err = c.Write([]byte{0x01, 0x00})
	return err
}

func (s *Server) connect(c net.Conn) error {
	var req [4]byte
	if _, err := io.ReadFull(c, req[:]); err != nil {
		return err
	}
	if req[0] != version || req[1] != 0x01 {
		_, _ = c.Write([]byte{version, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return errors.New("unsupported command")
	}
	var host string
	switch req[3] {
	case 0x01:
		var ip [4]byte
		if _, err := io.ReadFull(c, ip[:]); err != nil {
			return err
		}
		host = net.IP(ip[:]).String()
	case 0x03:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return err
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(c, b); err != nil {
			return err
		}
		host = string(b)
	case 0x04:
		var ip [16]byte
		if _, err := io.ReadFull(c, ip[:]); err != nil {
			return err
		}
		host = net.IP(ip[:]).String()
	default:
		_, _ = c.Write([]byte{version, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return errors.New("bad address type")
	}
	var p [2]byte
	if _, err := io.ReadFull(c, p[:]); err != nil {
		return err
	}
	target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(p[:])))))
	if err != nil {
		_, _ = c.Write([]byte{version, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return err
	}
	defer func() { _ = target.Close() }()
	s.connects.Add(1)
	if _, err := c.Write([]byte{version, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(target, c)
		if tc, ok := target.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		close(done)
	}()
	_, _ = io.Copy(c, target)
	_ = c.Close()
	<-done
	return nil
}
