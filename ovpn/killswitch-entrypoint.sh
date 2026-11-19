#!/bin/sh
# Kill switch for the ovpn container's network namespace.
#
# Everything leaving the namespace is dropped unless it goes
#   - to loopback (including Docker's embedded DNS at 127.0.0.11),
#   - to a VPN server, and only to the port/protocol of its `remote` line,
#   - through the tunnel interface (tun+/tap+),
#   - or is a reply to a connection opened from outside (conntrack REPLY:
#     SOCKS5 clients via the compose network or the 127.0.0.1:1080 publish).
# SOCKS5 traffic therefore never leaves through eth0 with the host's real IP
# when the tunnel is down. IPv6 egress is limited the same way.
#
# VPN server host names are resolved once here, allowed by address and pinned
# in /etc/hosts so that OpenVPN reconnects to exactly the allowed addresses
# without needing DNS outside the tunnel. A changed server address therefore
# needs a container restart.
#
# Fail closed: any error (unparsable config, unresolvable server, iptables
# failure) exits non-zero and OpenVPN is not started.
#
# Environment:
#   VPN_CONFIG         OpenVPN config (default /vpn/config.ovpn)
#   VPN_ENDPOINT_HOST  optional list (comma/space separated) of server hosts or
#                      IPs replacing the hosts of the `remote` lines; ports and
#                      protocols still come from the config.
#
# Finally execs the image's own entrypoint (tini -> openvpn.sh) with the
# container command (e.g. -d).
set -eu

VPN_CONFIG="${VPN_CONFIG:-/vpn/config.ovpn}"
VPN_ENDPOINT_HOST="${VPN_ENDPOINT_HOST:-}"
HOSTS_MARK="# vpn-killswitch"

die() {
    echo "killswitch: $*" >&2
    exit 1
}

[ -r "$VPN_CONFIG" ] || die "cannot read $VPN_CONFIG"

# Connecting through a proxy would need egress to the proxy, not to the
# remotes; this kill switch does not support it.
if tr -d '\r' <"$VPN_CONFIG" | grep -Eq '^[[:space:]]*(http-proxy|socks-proxy)[[:space:]]'; then
    die "http-proxy/socks-proxy in $VPN_CONFIG is not supported"
fi

# Print "host port proto" for every `remote` (also inside <connection>
# blocks). A remote without port/proto takes the <connection> block's
# port/proto, then the global ones, then 1194/udp. The last line is
# "* port proto" with the global defaults. CRLF configs are accepted.
remotes=$(tr -d '\r' <"$VPN_CONFIG" | awk '
    { sub(/^[ \t]+/, "") }
    /^[#;]/ { next }
    $1 == "<connection>" { inconn = 1; first = n + 1; cport = ""; cproto = ""; next }
    $1 == "</connection>" {
        for (i = first; i <= n; i++) {
            if (P[i] == "") P[i] = cport
            if (R[i] == "") R[i] = cproto
        }
        inconn = 0
        next
    }
    $1 == "remote" && NF >= 2 { n++; H[n] = $2; P[n] = $3; R[n] = $4; next }
    ($1 == "port" || $1 == "rport") && NF >= 2 { if (inconn) cport = $2; else gport = $2; next }
    $1 == "proto" && NF >= 2 { if (inconn) cproto = $2; else gproto = $2; next }
    END {
        if (gport == "") gport = "1194"
        if (gproto == "") gproto = "udp"
        for (i = 1; i <= n; i++)
            print H[i], (P[i] != "" ? P[i] : gport), (R[i] != "" ? R[i] : gproto)
        print "*", gport, gproto
    }')

# normalize_proto maps tcp-client/tcp4/udp6/... to tcp or udp.
normalize_proto() {
    case "$1" in
        tcp*) echo tcp ;;
        udp*) echo udp ;;
        *) die "unsupported proto '$1'" ;;
    esac
}

# Resulting "host port proto" endpoints, one per line.
endpoints=$(echo "$remotes" | awk '$1 != "*"')
if [ -n "$VPN_ENDPOINT_HOST" ]; then
    # Overridden hosts get every port/proto pair of the config's remotes, or
    # the global defaults when there are none.
    pairs=$(echo "$remotes" | awk '$1 != "*" { print $2, $3 }' | sort -u)
    [ -n "$pairs" ] || pairs=$(echo "$remotes" | awk '$1 == "*" { print $2, $3 }')
    endpoints=$(for h in $(echo "$VPN_ENDPOINT_HOST" | tr ',' ' '); do
        echo "$pairs" | while read -r port proto; do echo "$h $port $proto"; done
    done)
fi
[ -n "$endpoints" ] || die "no VPN endpoint: no 'remote' in $VPN_CONFIG and VPN_ENDPOINT_HOST is empty"

is_ipv4() {
    echo "$1" | grep -Eq '^([0-9]{1,3}\.){3}[0-9]{1,3}$'
}

is_ipv6() {
    case "$1" in
        *:*) echo "$1" | grep -Eq '^[0-9A-Fa-f:.]+$' ;;
        *) return 1 ;;
    esac
}

