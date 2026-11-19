> **Archived change (historical record).** In the current public version
> htmx is not used, `/api/rebuild` and the Full Rebuild button are removed
> (use `make rebuild` on the host), the project is mounted read-only at
> `HOST_PROJECT_DIR` instead of `/compose`, and compose output is written only
> to the admin log, not returned to the client. See `README.md`.

## 1. Project scaffold & Docker setup

> **Dependencies:** None — start here.

- [x] 1.1 Create `admin/` directory with Go module (`go mod init github.com/pom6ac/vpn-socks/admin`)
- [x] 1.2 Add Go dependencies: `docker/docker/client`, `modernc.org/sqlite`, `chi` (or use stdlib `net/http`)
- [x] 1.3 Create `admin/Dockerfile` (multi-stage: `golang:alpine` build → `docker:cli` runtime). Verify `docker:cli` image includes compose v2 plugin; if not, install `docker-compose-plugin` in Dockerfile
- [x] 1.4 Add `admin` service to `docker-compose.yml` (bridge net, port `127.0.0.1:8080:8080`, docker socket mount, compose.yml mount, `extra_hosts: host.docker.internal:host-gateway`, `admin-data` volume, env vars)
- [x] 1.5 Verify `docker compose up` starts all three containers; admin reachable at `localhost:8080`

## 2. Web UI foundation (htmx + SSE + embed)

> **Dependencies:** Task 1 (scaffold & Docker) must be complete.

- [x] 2.1 Create `admin/web/` with `index.html` (base layout: header, container status panel, log panel, health panel, traffic chart panel, control buttons)
- [x] 2.2 Create `admin/web/style.css` (dark theme, status indicators green/yellow/red, log line highlighting styles)
- [x] 2.3 Vendor `htmx.min.js` and `chart.min.js` (Chart.js) into `admin/web/`
- [x] 2.4 Create `admin/web/app.js` (SSE EventSource setup, auto-reconnect with log cursor, Chart.js initialization for traffic chart)
- [x] 2.5 Implement `embed.FS` in Go to serve static files (`index.html`, `style.css`, `app.js`, `htmx.min.js`, `chart.min.js`)
- [x] 2.6 Implement HTTP routing (`/`, `/static/*`, `/api/*`, `/healthz`) and start server on `:8080`. `/healthz` returns 200 for Docker healthcheck.

## 3. Container monitoring (read-only, iteration 1)

> **Dependencies:** Tasks 1 (scaffold) and 2 (UI foundation) must be complete.

- [x] 3.1 Implement Docker client wrapper: connect to `/var/run/docker.sock`, list/inspect containers by name (`vpn-ovpn`, `vpn-socks`). Wrap all Docker API calls in retry-with-backoff to handle temporary socket unavailability gracefully.
- [x] 3.2 Implement `/api/status` endpoint: return JSON array of containers with state, `StartedAt` (uptime), `RestartCount`, exit code. On Docker API error, return `"docker_api_unavailable": true` instead of crashing.
- [x] 3.3 Implement status poller goroutine: inspect containers every 2 seconds, emit status via SSE or htmx polling
- [x] 3.4 Implement SSE log streaming: `/api/logs/{container}?since=<timestamp>` — attach to container logs via Docker API, stream new lines as SSE events. Support `since` query param to resume from last timestamp on reconnect (avoids replaying entire log buffer).
- [x] 3.5 Implement log error highlighting: regex matcher for OpenVPN markers ("TLS Error", "AUTH_FAILED", "Connection reset", "Inactivity timeout", "Initialization Sequence Completed") and SOCKS5 markers ("connection refused", "timeout", "EOF"); annotate SSE events with severity level
- [x] 3.6 Wire up UI: container status cards with indicators, log panel with auto-scroll and color-coded lines, htmx/SSE integration. On Docker API unavailable, show "Docker API unavailable" banner.

## 4. Tunnel health probes (iteration 1)

> **Dependencies:** Tasks 1 (scaffold) and 2 (UI foundation) must be complete. Independent of Task 3.

- [x] 4.1 Implement SOCKS5 dialer helper: connect through `host.docker.internal:1080` (env `SOCKS5_PROXY`) using Go SOCKS5 client
- [x] 4.2 Implement tunnel ping probe: TCP connect to VPN server IP (env `TUNNEL_TARGET`, default `203.0.113.10`) through SOCKS5, measure latency, timeout 5s, run every 5s
- [x] 4.3 Implement DNS resolve probe: HTTP GET to a domain (env `DNS_CHECK_URL`, default `http://example.com`) through SOCKS5, timeout 10s, run every 10s
- [x] 4.4 Implement HTTP availability probe: HTTP GET to reference URLs (env `HTTP_CHECK_URLS`, comma-separated, default `https://ifconfig.me,https://example.com`) through SOCKS5, check status 200, timeout 10s, run every 30s
- [x] 4.5 Implement `/api/health` endpoint: return JSON with all probe results (tunnel, dns, http) and aggregated overall status (healthy/degraded/down per D5 aggregation rules)
- [x] 4.6 Wire up UI: health panel with individual probe indicators and overall health badge (green/yellow/red)

## 5. Traffic metrics (iteration 1 — real-time)

> **Dependencies:** Tasks 1 (scaffold) and 2 (UI foundation) must be complete. Independent of Tasks 3 and 4.

