package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/net/proxy"
)

// SOCKS5Auth returns the RFC 1929 username/password credentials the admin
// panel offers to the SOCKS5 proxy, or nil when PROXY_USER is empty (no
// authentication). The password comes from PROXY_PASSWORD_FILE when set
// (trailing newline trimmed), otherwise from PROXY_PASSWORD.
func (c Config) SOCKS5Auth() (*proxy.Auth, error) {
	if c.SOCKS5User == "" {
		return nil, nil
	}
	password := c.SOCKS5Password
	if c.SOCKS5PasswordFile != "" {
		b, err := os.ReadFile(c.SOCKS5PasswordFile)
		if err != nil {
			return nil, fmt.Errorf("read PROXY_PASSWORD_FILE: %w", err)
		}
		password = strings.TrimRight(string(b), "\r\n")
	}
	return &proxy.Auth{User: c.SOCKS5User, Password: password}, nil
}

// SOCKS5Dialer returns a context-aware SOCKS5 dialer for addr. auth may be
// nil (no authentication). It is shared by the health probes and the
// topology collector so both talk to the proxy the same way.
func SOCKS5Dialer(addr string, auth *proxy.Auth) (proxy.ContextDialer, error) {
	if addr == "" {
		return nil, errors.New("socks5 proxy address is not configured")
	}
	d, err := proxy.SOCKS5("tcp", addr, auth, proxy.Direct)
	if err != nil {
		return nil, err
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("socks5 dialer does not support contexts")
	}
	return cd, nil
}
