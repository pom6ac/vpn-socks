# Troubleshooting

[Back to README](../README.md)

## `ovpn` exits right after start with `killswitch: ...`

The kill switch could not determine or resolve the VPN server, the config
uses `http-proxy`/`socks-proxy`, or `iptables` failed (the container needs
`NET_ADMIN`, which `docker-compose.yml` grants). It fails closed on purpose;
fix the cause shown in `docker logs vpn-ovpn`. See [kill-switch.md](kill-switch.md).

## Requests through the proxy hang or fail

When the tunnel is down, the kill switch drops proxied traffic. Check
`docker logs vpn-ovpn` for OpenVPN errors (authentication, TLS, unreachable
server) and the "Tunnel health" cards in the panel. "Reconnect OpenVPN" in the
panel sends `SIGUSR1` to the OpenVPN process.

## `egress_ip` shows `LEAK`

Traffic through the proxy left with an IP that is not in `EXPECTED_EXIT_IP`:
either the tunnel is bypassed (check that the kill switch is active:
`docker exec vpn-ovpn iptables -S KILLGUARD`) or your provider changed the
exit address — then add it to the list.

## The panel does not start

- `EXPECTED_EXIT_IP: invalid IP address ...` — fix the entry in `.env`.
- Token configured, but the secret file is missing, empty or not readable
  (see the permission note below).
- `PROXY_PASSWORD_FILE` is set together with `PROXY_USER`, but the file
  cannot be read.

Run `docker logs vpn-admin` for the exact error.

## Health probes fail with a SOCKS5 authentication error

With `SOCKS_REQUIRE_AUTH=true` the panel must know the credentials: compose
passes `PROXY_USER`/`PROXY_PASSWORD` from `.env` to `admin` as well. To keep
the password out of the panel's environment, set `PROXY_PASSWORD_FILE` to a
compose secret in `docker-compose.override.yml` instead.

## Changing the SOCKS5 password

After editing `PROXY_USER`, `PROXY_PASSWORD` or `SOCKS_REQUIRE_AUTH` in
`.env`, restart the whole stack on the host (`make up` or
`docker compose up -d`) so that `admin` also gets the new credentials.
"Recreate stack" in the panel recreates only `ovpn` and `socks5` from the
project `.env` (compose runs with a minimal environment — `PATH`, `HOME`,
`TMPDIR`, `DOCKER_*`, `HOST_PROJECT_DIR` — so the credentials `admin` was
started with never override `.env`). `admin` keeps its old credentials until
it is recreated, and its probes then fail with a SOCKS5 authentication error.

## Probes report the proxy unreachable (`ovpn:1080`)

`admin` must share the compose network with `ovpn` (the project's default
network). If you enabled the image's own firewall (`-f`, `FIREWALL`,
`-r`/`-R`, `ROUTE`/`ROUTE6`), make sure TCP 1080 from the compose network
stays open.

## "Recreate stack" fails / `permission denied` in `docker logs vpn-admin`

`admin` runs as root with `cap_drop: ALL`, so it has no `DAC_OVERRIDE`: it can
read only files that are world-readable or owned by root. On Linux, keep
`docker-compose.yml`, `docker-compose.override.yml` (if any) and `.env`
readable (`chmod 644`) and the project
directory traversable (`chmod 755`). The Docker socket (`root:docker`, mode
`660`) is accessible because the process runs as its owner (uid 0). On
Docker Desktop (macOS/Windows) file access goes through the VM file sharing,
so this usually just works; if it does not, check that the project directory
is shared in Docker Desktop settings.

## "Recreate stack" returns `503`

`HOST_PROJECT_DIR` is empty or not an absolute path inside the container.

## "Recreate stack" ignores my override

"Recreate stack" uses `.env` and `docker-compose.override.yml` from
`HOST_PROJECT_DIR`, like `docker compose up` there, and recreates only `ovpn`
and `socks5`. The override is added only when `COMPOSE_FILE` is a file named
`docker-compose.yml` and the override is a regular file next to it; with
another `COMPOSE_FILE`, merge your settings into that file or recreate from
the host. An override that exists but is not readable by `admin` makes the
recreate fail (see the `permission denied` section above). `admin` itself is rebuilt with
`make rebuild` on the host. For `VPN_ENDPOINT_HOST` no override is needed:
set it in `.env`.

## `421 Misdirected Request`

You opened the panel via a host name that is not loopback; add it to
`ADMIN_ALLOWED_HOSTS`.
