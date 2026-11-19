package config

import (
	"testing"
)

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("SOCKS5_PROXY", "")
	t.Setenv("COMPOSE_FILE", "")
	t.Setenv("HOST_PROJECT_DIR", "")
	t.Setenv("DB_PATH", "")
	t.Setenv("TUNNEL_TARGET", "")
	t.Setenv("TUNNEL_PORT", "")
	t.Setenv("DNS_CHECK_URL", "")
	t.Setenv("HTTP_CHECK_URLS", "")

	c := Load()

	if c.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want %q", c.ListenAddr, ":8080")
	}
	if c.SOCKS5Proxy != "ovpn:1080" {
		t.Errorf("SOCKS5Proxy = %q, want %q", c.SOCKS5Proxy, "ovpn:1080")
	}
	if c.ComposeFile != "" {
		t.Errorf("ComposeFile = %q, want empty without HOST_PROJECT_DIR", c.ComposeFile)
	}
	if c.ComposeProject != "vpn-socks" || c.OVPNContainer != "vpn-ovpn" || c.SOCKSContainer != "vpn-socks" || c.AdminContainer != "vpn-admin" {
		t.Errorf("names = %q %q %q %q", c.ComposeProject, c.OVPNContainer, c.SOCKSContainer, c.AdminContainer)
	}
	if c.AllowedHosts != nil || c.AdminTokenFile != "" {
		t.Errorf("AllowedHosts = %v, AdminTokenFile = %q", c.AllowedHosts, c.AdminTokenFile)
	}
	if c.HostProjectDir != "" {
		t.Errorf("HostProjectDir = %q, want empty", c.HostProjectDir)
	}
	if c.DBPath != "/data/admin.db" {
		t.Errorf("DBPath = %q, want %q", c.DBPath, "/data/admin.db")
	}
	if c.TunnelTarget != "1.1.1.1" {
		t.Errorf("TunnelTarget = %q, want %q", c.TunnelTarget, "1.1.1.1")
	}
	if c.TunnelPort != "443" {
		t.Errorf("TunnelPort = %q, want %q", c.TunnelPort, "443")
	}
	if c.DNSCheckURL != "http://example.com" {
		t.Errorf("DNSCheckURL = %q, want %q", c.DNSCheckURL, "http://example.com")
	}
	if len(c.ContainerNames) != 2 || c.ContainerNames[0] != "vpn-ovpn" || c.ContainerNames[1] != "vpn-socks" {
		t.Errorf("ContainerNames = %v, want [vpn-ovpn vpn-socks]", c.ContainerNames)
	}
	if len(c.HTTPCheckURLs) != 2 {
		t.Fatalf("HTTPCheckURLs len = %d, want 2", len(c.HTTPCheckURLs))
	}
	if c.HTTPCheckURLs[0] != "https://ifconfig.me" {
		t.Errorf("HTTPCheckURLs[0] = %q, want %q", c.HTTPCheckURLs[0], "https://ifconfig.me")
	}
	if c.HTTPCheckURLs[1] != "https://example.com" {
		t.Errorf("HTTPCheckURLs[1] = %q, want %q", c.HTTPCheckURLs[1], "https://example.com")
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	t.Setenv("LISTEN_ADDR", ":9090")
	t.Setenv("SOCKS5_PROXY", "127.0.0.1:1080")
	t.Setenv("COMPOSE_FILE", "/tmp/compose.yml")
	t.Setenv("HOST_PROJECT_DIR", "/srv/project")
	t.Setenv("DB_PATH", "/tmp/test.db")
	t.Setenv("TUNNEL_TARGET", "10.0.0.1")
	t.Setenv("TUNNEL_PORT", "1194")
	t.Setenv("DNS_CHECK_URL", "http://test.local")
	t.Setenv("HTTP_CHECK_URLS", "https://a.com, https://b.com ,https://c.com")

	c := Load()

	if c.ListenAddr != ":9090" {
		t.Errorf("ListenAddr = %q, want %q", c.ListenAddr, ":9090")
	}
	if c.SOCKS5Proxy != "127.0.0.1:1080" {
		t.Errorf("SOCKS5Proxy = %q, want %q", c.SOCKS5Proxy, "127.0.0.1:1080")
	}
	if c.ComposeFile != "/tmp/compose.yml" {
		t.Errorf("ComposeFile = %q, want %q", c.ComposeFile, "/tmp/compose.yml")
	}
	if c.HostProjectDir != "/srv/project" {
		t.Errorf("HostProjectDir = %q, want %q", c.HostProjectDir, "/srv/project")
	}
	if c.DBPath != "/tmp/test.db" {
		t.Errorf("DBPath = %q, want %q", c.DBPath, "/tmp/test.db")
	}
	if c.TunnelTarget != "10.0.0.1" {
		t.Errorf("TunnelTarget = %q, want %q", c.TunnelTarget, "10.0.0.1")
	}
	if c.TunnelPort != "1194" {
		t.Errorf("TunnelPort = %q, want %q", c.TunnelPort, "1194")
	}
	if c.DNSCheckURL != "http://test.local" {
		t.Errorf("DNSCheckURL = %q, want %q", c.DNSCheckURL, "http://test.local")
	}
	if len(c.HTTPCheckURLs) != 3 {
		t.Fatalf("HTTPCheckURLs len = %d, want 3", len(c.HTTPCheckURLs))
	}
	if c.HTTPCheckURLs[0] != "https://a.com" {
		t.Errorf("HTTPCheckURLs[0] = %q, want %q", c.HTTPCheckURLs[0], "https://a.com")
	}
	if c.HTTPCheckURLs[1] != "https://b.com" {
		t.Errorf("HTTPCheckURLs[1] = %q, want %q", c.HTTPCheckURLs[1], "https://b.com")
	}
	if c.HTTPCheckURLs[2] != "https://c.com" {
		t.Errorf("HTTPCheckURLs[2] = %q, want %q", c.HTTPCheckURLs[2], "https://c.com")
	}
}

