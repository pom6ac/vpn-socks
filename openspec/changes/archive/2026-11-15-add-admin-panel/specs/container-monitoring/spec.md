## ADDED Requirements

### Requirement: Container status display
The system SHALL display the current status of `ovpn` and `socks5` containers, including state (running / exited / restarting), uptime (time since `StartedAt`), and restart count.

#### Scenario: Both containers running
- **WHEN** both `ovpn` and `socks5` containers are in running state
- **THEN** the admin panel displays both as "running" with green indicator, uptime, and restart count for each

#### Scenario: Container crashed
- **WHEN** the `ovpn` container is in exited state
- **THEN** the admin panel displays `ovpn` as "exited" with red indicator and shows the exit code

#### Scenario: Container restarting
- **WHEN** the `ovpn` container is in restarting state
- **THEN** the admin panel displays `ovpn` as "restarting" with yellow indicator

### Requirement: Live log streaming
The system SHALL stream container logs in real-time via Server-Sent Events (SSE) to the browser, with automatic reconnection on disconnect.

#### Scenario: Viewing live logs
- **WHEN** the user opens the admin panel
- **THEN** logs from both `ovpn` and `socks5` containers are streamed live in the browser via SSE, with newest entries at the bottom

#### Scenario: SSE reconnection
- **WHEN** the SSE connection drops (e.g. admin container restarted)
- **THEN** the browser automatically reconnects and resumes the log stream without manual page reload

#### Scenario: SSE reconnection with log cursor
- **WHEN** the SSE connection drops and reconnects
- **THEN** the browser sends the last received log timestamp as a query parameter, and the server resumes streaming from that point using `docker logs --since <timestamp>`, avoiding replay of the entire log buffer

### Requirement: Log error highlighting
The system SHALL highlight log lines matching known error patterns with a distinct visual style (e.g. red background) to draw attention.

#### Scenario: OpenVPN error in logs
- **WHEN** a log line from `ovpn` contains a known error marker (e.g. "TLS Error", "AUTH_FAILED", "Connection reset", "Inactivity timeout")
- **THEN** that log line is rendered with error highlighting (red background)

#### Scenario: Successful OpenVPN initialization
- **WHEN** a log line contains "Initialization Sequence Completed"
- **THEN** that log line is rendered with info highlighting (green background), distinct from error lines

#### Scenario: SOCKS5 error in logs
- **WHEN** a log line from `socks5` contains "connection refused", "timeout", or "EOF"
- **THEN** that log line is rendered with error highlighting (red background)

### Requirement: Automatic status refresh
The system SHALL poll container status at regular intervals (every 2 seconds) and update the UI without full page reload.

#### Scenario: Status update without reload
- **WHEN** a container transitions from running to exited
- **THEN** the admin panel reflects the new status within 2 seconds without a full page reload

### Requirement: Graceful degradation on Docker API unavailability
The system SHALL continue operating when the Docker API is temporarily unavailable, displaying a degraded status rather than crashing.

#### Scenario: Docker socket unavailable
- **WHEN** the Docker socket (`/var/run/docker.sock`) is temporarily unresponsive or disconnected
- **THEN** the admin panel displays "Docker API unavailable" status for container monitoring and logs, the HTTP server stays alive, and health probes continue working (they do not depend on Docker API)

#### Scenario: Docker socket recovers
- **WHEN** the Docker socket becomes available again after being unavailable
- **THEN** the admin panel automatically resumes container monitoring and log streaming without requiring a manual page reload
