## ADDED Requirements

### Requirement: Real-time traffic display
The system SHALL measure and display traffic throughput (bytes per second, RX and TX) through the `tun` interface, updated every second in the browser. Traffic data SHALL be read by executing `cat /proc/net/dev` inside the `ovpn` container via Docker exec API, parsing the `tun0` interface line.

#### Scenario: Traffic flowing through tunnel
- **WHEN** data is flowing through the `tun` interface
- **THEN** the admin panel displays a real-time line chart showing RX and TX bytes per second, updated every second

#### Scenario: No traffic
- **WHEN** no data is flowing through the `tun` interface
- **THEN** the admin panel displays the chart with values at or near zero

#### Scenario: Docker exec unavailable
- **WHEN** the Docker API is temporarily unavailable and traffic cannot be read from the `ovpn` container
- **THEN** the admin panel displays "traffic data unavailable" and the chart shows a gap, without crashing

### Requirement: Historical metrics storage
The system SHALL persist traffic and health metrics in a SQLite database with a retention period of 7 days. Records older than 7 days SHALL be automatically deleted.

#### Scenario: Metrics persisted
- **WHEN** a traffic or health metric is collected
- **THEN** it is written to the SQLite database with a timestamp

#### Scenario: Old metrics pruned
- **WHEN** metrics older than 7 days exist in the database
- **THEN** they are automatically deleted by a hourly pruning cycle (not on every write, to avoid unnecessary I/O)

### Requirement: Historical traffic charts
The system SHALL display historical traffic charts with selectable time ranges (last hour, last day).

#### Scenario: View last hour
- **WHEN** the user selects "1 hour" time range
- **THEN** the admin panel displays a chart of traffic throughput over the last hour from the SQLite database

#### Scenario: View last day
- **WHEN** the user selects "24 hours" time range
- **THEN** the admin panel displays a chart of traffic throughput over the last 24 hours from the SQLite database

### Requirement: Browser notifications on tunnel failure
The system SHALL send a browser notification when the tunnel transitions from healthy to down state.

#### Scenario: Tunnel goes down
- **WHEN** the tunnel health transitions from "up" to "down"
- **THEN** a browser notification is sent (if permissions granted) alerting the user that the tunnel is down

#### Scenario: Notification permission not granted
- **WHEN** the tunnel goes down and browser notification permissions have not been granted
- **THEN** the admin panel prompts the user to enable notifications
