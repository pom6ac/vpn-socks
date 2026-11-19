# Kill switch

[Back to README](../README.md)

`ovpn` starts through [ovpn/killswitch-entrypoint.sh](../ovpn/killswitch-entrypoint.sh)
(the compose `entrypoint`). The script installs an `iptables` chain
(`KILLGUARD`, first in `OUTPUT`) and then execs the image's own entrypoint
(`/sbin/tini -- /usr/bin/openvpn.sh`) with the compose `command` (`-d`).

## What is allowed

Egress of the network namespace shared by OpenVPN and socks5 is allowed only:

- to loopback (`lo`, `127.0.0.0/8`, including Docker's embedded DNS at
  `127.0.0.11`);
- to each VPN server address, **only on the port and protocol of its
  `remote`** (from `remote host port proto`, a `<connection>` block's
  `port`/`proto`, the global `port`/`proto`, or the default `1194/udp`; all
  `remote` lines are used and CRLF configs are accepted);
- out of the tunnel interface (`tun+`, `tap+`);
- as replies to connections opened from outside (`conntrack --ctdir REPLY`),
  which keeps `admin → ovpn:1080` and the `127.0.0.1:1080` publish working.

Everything else is dropped, so when the tunnel is down SOCKS5 traffic fails
instead of leaving with your real IP. IPv6 gets the same chain (`KILLGUARD6`)
when `ip6tables` is usable; it is mandatory (the script fails otherwise) when
the namespace has an IPv6 address outside loopback or a server address is
IPv6. Docker networks have IPv6 disabled by default, in which case the IPv6
rules may be skipped with a warning.

## Fail closed

If the config has no `remote`, a server name cannot be resolved, the config
uses `http-proxy`/`socks-proxy`, a port or protocol is invalid, or any
`iptables` command fails, the script prints `killswitch: <reason>` and the
container exits without starting OpenVPN. On success the log shows
`killswitch: allow ...` lines and `killswitch: active`.

## Environment

| Variable            | Default            | Description |
| ------------------- | ------------------ | ----------- |
| `VPN_CONFIG`        | `/vpn/config.ovpn` | Config the script parses (the compose file mounts `OVPN_CONFIG` there). |
| `VPN_ENDPOINT_HOST` | empty              | Comma- or space-separated hosts or IPs replacing the hosts of the `remote` lines; ports and protocols still come from the config. Set in `.env`, passed by `docker-compose.yml`; see [configuration.md](configuration.md#kill-switch-variables-ovpn-container). |

## Limitations

- **Scope.** Only traffic of the ovpn/socks5 namespace is filtered, i.e.
  traffic sent through the proxy. Applications on the host that do not use the
  proxy are not affected.
- **Server address changes.** Server host names are resolved **once at
  start**, allowed by address and pinned in the container's `/etc/hosts`, so
  OpenVPN reconnects to exactly those addresses without DNS outside the
  tunnel. If the server's DNS record changes, restart `ovpn` ("Recreate stack"
  or `docker compose restart ovpn`).
- **Server ports.** Direct traffic to the VPN server is allowed on its
  OpenVPN port/protocol only; any other port on that host is dropped.
- **DNS.** The compose file passes `-d` to the image: OpenVPN runs the image's
  `up`/`down` scripts, which write the DNS servers pushed by the VPN
  (`dhcp-option DNS`) into `/etc/resolv.conf`. Queries then go to the VPN's
  resolvers through `tun0` and are dropped while the tunnel is down. If your
  VPN pushes no DNS servers, `/etc/resolv.conf` keeps Docker's embedded DNS
  (`127.0.0.11`), which the kill switch allows. The embedded DNS forwards
  external names to the Docker daemon's upstream resolvers; whether that query
  leaves through the container namespace (then it goes through the tunnel or
  is dropped) or from the daemon/host side (then the *names* you resolve are
  visible to the host's resolver — a DNS leak, not a traffic leak) depends on
  the Docker version and platform.
- **Inbound.** The kill switch filters egress only; inbound exposure is
  governed by the compose publish (`127.0.0.1:1080`) and the compose network.
- **Image firewall.** The `dperson/openvpn-client` image installs its own
  DROP firewall only with `-f`, `FIREWALL`, `-r`/`-R` or `ROUTE`/`ROUTE6`;
  this compose file uses none of them. If you enable it, keep TCP 1080 from
  the compose network open, or the panel's probes cannot reach the proxy.

## Verifying

```sh
docker logs vpn-ovpn 2>&1 | grep killswitch      # allow ... / active
docker exec vpn-ovpn iptables -S KILLGUARD        # installed rules
docker exec vpn-ovpn cat /etc/resolv.conf         # DNS servers in use
```

To see it work, stop OpenVPN inside the container (for example
`docker exec vpn-ovpn pkill openvpn`; the container may restart) and run
`curl --socks5-hostname 127.0.0.1:1080 https://ifconfig.me/ip`: it should
fail or time out instead of printing your real IP.

The script has a test harness with stubbed `iptables`/resolvers:
`make shell` (runs [ovpn/killswitch_test.sh](../ovpn/killswitch_test.sh)).
