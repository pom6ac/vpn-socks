package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// Config holds all runtime configuration for the admin panel.
type Config struct {
	ListenAddr  string
	SOCKS5Proxy string
	// SOCKS5 username/password (RFC 1929); empty user = no authentication.
	SOCKS5User         string
	SOCKS5Password     string
	SOCKS5PasswordFile string
	DBPath             string
	TunnelTarget       string
	TunnelPort         string
	DNSCheckURL        string
	HTTPCheckURLs      []string
	OVPNConfigPath     string
	ExitIPURL          string
	// ExpectedExitIP is the raw EXPECTED_EXIT_IP value: a comma-separated list
	// of acceptable VPN exit IPs (see ExpectedExitIPs).
	ExpectedExitIP string

	// Container names of the managed stack.
	OVPNContainer  string
	SOCKSContainer string
	AdminContainer string
	// ContainerNames lists the containers the panel may inspect, restart and
	// stream logs from (OVPNContainer and SOCKSContainer).
	ContainerNames []string

	// Compose settings used by "recreate". HostProjectDir is the absolute
	// project path on the host; it is bind-mounted into the admin container at
	// the same path so compose resolves relative paths identically.
	ComposeProject string
	HostProjectDir string
	ComposeFile    string

	// AllowedHosts are extra Host header values (hostname only, no port)
	// accepted in addition to loopback names.
	AllowedHosts []string
	// AdminTokenFile, when set, points to a file with a bearer token that is
	// required for /api/* requests.
	AdminTokenFile string
}

// Load reads configuration from environment variables with sensible defaults.
func Load() Config {
	c := Config{
		ListenAddr:  envOrDefault("LISTEN_ADDR", ":8080"),
		SOCKS5Proxy: envOrDefault("SOCKS5_PROXY", "ovpn:1080"),
		DBPath:      envOrDefault("DB_PATH", "/data/admin.db"),
		// Probe target must NOT be the VPN server itself: connections to the
		// VPN endpoint from inside the tunnel netns are unreliable (the server
		// blocks/unroutes them even when the tunnel is healthy), which caused
		// a permanent false "degraded". Use an always-reachable IP instead.
		TunnelTarget:   envOrDefault("TUNNEL_TARGET", "1.1.1.1"),
		TunnelPort:     envOrDefault("TUNNEL_PORT", "443"),
		DNSCheckURL:    envOrDefault("DNS_CHECK_URL", "http://example.com"),
		OVPNConfigPath: envOrDefault("OVPN_CONFIG_PATH", "/vpn/config.ovpn"),
		ExitIPURL:      envOrDefault("EXIT_IP_URL", "https://ifconfig.me/ip"),
		ExpectedExitIP: os.Getenv("EXPECTED_EXIT_IP"),

		OVPNContainer:  envOrDefault("OVPN_CONTAINER", "vpn-ovpn"),
		SOCKSContainer: envOrDefault("SOCKS_CONTAINER", "vpn-socks"),
		AdminContainer: envOrDefault("ADMIN_CONTAINER", "vpn-admin"),

		ComposeProject: envOrDefault("COMPOSE_PROJECT_NAME", "vpn-socks"),
		HostProjectDir: os.Getenv("HOST_PROJECT_DIR"),
		AdminTokenFile: os.Getenv("ADMIN_TOKEN_FILE"),

		SOCKS5User:         os.Getenv("PROXY_USER"),
		SOCKS5Password:     os.Getenv("PROXY_PASSWORD"),
		SOCKS5PasswordFile: os.Getenv("PROXY_PASSWORD_FILE"),
	}
	c.ContainerNames = []string{c.OVPNContainer, c.SOCKSContainer}
	c.ComposeFile = os.Getenv("COMPOSE_FILE")
	if c.ComposeFile == "" && c.HostProjectDir != "" {
		c.ComposeFile = filepath.Join(c.HostProjectDir, "docker-compose.yml")
	}
	c.HTTPCheckURLs = splitList(envOrDefault("HTTP_CHECK_URLS", "https://ifconfig.me,https://example.com"))
	c.AllowedHosts = splitList(os.Getenv("ADMIN_ALLOWED_HOSTS"))
	return c
}

// ExpectedExitIPs parses ExpectedExitIP. An empty value yields no addresses
// (the egress_ip probe then only reports the observed IP). Any invalid element
// is an error: a typo must not silently disable leak detection.
func (c Config) ExpectedExitIPs() ([]net.IP, error) {
	var ips []net.IP
	for _, v := range splitList(c.ExpectedExitIP) {
		ip := net.ParseIP(v)
		if ip == nil {
			return nil, fmt.Errorf("EXPECTED_EXIT_IP: invalid IP address %q", v)
		}
		ips = append(ips, ip)
	}
	return ips, nil
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
