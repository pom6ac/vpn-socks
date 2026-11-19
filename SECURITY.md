# Security Policy

## Reporting a vulnerability

Please report security issues via **GitHub Security Advisories**
("Report a vulnerability" on the repository's Security tab) rather than a
public issue. Include a description, reproduction steps and the affected
commit or image digest. You will get a response as soon as possible; fixes
are coordinated privately and released in a patch commit.

## Threat model (short)

This project is designed to run **on a single trusted machine, bound to
loopback only**:

- The SOCKS5 proxy (`127.0.0.1:1080`) and the admin panel (`127.0.0.1:8090`)
  are published on localhost only. They are not protected against other local
  users — do not run this on a shared machine without enabling SOCKS auth.
- The admin panel has **no authentication by default**. It enforces a Host
  allowlist (`421` otherwise, against DNS rebinding) and same-origin +
  `X-Requested-With: vpn-admin` headers on all non-GET requests, which
  protects against drive-by CSRF from web pages, not against a local
  attacker. An optional bearer token (`ADMIN_TOKEN_FILE`, provided as a
  compose secret via `docker-compose.override.yml.example`) protects `/api/*`.
- The `admin` container mounts **`/var/run/docker.sock`**, which is
  equivalent to root on the host. The container itself is hardened
  (`read_only`, `no-new-privileges`, `cap_drop: ALL`), but the socket remains
  the main trust boundary. A socket proxy (e.g.
  [tecnativa/docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy))
  is the recommended further hardening.
- OpenVPN credentials and private keys live in the `.ovpn` file, which is
  gitignored and never committed.
- **Traffic leaks.** The `ovpn` kill switch (`ovpn/killswitch-entrypoint.sh`)
  drops egress of the OpenVPN/socks5 network namespace unless it goes through
  the tunnel, to the VPN server's OpenVPN port/protocol, to loopback, or is a
  reply to an inbound connection; it fails closed (OpenVPN does not start) if
  it cannot be installed. The `egress_ip` probe independently checks the exit
  IP against `EXPECTED_EXIT_IP` and reports a mismatch as a leak (`down`).
  Not covered: DNS queries forwarded by Docker's embedded DNS from outside the
  container namespace (see [docs/kill-switch.md](docs/kill-switch.md) — the
  stack uses the VPN-pushed resolvers via `-d`), a VPN server whose DNS record changes while
  the container runs (needs a restart), and a compromised VPN server or exit
  IP service. The exit IP service sees your VPN exit IP; only a valid IP
  address from its response is shown in the panel.