func TestLoad_HTTPCheckURLs_EmptyEntries(t *testing.T) {
	t.Setenv("HTTP_CHECK_URLS", "https://a.com,, ,https://b.com")

	c := Load()

	if len(c.HTTPCheckURLs) != 2 {
		t.Fatalf("HTTPCheckURLs len = %d, want 2 (empty entries should be skipped)", len(c.HTTPCheckURLs))
	}
}

func TestEnvOrDefault(t *testing.T) {
	t.Setenv("TEST_ENV_VAR", "custom-value")
	if v := envOrDefault("TEST_ENV_VAR", "default"); v != "custom-value" {
		t.Errorf("got %q, want %q", v, "custom-value")
	}

	t.Setenv("TEST_ENV_VAR", "")
	if v := envOrDefault("TEST_ENV_VAR", "default"); v != "default" {
		t.Errorf("got %q, want %q for empty env", v, "default")
	}

	if v := envOrDefault("NONEXISTENT_ENV_VAR_XYZ", "default"); v != "default" {
		t.Errorf("got %q, want %q for unset env", v, "default")
	}
}

func TestLoad_ContainerNamesAndCompose(t *testing.T) {
	t.Setenv("OVPN_CONTAINER", "my-ovpn")
	t.Setenv("SOCKS_CONTAINER", "my-socks")
	t.Setenv("ADMIN_CONTAINER", "my-admin")
	t.Setenv("COMPOSE_PROJECT_NAME", "proj")
	t.Setenv("COMPOSE_FILE", "")
	t.Setenv("HOST_PROJECT_DIR", "/srv/vpn-socks")
	t.Setenv("ADMIN_ALLOWED_HOSTS", " vpn.lan, ,10.0.0.5 ")
	t.Setenv("ADMIN_TOKEN_FILE", "/run/secrets/admin_token")

	c := Load()
	if c.OVPNContainer != "my-ovpn" || c.SOCKSContainer != "my-socks" || c.AdminContainer != "my-admin" {
		t.Errorf("containers = %q %q %q", c.OVPNContainer, c.SOCKSContainer, c.AdminContainer)
	}
	if len(c.ContainerNames) != 2 || c.ContainerNames[0] != "my-ovpn" || c.ContainerNames[1] != "my-socks" {
		t.Errorf("ContainerNames = %v", c.ContainerNames)
	}
	if c.ComposeProject != "proj" {
		t.Errorf("ComposeProject = %q", c.ComposeProject)
	}
	if c.ComposeFile != "/srv/vpn-socks/docker-compose.yml" {
		t.Errorf("ComposeFile = %q, want derived from HOST_PROJECT_DIR", c.ComposeFile)
	}
	if len(c.AllowedHosts) != 2 || c.AllowedHosts[0] != "vpn.lan" || c.AllowedHosts[1] != "10.0.0.5" {
		t.Errorf("AllowedHosts = %q", c.AllowedHosts)
	}
	if c.AdminTokenFile != "/run/secrets/admin_token" {
		t.Errorf("AdminTokenFile = %q", c.AdminTokenFile)
	}
}

func TestExpectedExitIPs(t *testing.T) {
	t.Setenv("EXPECTED_EXIT_IP", " 203.0.113.7, 2001:db8::1 ,,")
	ips, err := Load().ExpectedExitIPs()
	if err != nil || len(ips) != 2 || ips[0].String() != "203.0.113.7" || ips[1].String() != "2001:db8::1" {
		t.Fatalf("ips = %v, err = %v", ips, err)
	}

	t.Setenv("EXPECTED_EXIT_IP", "")
	if ips, err := Load().ExpectedExitIPs(); err != nil || len(ips) != 0 {
		t.Errorf("empty: ips = %v, err = %v", ips, err)
	}

	t.Setenv("EXPECTED_EXIT_IP", "203.0.113.7,vpn.example.com")
	if _, err := Load().ExpectedExitIPs(); err == nil {
		t.Error("invalid element must be an error")
	}
}
