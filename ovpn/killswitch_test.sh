#!/bin/sh
# Tests for killswitch-entrypoint.sh. The script runs against stub iptables,
# ip6tables, getent and nslookup commands and a temporary hosts file; the
# final exec is replaced by an echo. Usage: sh ovpn/killswitch_test.sh
set -eu

here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/killswitch-test.XXXXXX")
trap 'rm -rf "$work"' EXIT

mkdir "$work/bin"
cat >"$work/bin/iptables" <<'EOF'
#!/bin/sh
echo "iptables $*" >>"$KS_LOG"
case "$*" in
    "-C OUTPUT"*) exit 1 ;;
    *"--dport 666"*) exit 3 ;;
esac
exit 0
EOF
sed 's/^echo "iptables/echo "ip6tables/' "$work/bin/iptables" >"$work/bin/ip6tables"
cat >"$work/bin/getent" <<'EOF'
#!/bin/sh
case "$2" in
    vpn.example.com) printf '203.0.113.10    vpn.example.com\n203.0.113.11    vpn.example.com\n' ;;
    v6.example.com) printf '2001:db8::10    v6.example.com\n' ;;
    *) exit 2 ;;
esac
EOF
cat >"$work/bin/nslookup" <<'EOF'
#!/bin/sh
exit 1
EOF
chmod +x "$work/bin/"*

sed -e "s|/etc/hosts|$work/hosts|g" \
    -e 's|exec /sbin/tini -- /usr/bin/openvpn.sh|exec echo EXEC|' \
    "$here/killswitch-entrypoint.sh" >"$work/ks.sh"

fails=0
fail() {
    echo "FAIL: $*" >&2
    fails=$((fails + 1))
}

# run <config> [env...]: runs the script, leaving output in $out, the iptables
# log in $log and the exit status in $rc.
run() {
    printf '%b' "$1" >"$work/config.ovpn"
    shift
    printf '127.0.0.1 localhost\n10.0.0.2 old # vpn-killswitch\n' >"$work/hosts"
    : >"$work/log"
    rc=0
    out=$(env PATH="$work/bin:$PATH" KS_LOG="$work/log" VPN_CONFIG="$work/config.ovpn" \
        VPN_ENDPOINT_HOST= "$@" sh "$work/ks.sh" -d 2>&1) || rc=$?
    log=$(cat "$work/log")
}

has() {
    case "$2" in
        *"$3"*) ;;
        *) fail "$1: missing '$3'" ;;
    esac
}

hasnt() {
    case "$2" in
        *"$3"*) fail "$1: unexpected '$3'" ;;
    esac
}

# CRLF config, global proto/port, explicit remote fields, <connection> block,
# host name resolved to two addresses.
run 'client\r\nproto tcp-client\r\nport 443\r\nremote vpn.example.com\r\nremote 198.51.100.5 1194 udp4\r\n<connection>\r\nremote 198.51.100.6\r\nport 8443\r\n</connection>\r\n<key>\r\nremote 192.0.2.99\r\n</key>\r\n'
[ "$rc" = 0 ] || fail "basic: rc=$rc: $out"
has basic "$log" "iptables -A KILLGUARD -d 203.0.113.10 -p tcp --dport 443 -j ACCEPT"
has basic "$log" "iptables -A KILLGUARD -d 203.0.113.11 -p tcp --dport 443 -j ACCEPT"
has basic "$log" "iptables -A KILLGUARD -d 198.51.100.5 -p udp --dport 1194 -j ACCEPT"
has basic "$log" "iptables -A KILLGUARD -d 198.51.100.6 -p tcp --dport 8443 -j ACCEPT"
has basic "$log" "iptables -A KILLGUARD -m conntrack --ctdir REPLY -j ACCEPT"
has basic "$log" "iptables -A KILLGUARD -o tun+ -j ACCEPT"
has basic "$log" "iptables -A KILLGUARD -j DROP"
has basic "$log" "iptables -I OUTPUT 1 -j KILLGUARD"
hasnt basic "$log" "-d 203.0.113.10 -j ACCEPT"
has basic "$out" "EXEC -d"
hosts=$(cat "$work/hosts")
has basic "$hosts" "203.0.113.10 vpn.example.com # vpn-killswitch"
has basic "$hosts" "127.0.0.1 localhost"
hasnt basic "$hosts" "10.0.0.2 old"
# The DROP rule must come last in the IPv4 chain.
last=$(echo "$log" | grep '^iptables -A KILLGUARD' | tail -n 1)
[ "$last" = "iptables -A KILLGUARD -j DROP" ] || fail "basic: last rule is '$last'"

# Defaults 1194/udp.
run 'remote 198.51.100.7\n'
[ "$rc" = 0 ] || fail "defaults: rc=$rc"
has defaults "$log" "-d 198.51.100.7 -p udp --dport 1194 -j ACCEPT"

# VPN_ENDPOINT_HOST overrides hosts, keeps ports/protocols.
run 'proto tcp\nremote ignored.example.com 443\n' VPN_ENDPOINT_HOST="198.51.100.8, 198.51.100.9"
[ "$rc" = 0 ] || fail "override: rc=$rc: $out"
has override "$log" "-d 198.51.100.8 -p tcp --dport 443 -j ACCEPT"
has override "$log" "-d 198.51.100.9 -p tcp --dport 443 -j ACCEPT"
hasnt override "$log" "ignored"

# IPv6 server address goes to ip6tables only.
run 'remote v6.example.com 1194 udp6\n'
[ "$rc" = 0 ] || fail "ipv6: rc=$rc: $out"
has ipv6 "$log" "ip6tables -A KILLGUARD6 -d 2001:db8::10 -p udp --dport 1194 -j ACCEPT"
hasnt ipv6 "$log" "iptables -A KILLGUARD -d 2001:db8::10"

# Fail closed.
run 'client\n'
[ "$rc" != 0 ] || fail "no remote must fail"
has no-remote "$out" "no VPN endpoint"
hasnt no-remote "$out" "EXEC"

run 'remote unknown.example.com\n'
[ "$rc" != 0 ] || fail "unresolvable remote must fail"
hasnt unresolvable "$out" "EXEC"

run 'remote 198.51.100.5 666\n'
[ "$rc" != 0 ] || fail "iptables failure must fail"
hasnt iptables-failure "$out" "EXEC"

run 'remote 198.51.100.5 1194 icmp\n'
[ "$rc" != 0 ] || fail "bad proto must fail"
hasnt bad-proto "$out" "EXEC"

run 'remote 198.51.100.5\nhttp-proxy 192.0.2.1 3128\n'
[ "$rc" != 0 ] || fail "http-proxy must fail"
hasnt http-proxy "$out" "EXEC"

if [ "$fails" -ne 0 ]; then
    echo "$fails killswitch test(s) failed" >&2
    exit 1
fi
echo "killswitch tests: ok"