- [x] 5.1 Implement `tun0` interface traffic reader: use Docker exec API to run `cat /proc/net/dev` inside `ovpn` container, parse `tun0` line for RX/TX bytes, compute bytes/sec delta. Handle Docker exec unavailability gracefully (show "traffic data unavailable").
- [x] 5.2 Implement `/api/metrics/traffic` endpoint: return current RX/TX bytes per second as JSON
- [x] 5.3 Wire up UI: real-time line chart using Chart.js, updated every second via SSE or polling, showing RX and TX lines

## 6. Stack control — рычаги (iteration 2)

> **Dependencies:** Tasks 1 (scaffold) and 2 (UI foundation) must be complete. Task 3 (Docker client wrapper) is required for restart/reconnect.

- [x] 6.1 Implement `/api/restart/{container}` endpoint (POST): restart container via Docker API `ContainerRestart`, return success/failure
- [x] 6.2 Implement `/api/reconnect` endpoint (POST): send SIGUSR1 to `ovpn` container via Docker API `ContainerKill(signal=SIGUSR1)`, return success/failure
- [x] 6.3 Implement `/api/recreate` endpoint (POST): run `docker compose up -d --force-recreate ovpn socks5` via `os/exec` using mounted compose file (`COMPOSE_FILE` env). Capture stdout/stderr and return in response. Return success/failure with compose output.
- [x] 6.4 Implement `/api/rebuild` endpoint (POST): run `docker compose down && docker compose build --no-cache && docker compose up -d` via `os/exec` with `syscall.SysProcAttr{Setsid: true}` (detached session survives admin SIGTERM). Write compose stdout/stderr to admin stdout (visible in `docker logs vpn-admin` after recovery). Return HTTP 202 Accepted immediately. *(В публичной версии эндпоинт `/api/rebuild` удалён; пересборка выполняется командой `make rebuild` на хосте.)*
- [x] 6.5 Wire up UI: control buttons (Restart ovpn, Restart socks5, Reconnect OpenVPN, Recreate Stack, Full Rebuild), confirmation dialog for Full Rebuild
- [x] 6.6 Implement action feedback: loading spinner on button during action, success/error toast notification on completion (with compose stdout/stderr for recreate), disable button during action

## 7. SQLite metrics storage & historical charts (iteration 3)

> **Dependencies:** Tasks 4 (health probes) and 5 (traffic metrics) must be complete.

- [x] 7.1 Implement SQLite schema: `metrics` (id, ts, kind, value) and `container_events` (id, ts, container, event) tables
- [x] 7.2 Implement metrics writer goroutine: write traffic and health probe results to SQLite every cycle. Run pruning (`DELETE WHERE ts < datetime('now', '-7 days')`) once per hour, not on every write.
- [x] 7.3 Implement `/api/metrics/history` endpoint: return time-series data for a given metric kind and time range (1h / 24h) from SQLite
- [x] 7.4 Wire up UI: time range selector (1h / 24h) on traffic chart, fetch historical data and render on canvas
- [x] 7.5 Implement container event logging: subscribe to Docker events stream, record start/die/restart events to `container_events` table

## 8. Browser notifications (iteration 3)

> **Dependencies:** Tasks 4 (health probes) must be complete for tunnel-down detection.

- [x] 8.1 Implement notification permission request flow in `app.js` (request on first user interaction)
- [x] 8.2 Implement tunnel-down notification: when tunnel health transitions from up → down, send browser notification
- [x] 8.3 Implement notification prompt fallback: if permissions not granted, show in-panel banner prompting to enable notifications

## 9. Admin logging & observability

> **Dependencies:** Task 2 (HTTP routing) must be complete.

- [x] 9.1 Implement structured logging in admin: all significant events (startup, Docker API errors, compose command output, probe transitions) logged to stdout via `log.Printf`, visible in `docker logs vpn-admin`
- [x] 9.2 Implement Docker API retry-with-backoff wrapper: all Docker API calls wrapped in retry logic (3 attempts, exponential backoff) to handle temporary socket unavailability without crashing

## 10. Polish & verification

> **Dependencies:** All previous task groups.

- [ ] 10.1 Test full flow: start stack, verify status/logs/health/traffic display correctly — требует ручной проверки в окружении Docker
- [ ] 10.2 Test control actions: restart ovpn, restart socks5, reconnect OpenVPN, recreate stack, full rebuild — verify each works and UI feedback is correct — требует ручной проверки в окружении Docker
- [ ] 10.3 Test failure scenarios: stop ovpn container, verify health goes red and notification fires; verify admin stays online during ovpn restart — требует ручной проверки в окружении Docker
- [ ] 10.4 Test Docker API unavailability: stop docker socket, verify admin shows "Docker API unavailable" without crashing; restore socket, verify auto-recovery — требует ручной проверки в окружении Docker
- [ ] 10.5 Test SQLite persistence: restart admin container, verify historical data survives — требует ручной проверки в окружении Docker
- [ ] 10.6 Verify `admin-data` volume persists SQLite across `docker compose down && up` — требует ручной проверки в окружении Docker
- [ ] 10.7 Verify `/healthz` endpoint and Docker healthcheck work correctly — требует ручной проверки в окружении Docker
- [x] 10.8 Update `openspec/config.yaml` plan section to reflect implemented capabilities
