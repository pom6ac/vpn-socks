# vpn-socks

[![CI](https://github.com/pom6ac/vpn-socks/actions/workflows/ci.yml/badge.svg)](https://github.com/pom6ac/vpn-socks/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

An OpenVPN client in Docker that exposes a SOCKS5 proxy on `127.0.0.1:1080`,
with a kill switch and a small local web panel for status, logs and health
checks.

It is meant for the case where only some applications (a browser, `curl`, a
CLI tool) should go through a VPN, while the rest of the machine keeps its
normal network connection. Nothing on the host is rerouted: an application
uses the VPN only when it is configured to use the proxy.

## Features

- **SOCKS5 proxy through OpenVPN**, published on loopback only
  (`127.0.0.1:1080`); optional username/password authentication.
- **Kill switch**: an `iptables` egress allowlist in the VPN container. When
  the tunnel is down, proxied traffic fails instead of leaving with your real
  IP. It fails closed: if it cannot be installed, OpenVPN does not start.
- **Health probes** through the proxy: tunnel TCP connect, DNS, HTTP and an
  exit IP check (`egress_ip`) that reports a leak when the exit IP is not one
  you expect.
- **Admin panel** on `127.0.0.1:8090`: container status, tunnel health, live
  `tun0` traffic and history, network topology, streamed logs, and buttons to
  restart containers, reconnect OpenVPN or recreate the VPN services.
- Images pinned by digest; hardened admin container; optional API token.

## How it works

```text
 browser / apps (proxy socks5://127.0.0.1:1080)
        │
        ▼
 ┌───────────────────────────── Docker ───────────────────────────┐
 │  ovpn   (dperson/openvpn-client)       container vpn-ovpn      │
 │    ├── kill switch (iptables, ovpn/killswitch-entrypoint.sh)   │
 │    ├── tun0 — OpenVPN tunnel to your VPN server                │
 │    └── publishes 127.0.0.1:1080                                │
 │  socks5 (serjs/go-socks5-proxy)        container vpn-socks     │
 │    └── network_mode: service:ovpn (shares the tunnel netns)    │
 │  admin  (built from ./admin)           container vpn-admin     │
 │    ├── http://127.0.0.1:8090 — monitoring & control panel      │
 │    ├── probes reach the proxy at ovpn:1080 (compose network)   │
 │    └── uses the Docker socket and the host compose file        │
 └────────────────────────────────────────────────────────────────┘
```

- **ovpn** connects to your VPN server with your `.ovpn` file (never
  committed). Its network namespace is the only way out for proxied traffic.
- **socks5** runs inside the ovpn network namespace, so every connection made
  through `127.0.0.1:1080` leaves through the tunnel.
- **admin** is a Go service with an embedded web UI. It reads container state
  and logs through the Docker socket and runs `docker compose` for "Recreate
  stack".

## Quick start

Requirements: Docker with Compose v2 (Docker Desktop on macOS/Windows, or
Docker Engine on Linux) and `/dev/net/tun` (Docker Desktop provides it), plus
an OpenVPN client config (`.ovpn`) from your VPN provider.

From a clone of this repository:

```sh
# 1. Create .env; HOST_PROJECT_DIR must be the absolute path of this directory
#    (docker-compose.yml refuses to start without it).
sed "s|^HOST_PROJECT_DIR=.*|HOST_PROJECT_DIR=$PWD|" .env.example > .env

# 2. Put your OpenVPN config at ./client.ovpn (or set OVPN_CONFIG in .env).
cp /path/to/your.ovpn client.ovpn

# 3. Start the stack (the first run builds the admin image).
docker compose up -d        # or: make up
```

Check that traffic exits through the VPN:

```sh
curl --socks5-hostname 127.0.0.1:1080 https://ifconfig.me/ip
```

This should print your VPN server's exit IP, not your own. Then open the
panel at <http://127.0.0.1:8090>.

Recommended next step: put that IP into `EXPECTED_EXIT_IP` in `.env` and run
`docker compose up -d` again, so the panel reports a leak if traffic ever
exits elsewhere.

## Configuration

Settings are read from `.env` (see [.env.example](.env.example)):

| Variable             | Default         | Purpose |
| -------------------- | --------------- | ------- |
| `HOST_PROJECT_DIR`   | — (required)    | Absolute path of this directory on the host. |
| `OVPN_CONFIG`        | `./client.ovpn` | OpenVPN config file. |
| `SOCKS_REQUIRE_AUTH` | `false`         | Require SOCKS5 username/password. |
| `PROXY_USER`         | empty           | SOCKS5 username (also used by the panel's probes). |
| `PROXY_PASSWORD`     | empty           | SOCKS5 password (also used by the panel's probes). |
| `EXPECTED_EXIT_IP`   | empty           | Comma-separated expected exit IPs; a mismatch is a leak. |
| `ADMIN_ALLOWED_HOSTS`| empty           | Extra host names accepted by the panel. |
| `VPN_ENDPOINT_HOST`  | empty           | VPN server host(s)/IP(s) for the kill switch instead of the config's `remote` hosts. |

The full reference — every admin panel variable with its default, the optional
API token, `PROXY_PASSWORD_FILE` and `VPN_ENDPOINT_HOST` — is in
[docs/configuration.md](docs/configuration.md).

## Using the proxy

Any SOCKS5 client can use `127.0.0.1:1080`. Let the proxy resolve host names
(`socks5h://`, "remote DNS"), otherwise lookups happen on the host outside the
VPN.

```sh
curl --socks5-hostname 127.0.0.1:1080 https://example.com
ALL_PROXY=socks5h://127.0.0.1:1080 some-tool   # tools that honour ALL_PROXY
```

Browsers (Firefox, Chromium), PAC files for sending only some sites through
the VPN, the macOS system proxy and SOCKS5 authentication:
[docs/using-the-proxy.md](docs/using-the-proxy.md).

## Admin panel

<!-- screenshot: docs/admin.png -->

<http://127.0.0.1:8090> (published on loopback only) shows:

- **Network** — the path host → `socks5` → `ovpn` (`tun0` address) → VPN
  server (`remote` host:port, protocol) → internet (exit IP), plus the admin
  container, each with a live status.
- **Container status** of `ovpn` and `socks5` (state, start time, restarts).
- **Tunnel health** — one card per [probe](#health-probes) and the overall
  status; the browser can show a notification when it turns `down`.
- **Traffic** — current `tun0` throughput and 1 h / 24 h history (SQLite,
  kept for 7 days).
- **Control** — restart `ovpn` or `socks5`, reconnect OpenVPN (`SIGUSR1`), and
  "Recreate stack", which runs
  `docker compose up -d --force-recreate ovpn socks5` for this project
  (with its `.env` and `docker-compose.override.yml`, if present).
- **Logs** — live logs of `ovpn` and `socks5`.

The panel has no login by default. An optional API token and the other
settings are described in [docs/configuration.md](docs/configuration.md).

## Health probes

All probes connect through the SOCKS5 proxy (`ovpn:1080`), so they test the
same path as your applications.

| Probe         | Interval | OK when |
| ------------- | -------- | ------- |
| `tunnel_ping` | 5 s      | TCP connect to `TUNNEL_TARGET:TUNNEL_PORT` (default `1.1.1.1:443`) succeeds. |
| `dns_resolve` | 10 s     | `DNS_CHECK_URL` returns `200`. |
| `http_check`  | 30 s     | At least one of `HTTP_CHECK_URLS` returns `200`. |
| `egress_ip`   | 60 s     | `EXIT_IP_URL` returns a valid IP and, if `EXPECTED_EXIT_IP` is set, the IP is in that list. |

The overall status is `healthy` when every probe that has run is OK,
`degraded` when some fail, and `down` when all fail, before any probe has
run, or when `egress_ip` reports `LEAK`. `egress_ip` is the only probe that
can tell the tunnel from a leak: without the kill switch the other probes
would stay green if traffic left through the container's own uplink.

By default the probes contact external services: `https://ifconfig.me`
(the exit IP check, once a minute; it sees your VPN exit IP),
`https://example.com`, `http://example.com` and `1.1.1.1:443`. All of them
can be changed, see [docs/configuration.md](docs/configuration.md).

## Kill switch

`ovpn` starts through [ovpn/killswitch-entrypoint.sh](ovpn/killswitch-entrypoint.sh),
which installs an `iptables` chain before OpenVPN starts. Egress from the
shared ovpn/socks5 network namespace is allowed only to loopback, to the VPN
server on the port and protocol of its `remote` lines, out of the tunnel
interface, and as replies to inbound connections. Everything else is dropped;
IPv6 is limited the same way when the namespace has it. If the script cannot parse the config, resolve
the server or install a rule, the container exits and OpenVPN is not started.

Limitations, in short:

- It protects traffic sent through the proxy only; the rest of the host is
  not affected.
- DNS: names are resolved by the resolvers your VPN pushes (`-d`). If the VPN
  pushes none, Docker's embedded DNS is used, and depending on the platform
  its upstream queries may leave from the Docker host outside the tunnel
  (the queried names leak, not the traffic).
- VPN server host names are resolved once at start; restart `ovpn` if the
  server's DNS record changes.

Details, rules and how to verify them: [docs/kill-switch.md](docs/kill-switch.md).

## Security

The stack is designed for a single trusted machine with everything bound to
loopback:

- The SOCKS5 proxy is unauthenticated by default. Enable
  `SOCKS_REQUIRE_AUTH` if other local users exist.
- The admin panel has no authentication by default. It accepts only loopback
  host names (plus `ADMIN_ALLOWED_HOSTS`), requires same-origin requests with
  an `X-Requested-With: vpn-admin` header for every non-GET call, and sends a
  strict Content-Security-Policy. Do not expose it beyond loopback.
- **The Docker socket is mounted into `admin`, which is equivalent to root on
  the host.** Anyone who can use the panel can control Docker.
- Keep your `.ovpn`, `.env` and token files out of version control (they are
  in [.gitignore](.gitignore)).

The threat model and how to report a vulnerability: [SECURITY.md](SECURITY.md).

## Troubleshooting

| Symptom | Likely cause |
| ------- | ------------ |
| `ovpn` exits with `killswitch: ...` | The kill switch could not find or resolve the VPN server, or `iptables` failed. Fix the cause in `docker logs vpn-ovpn`. |
| `curl` through the proxy hangs or fails | Tunnel is down (kill switch drops traffic). Check `docker logs vpn-ovpn` and the panel. |
| `egress_ip` shows `LEAK` | The exit IP is not in `EXPECTED_EXIT_IP`: the tunnel is bypassed, or your provider changed the exit address. |
| Probes fail with a SOCKS5 auth error | The panel has stale or missing credentials; restart the whole stack after changing them. |
| "Recreate stack" fails or returns `503` | `HOST_PROJECT_DIR` is missing, not absolute, or files are not readable by the container. |
| `421 Misdirected Request` | The panel was opened via a non-loopback host name; add it to `ADMIN_ALLOWED_HOSTS`. |

Explanations and fixes: [docs/troubleshooting.md](docs/troubleshooting.md).

## Development

The admin panel is in [admin/](admin) (Go, version in
[admin/go.mod](admin/go.mod)); the kill switch is a POSIX shell script with
its own test harness.

```sh
make test     # go test -race ./... in admin/
make cover    # coverage, fails below 85%
make lint     # golangci-lint run (config: admin/.golangci.yml)
make shell    # kill switch tests (+ shellcheck when installed)
make run      # run the panel locally (DB_PATH=./admin.db)
make help     # all targets: up, down, ps, logs, rebuild, build, fmt, clean, ...
```

CI
([.github/workflows/ci.yml](.github/workflows/ci.yml)) runs vet, race tests,
coverage, golangci-lint, compose validation, the admin image build,
shellcheck and the kill switch tests.

See [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request.
A full-tunnel mode (routing all host traffic, not only proxied applications)
is a possible future direction; it is not implemented, and the files in
[host/](host) are example inputs for it.

## License

[MIT](LICENSE). Third-party components and their licenses are listed in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). The `dperson/openvpn-client`
image is no longer actively updated; both images are pinned by digest in
[docker-compose.yml](docker-compose.yml).