# resolve prints every IPv4/IPv6 address of a host name. The image is Alpine:
# musl's getent (musl-utils) and busybox nslookup are both tried.
resolve() {
    {
        if command -v getent >/dev/null 2>&1; then
            getent hosts "$1" 2>/dev/null | awk '{ print $1 }' || true
        fi
        if command -v nslookup >/dev/null 2>&1; then
            nslookup "$1" 2>/dev/null | awk '/^Name:/ { ans = 1; next } ans && /^Address/ { print $NF }' || true
        fi
    } | while read -r a; do
        if is_ipv4 "$a" || is_ipv6 "$a"; then echo "$a"; fi
    done | sort -u
}

# Build "addr port proto" rules and "addr host" pins.
rules=""
pins=""
nl='
'
while read -r host port proto; do
    [ -n "$host" ] || continue
    case "$port" in
        '' | *[!0-9]*) die "invalid port '$port' for $host" ;;
    esac
    proto=$(normalize_proto "$proto")
    if is_ipv4 "$host" || is_ipv6 "$host"; then
        addrs="$host"
    else
        addrs=$(resolve "$host")
        [ -n "$addrs" ] || die "cannot resolve VPN server $host"
        for a in $addrs; do pins="$pins$a $host$nl"; done
    fi
    for a in $addrs; do rules="$rules$a $port $proto$nl"; done
done <<EOF
$endpoints
EOF
rules=$(printf '%s' "$rules" | sort -u)
[ -n "$rules" ] || die "no VPN endpoint address"

# Pin resolved server names. /etc/hosts is a Docker bind mount: rewrite it in
# place (no rename) and drop pins from a previous start first.
if [ -n "$pins" ]; then
    kept=$(grep -v "$HOSTS_MARK\$" /etc/hosts || true)
    {
        printf '%s\n' "$kept"
        printf '%s' "$pins" | sort -u | sed "s/\$/ $HOSTS_MARK/"
    } >/etc/hosts.killswitch
    cat /etc/hosts.killswitch >/etc/hosts
    rm -f /etc/hosts.killswitch
fi

# ipt runs one iptables/ip6tables command and aborts on failure. Every rule is
# checked explicitly: set -e does not apply inside functions called from a
# condition, and a partially installed chain must never let OpenVPN start.
ipt() {
    "$@" || die "failed: $*"
}

# setup_chain <iptables|ip6tables> <chain> <4|6>: installs the chain and hooks
# it first into OUTPUT.
setup_chain() {
    cmd=$1
    chain=$2
    family=$3
    "$cmd" -N "$chain" 2>/dev/null || true
    ipt "$cmd" -F "$chain"
    ipt "$cmd" -A "$chain" -o lo -j ACCEPT
    if [ "$family" = 4 ]; then
        # Docker's embedded DNS (127.0.0.11) is reached through DNAT rules and
        # may leave via eth0 with a loopback destination (route_localnet).
        ipt "$cmd" -A "$chain" -d 127.0.0.0/8 -j ACCEPT
    fi
    while read -r addr port proto; do
        [ -n "$addr" ] || continue
        case "$family:$addr" in
            4:*:*) continue ;;
            6:*:*) ;;
            6:*) continue ;;
        esac
        echo "killswitch: allow $proto $addr:$port (IPv$family)"
        ipt "$cmd" -A "$chain" -d "$addr" -p "$proto" --dport "$port" -j ACCEPT
    done <<RULES
$rules
RULES
    # Replies to connections opened from outside (SOCKS5 clients). A leak
    # always starts with a packet in the ORIGINAL direction, which is not
    # matched here.
    ipt "$cmd" -A "$chain" -m conntrack --ctdir REPLY -j ACCEPT
    ipt "$cmd" -A "$chain" -o tun+ -j ACCEPT
    ipt "$cmd" -A "$chain" -o tap+ -j ACCEPT
    ipt "$cmd" -A "$chain" -j DROP
    if ! "$cmd" -C OUTPUT -j "$chain" 2>/dev/null; then
        ipt "$cmd" -I OUTPUT 1 -j "$chain"
    fi
}

setup_chain iptables KILLGUARD 4

# IPv6: mandatory when the namespace has a non-loopback IPv6 address or an
# IPv6 server address is configured; skipped otherwise when ip6tables is not
# usable (Docker networks have IPv6 disabled by default).
v6_needed=false
if [ -r /proc/net/if_inet6 ] && awk '$6 != "lo" { found = 1 } END { exit !found }' /proc/net/if_inet6; then
    v6_needed=true
fi
case "$rules" in
    *:*) v6_needed=true ;;
esac
if command -v ip6tables >/dev/null 2>&1 && ip6tables -S OUTPUT >/dev/null 2>&1; then
    setup_chain ip6tables KILLGUARD6 6
elif [ "$v6_needed" = true ]; then
    die "ip6tables is not usable while IPv6 is enabled"
else
    echo "killswitch: IPv6 rules not installed (no IPv6 outside loopback)" >&2
fi

echo "killswitch: active"
exec /sbin/tini -- /usr/bin/openvpn.sh "$@"
