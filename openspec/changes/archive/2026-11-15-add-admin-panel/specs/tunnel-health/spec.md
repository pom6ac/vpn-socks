## ADDED Requirements

### Requirement: Tunnel ping probe
The system SHALL periodically (every 5 seconds) test connectivity through the SOCKS5 proxy by establishing a TCP connection via `host.docker.internal:1080` to an always-reachable endpoint. The target SHALL NOT be the VPN server IP: the VPN endpoint is unreachable from inside the tunnel network namespace even when the tunnel is healthy, which produced a permanent false "degraded" status. The target IP SHALL be configurable via the `TUNNEL_TARGET` environment variable (default: `1.1.1.1`, port via `TUNNEL_PORT`, default: `443`).

#### Scenario: Tunnel is up
- **WHEN** the 2nd hop is reachable through SOCKS5
- **THEN** the admin panel displays tunnel status as "up" with green indicator and shows latency in milliseconds

#### Scenario: Tunnel is down
- **WHEN** the TCP connection through SOCKS5 fails or times out (timeout: 5 seconds)
- **THEN** the admin panel displays tunnel status as "down" with red indicator

### Requirement: DNS resolve probe
The system SHALL periodically (every 10 seconds) test DNS resolution through the tunnel by performing an HTTP request through SOCKS5 to a domain name (not a raw IP). The check URL SHALL be configurable via the `DNS_CHECK_URL` environment variable (default: `http://example.com`).

#### Scenario: DNS resolves through tunnel
- **WHEN** a domain name resolves successfully through the SOCKS5 proxy (e.g. HTTP GET to `http://example.com` via SOCKS5 returns a response)
- **THEN** the admin panel displays DNS status as "ok" with green indicator

#### Scenario: DNS resolution fails
- **WHEN** the DNS resolution through SOCKS5 fails or times out (timeout: 10 seconds)
- **THEN** the admin panel displays DNS status as "fail" with red indicator

### Requirement: HTTP availability probe
The system SHALL periodically (every 30 seconds) test end-to-end HTTP availability through the SOCKS5 proxy by requesting 2-3 reference URLs and checking for HTTP 200 status. The reference URLs SHALL be configurable via the `HTTP_CHECK_URLS` environment variable (comma-separated, default: `https://ifconfig.me,https://example.com`).

#### Scenario: Websites accessible through tunnel
- **WHEN** at least one reference URL returns HTTP 200 through SOCKS5
- **THEN** the admin panel displays HTTP status as "ok" with green indicator

#### Scenario: Websites not accessible
- **WHEN** all reference URLs fail or return non-200 status through SOCKS5
- **THEN** the admin panel displays HTTP status as "fail" with red indicator

### Requirement: Health overview aggregation
The system SHALL display a combined health summary that aggregates all probe results (tunnel ping, DNS, HTTP) into a single status indicator.

#### Scenario: All probes pass
- **WHEN** tunnel ping, DNS resolve, and HTTP availability probes all succeed
- **THEN** the admin panel displays an overall health indicator as "healthy" (green)

#### Scenario: Any probe fails
- **WHEN** one or more probes fail
- **THEN** the admin panel displays an overall health indicator as "degraded" (yellow) or "down" (red, if all probes fail)
