# Configuration

[Back to README](../README.md)

## Compose variables (`.env`)

`docker compose` reads these from `.env` (template: [.env.example](../.env.example))
and interpolates them into [docker-compose.yml](../docker-compose.yml).

| Variable              | Default         | Description |
| --------------------- | --------------- | ----------- |
| `HOST_PROJECT_DIR`    | — (required)    | Absolute path to this project on the host. Bind-mounted read-only into `admin` **at the same path**, so "Recreate stack" resolves relative paths (`./client.ovpn`, `.env`) exactly as on the host. Compose refuses to start when it is unset. |
| `OVPN_CONFIG`         | `./client.ovpn` | OpenVPN config, mounted into `ovpn` as `/vpn/config.ovpn` (read-only). |
| `SOCKS_REQUIRE_AUTH`  | `false`         | SOCKS5 authentication (`REQUIRE_AUTH` of go-socks5-proxy). |
| `PROXY_USER`          | empty           | SOCKS5 username; passed to `socks5` and to `admin` (for its probes). |
| `PROXY_PASSWORD`      | empty           | SOCKS5 password; passed to `socks5` and to `admin` (for its probes). |
| `EXPECTED_EXIT_IP`    | empty           | Passed to `admin`, see below. |
| `ADMIN_ALLOWED_HOSTS` | empty           | Passed to `admin`, see below. |
| `VPN_ENDPOINT_HOST`   | empty           | Passed to `ovpn`, see [Kill switch variables](#kill-switch-variables-ovpn-container). |

After changing SOCKS credentials or `SOCKS_REQUIRE_AUTH`, restart the whole
stack on the host (`make up` or `docker compose up -d`) — see
[Troubleshooting](troubleshooting.md#changing-the-socks5-password).

## Admin panel variables (`admin` container)

[docker-compose.yml](../docker-compose.yml) sets all of these except
`LISTEN_ADDR`, `PROXY_PASSWORD_FILE` and `ADMIN_TOKEN_FILE`. The defaults
below are the ones built into the binary
([admin/internal/config/config.go](../admin/internal/config/config.go)),
used for example by `make run`.

| Variable               | Default                                   | Description |
| ---------------------- | ----------------------------------------- | ----------- |
| `LISTEN_ADDR`          | `:8080`                                   | HTTP listen address inside the container (published as `127.0.0.1:8090`). |
| `SOCKS5_PROXY`         | `ovpn:1080`                               | SOCKS5 proxy used by the probes and the topology view. `socks5` shares the ovpn network namespace, so it listens on `ovpn:1080` in the compose network. |
| `PROXY_USER`           | empty (no SOCKS5 auth)                    | SOCKS5 username (RFC 1929) used by the panel. |
| `PROXY_PASSWORD`       | empty                                     | SOCKS5 password; ignored when `PROXY_PASSWORD_FILE` is set. |
| `PROXY_PASSWORD_FILE`  | empty                                     | File with the SOCKS5 password (trailing newline trimmed), e.g. a compose secret. Read only when `PROXY_USER` is set; the panel then refuses to start if the file cannot be read. |
| `DB_PATH`              | `/data/admin.db`                          | SQLite database for traffic history and container events (pruned after 7 days). |
| `TUNNEL_TARGET`        | `1.1.1.1`                                 | TCP target of `tunnel_ping`. Must not be the VPN server itself. |
| `TUNNEL_PORT`          | `443`                                     | Port of `TUNNEL_TARGET`. |
| `DNS_CHECK_URL`        | `http://example.com`                      | URL fetched by `dns_resolve`. |
| `HTTP_CHECK_URLS`      | `https://ifconfig.me,https://example.com` | Comma-separated URLs for `http_check`. |
| `EXIT_IP_URL`          | `https://ifconfig.me/ip`                  | Plain-text "what is my IP" service queried by `egress_ip` once a minute; the topology view shows the same result. |
| `EXPECTED_EXIT_IP`     | empty (no comparison)                     | Comma-separated expected exit IPs (IPv4/IPv6, compared as addresses). If the observed exit IP is not in the list, `egress_ip` reports `LEAK` and the overall status is `down`. An invalid entry stops the panel at startup. |
| `OVPN_CONFIG_PATH`     | `/vpn/config.ovpn`                        | Config path inside the ovpn container; read via `docker exec` to show `remote`/`proto`/`port`. |
| `OVPN_CONTAINER`       | `vpn-ovpn`                                | Name of the OpenVPN container. |
| `SOCKS_CONTAINER`      | `vpn-socks`                               | Name of the SOCKS5 container. |
| `ADMIN_CONTAINER`      | `vpn-admin`                               | Name of the admin container (topology view). |
| `COMPOSE_PROJECT_NAME` | `vpn-socks`                               | Compose project (`-p`) used by "Recreate stack". |
| `HOST_PROJECT_DIR`     | empty (recreate returns `503`)            | Must be absolute; `--project-directory` for "Recreate stack". |
| `COMPOSE_FILE`         | `$HOST_PROJECT_DIR/docker-compose.yml` when `HOST_PROJECT_DIR` is set, otherwise empty | Compose file (`-f`) used by "Recreate stack". A single path; `docker-compose.override.yml` next to a `docker-compose.yml` is added automatically. |
| `ADMIN_ALLOWED_HOSTS`  | empty                                     | Extra `Host` names (no port, comma-separated). `localhost`, `127.0.0.1` and `::1` are always allowed; other hosts get `421`. |
| `ADMIN_TOKEN_FILE`     | empty (no token)                          | File with the access token for `/api/*`, see below. |
| `DOCKER_CONFIG`        | set to `/tmp/.docker` in compose          | Read by the Docker CLI, not by the panel: the root filesystem of `admin` is read-only, so CLI state is kept on the `/tmp` tmpfs. |

"Recreate stack" runs

```sh
docker compose --project-directory "$HOST_PROJECT_DIR" -p "$COMPOSE_PROJECT_NAME" \
  -f "$COMPOSE_FILE" [-f "$HOST_PROJECT_DIR/docker-compose.override.yml"] \
  up -d --force-recreate ovpn socks5
```

with a minimal environment (`PATH`, `HOME`, `TMPDIR`, `DOCKER_*`,
`HOST_PROJECT_DIR`), so values are taken from the project's `.env` (read from
`--project-directory`), not from the panel's own environment — the same
values a manual `docker compose up` in the project directory uses. An explicit
`-f` turns off compose's automatic override merge, so when `COMPOSE_FILE` is a
file named `docker-compose.yml` and a regular file
`docker-compose.override.yml` exists next to it, the panel passes it as a
second `-f`; other compose file names are used alone. The override must be
readable by `admin` like the other project files (see
[Troubleshooting](troubleshooting.md#recreate-stack-fails--permission-denied-in-docker-logs-vpn-admin)).
`admin` itself is not recreated.

## Admin panel token (optional)

By default the panel has no authentication (it is published on loopback only).
To require a token for `/api/*`:

```sh
cp docker-compose.override.yml.example docker-compose.override.yml
openssl rand -hex 32 > admin-token
chmod 644 admin-token   # or: sudo chown root admin-token && sudo chmod 600 admin-token
docker compose up -d
```

The override ([docker-compose.override.yml.example](../docker-compose.override.yml.example))
mounts `./admin-token` as the compose secret `/run/secrets/admin_token` and
sets `ADMIN_TOKEN_FILE` to that path. The UI asks for the token on the first
`401`; API clients send `Authorization: Bearer <token>`. If `ADMIN_TOKEN_FILE`
is set but the file is missing, unreadable or empty, the panel refuses to
start. Both `docker-compose.override.yml` and `admin-token` are gitignored.

## Kill switch variables (`ovpn` container)

| Variable            | Default | Description |
| ------------------- | ------- | ----------- |
| `VPN_ENDPOINT_HOST` | empty   | Comma- or space-separated VPN server hosts or IPs that replace the hosts of the config's `remote` lines; ports and protocols still come from the config. |

Set it in `.env`; [docker-compose.yml](../docker-compose.yml) passes it to
the `ovpn` service (empty or unset = the `remote` hosts are used):

```sh
VPN_ENDPOINT_HOST=203.0.113.10
```

Apply it with `docker compose up -d` on the host or "Recreate stack" in the
panel; both read the same `.env`, so the value survives a recreate from the
panel. See [kill-switch.md](kill-switch.md).
