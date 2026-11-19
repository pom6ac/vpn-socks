> **Archived change (historical record).** In the current public version
> htmx is not used, `/api/rebuild` and the Full Rebuild button are removed
> (use `make rebuild` on the host), the project is mounted read-only at
> `HOST_PROJECT_DIR` instead of `/compose`, and compose output is written only
> to the admin log, not returned to the client. See `README.md`.

## ADDED Requirements

### Requirement: Restart individual container
The system SHALL allow the user to restart an individual container (`ovpn` or `socks5`) via a button in the admin panel, using the Docker API.

#### Scenario: Restart ovpn
- **WHEN** the user clicks "Restart" on the `ovpn` container
- **THEN** the `ovpn` container is restarted via Docker API and the admin panel reflects the restart (uptime resets, status transitions through restarting → running)

#### Scenario: Restart socks5
- **WHEN** the user clicks "Restart" on the `socks5` container
- **THEN** the `socks5` container is restarted via Docker API and the admin panel reflects the restart

### Requirement: Reconnect OpenVPN
The system SHALL allow the user to trigger an OpenVPN reconnect by sending SIGUSR1 to the `ovpn` container, without fully restarting the container.

#### Scenario: Reconnect OpenVPN session
- **WHEN** the user clicks "Reconnect OpenVPN"
- **THEN** a SIGUSR1 signal is sent to the `ovpn` container via Docker API, causing OpenVPN to reconnect without container restart, and the admin panel continues showing logs (container stays running)

### Requirement: Recreate stack without admin
The system SHALL allow the user to recreate the `ovpn` and `socks5` containers without affecting the `admin` container, using `docker compose up -d --force-recreate ovpn socks5`. The stdout/stderr of the compose command SHALL be captured and returned in the HTTP response for diagnostics.

#### Scenario: Recreate ovpn and socks5
- **WHEN** the user clicks "Recreate Stack"
- **THEN** the `ovpn` and `socks5` containers are force-recreated via `docker compose up -d --force-recreate ovpn socks5` and the admin panel stays online throughout the operation, showing the restart process in logs

#### Scenario: Recreate fails with error output
- **WHEN** the `docker compose up --force-recreate` command fails
- **THEN** the stdout/stderr of the failed command is displayed to the user in the error message

### Requirement: Full rebuild with no-cache
The system SHALL allow the user to trigger a full stack rebuild including `admin` using `docker compose down && docker compose build --no-cache && docker compose up -d`. The command SHALL be executed in a new session (via `syscall.SysProcAttr{Setsid: true}`) so that it survives the termination of the admin container.

#### Scenario: Full rebuild
- **WHEN** the user clicks "Full Rebuild (no-cache)"
- **THEN** a confirmation dialog appears warning that the admin panel will go offline during rebuild
- **WHEN** the user confirms
- **THEN** `docker compose down && docker compose build --no-cache && docker compose up -d` is executed in a detached session, the admin panel goes offline, and the browser auto-reconnects once the admin container is back up

#### Scenario: Rebuild command survives admin termination
- **WHEN** `docker compose down` sends SIGTERM to the admin container
- **THEN** the compose command continues executing in its new session (build --no-cache && up -d) because it was started with `Setsid: true`, and the admin container is recreated as part of `docker compose up -d`

### Requirement: Action feedback
The system SHALL provide visual feedback for control actions — showing in-progress state while the action runs and success/failure result upon completion.

#### Scenario: Action in progress
- **WHEN** a control action (restart, reconnect, recreate, rebuild) is in progress
- **THEN** the corresponding button shows a loading spinner and is disabled to prevent duplicate clicks

#### Scenario: Action succeeds
- **WHEN** a control action completes successfully
- **THEN** the button returns to normal state and a success indicator is briefly shown

#### Scenario: Action fails
- **WHEN** a control action fails (e.g. Docker API error)
- **THEN** the button returns to normal state and an error message is displayed to the user
