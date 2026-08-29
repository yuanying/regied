#!/usr/bin/env bash
#
# The reference implementation of the device under test. Out of hand-written ip / nft
# it assembles, inside the router netns, a router that satisfies the seven checks the
# testbed makes.
#
# This is not part of the testbed; it is the replaceable side. Point
# REGIED_NETNS_ROUTER_SETUP at any script that honours the same contract (the handover
# described in ADR 0010) and the same tests can be run against an existing
# implementation or against one of our own.
#
# Usage: reference.sh up | down
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "${here}/../lib.sh"

# The PPPoE link. Unless its name is pinned, it competes for numbers with the pppd on
# the provider side and ends up as ppp0 one time and ppp1 the next.
PPP_IF="ppp-wan"
PPP_LINKNAME="regied-netns"
PPP_OPTIONS="${RUNTIME_DIR}/router-pppd-options"
PPP_PID_FILE="/var/run/ppp-${PPP_LINKNAME}.pid"

# The DS-Lite tunnel.
DSLITE_IF="dslite0"

# The routing tables. Policy routing picks between them per source range.
TABLE_PPPOE=1
TABLE_DSLITE=2
MARK_PPPOE=1
MARK_DSLITE=2

r() { nse "${NS_ROUTER}" "$@"; }

setup_sysctls() {
  ns_sysctl "${NS_ROUTER}" \
    net.ipv4.ip_forward=1 \
    net.ipv6.conf.all.forwarding=1 \
    net.ipv4.conf.all.rp_filter=0 \
    net.ipv4.conf.default.rp_filter=0 \
    net.ipv6.conf.all.accept_dad=0 \
    net.ipv6.conf.default.accept_dad=0
}

setup_lan() {
  r ip addr add "${ROUTER_LAN_IP}/${LAN_PREFIXLEN}" dev "${ROUTER_LAN_IF}"
  r ip link set "${ROUTER_LAN_IF}" up
}

# The WAN-side IPv6. On a real line this comes from RA and DHCPv6-PD; in the testbed it
# is placed statically. DS-Lite rides on top of this IPv6 reachability.
setup_wan_v6() {
  r ip -6 addr add "${WAN_V6_ROUTER}/${WAN_V6_PREFIXLEN}" dev "${ROUTER_WAN_IF}"
  r ip link set "${ROUTER_WAN_IF}" up
  r ip -6 route replace default via "${WAN_V6_AFTR}" dev "${ROUTER_WAN_IF}"
}

# PPPoE. Credentials are read from files rather than written into the configuration.
start_pppoe() {
  local user password
  user="$(cat "${PPPOE_USER_FILE}")"
  password="$(cat "${PPPOE_PASSWORD_FILE}")"

  umask 077
  cat >"${PPP_OPTIONS}" <<OPTS
plugin rp-pppoe.so
nic-${ROUTER_WAN_IF}
ifname ${PPP_IF}
linkname ${PPP_LINKNAME}
user "${user}"
password "${password}"
mtu ${PPPOE_MTU}
mru ${PPPOE_MTU}
noauth
noipdefault
nodefaultroute
noipv6
maxfail 3
lcp-echo-interval 10
lcp-echo-failure 5
updetach
OPTS

  # Because of updetach, this call returns only once the IP layer is up.
  r pppd file "${PPP_OPTIONS}"
}

# The B4 side of DS-Lite. Nothing is translated here; NAT44 is the AFTR's job.
setup_dslite() {
  r ip link add "${DSLITE_IF}" type ip6tnl \
    mode ipip6 local "${WAN_V6_ROUTER}" remote "${WAN_V6_AFTR}" dev "${ROUTER_WAN_IF}" \
    encaplimit none
  r ip addr add "${DSLITE_B4_IP}/${DSLITE_PREFIXLEN}" dev "${DSLITE_IF}"
  r ip link set "${DSLITE_IF}" mtu "${DSLITE_MTU}" up
}

# Routing. The default is DS-Lite; only the hosts named by the range use PPPoE.
#
# Each table also carries the LAN route. Without it, traffic whose destination is
# rewritten back to the LAN after it was marked (the hairpin case) would follow the
# default route and leave for the outside.
setup_routing() {
  r ip route replace "${LAN_CIDR}" dev "${ROUTER_LAN_IF}" table "${TABLE_PPPOE}"
  r ip route replace default dev "${PPP_IF}" table "${TABLE_PPPOE}"

  r ip route replace "${LAN_CIDR}" dev "${ROUTER_LAN_IF}" table "${TABLE_DSLITE}"
  r ip route replace default dev "${DSLITE_IF}" table "${TABLE_DSLITE}"

  r ip route replace default dev "${DSLITE_IF}"

  r ip rule add fwmark "${MARK_PPPOE}" lookup "${TABLE_PPPOE}" pref 10
  r ip rule add fwmark "${MARK_DSLITE}" lookup "${TABLE_DSLITE}" pref 20
}

# Policy routing / NAT / firewall / MSS clamping.
#
# The address handed out over PPPoE can differ on every connection, so it is read here
# and baked into the rules. Application replaces whole tables, so running this twice
# leaves the same state.
setup_nftables() {
  local global_ip
  global_ip="$(r ip -4 -oneline addr show dev "${PPP_IF}" | awk '{print $4}' | cut -d/ -f1)"
  [[ -n "${global_ip}" ]] || die "could not read the PPPoE address"
  log "the PPPoE global address is ${global_ip}"

  r nft -f - <<NFT
table ip pbr
delete table ip pbr
table ip pbr {
	chain prerouting {
		type filter hook prerouting priority mangle; policy accept;
		iifname "${ROUTER_LAN_IF}" ip saddr ${PBR_PPPOE_RANGE_START}-${PBR_PPPOE_RANGE_END} meta mark set ${MARK_PPPOE} return
		iifname "${ROUTER_LAN_IF}" ip saddr ${LAN_CIDR} meta mark set ${MARK_DSLITE}
	}
}

table ip nat
delete table ip nat
table ip nat {
	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
		iifname "${PPP_IF}" tcp dport ${FORWARD_WAN_PORT} dnat to ${CLIENT_SERVER_IP}:${FORWARD_LAN_PORT}
		iifname "${ROUTER_LAN_IF}" ip daddr ${global_ip} tcp dport ${FORWARD_WAN_PORT} dnat to ${CLIENT_SERVER_IP}:${FORWARD_LAN_PORT}
	}

	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		oifname "${ROUTER_LAN_IF}" ip saddr ${LAN_CIDR} ip daddr ${CLIENT_SERVER_IP} tcp dport ${FORWARD_LAN_PORT} snat to ${ROUTER_LAN_IP}
		oifname "${PPP_IF}" masquerade
	}
}

table inet filter
delete table inet filter
table inet filter {
	chain input {
		type filter hook input priority filter; policy drop;
		ct state established,related accept
		ct state invalid drop
		iifname { "lo", "${ROUTER_LAN_IF}" } accept
		icmpv6 type { echo-request, echo-reply, destination-unreachable, packet-too-big, time-exceeded, parameter-problem, nd-router-solicit, nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } accept
		ip protocol icmp accept
		# ipip6 (protocol 4) arriving from the AFTR. DS-Lite return traffic comes in this way.
		iifname "${ROUTER_WAN_IF}" ip6 nexthdr 4 accept
	}

	chain forward {
		type filter hook forward priority filter; policy drop;
		ct state established,related accept
		ct state invalid drop
		iifname "${ROUTER_LAN_IF}" accept
		ct status dnat accept
	}

	chain output {
		type filter hook output priority filter; policy accept;
	}
}

table inet mangle
delete table inet mangle
table inet mangle {
	chain forward {
		type filter hook forward priority mangle; policy accept;
		oifname { "${PPP_IF}", "${DSLITE_IF}" } tcp flags syn tcp option maxseg size set ${MSS_CLAMP}
	}
}
NFT
}

up() {
  setup_sysctls
  setup_lan
  setup_wan_v6
  setup_dslite
  start_pppoe
  setup_routing
  setup_nftables
  log "assembled the device under test (the reference implementation)"
}

down() {
  if [[ -f "${PPP_PID_FILE}" ]]; then
    kill "$(cat "${PPP_PID_FILE}")" 2>/dev/null || true
    rm -f "${PPP_PID_FILE}"
  fi
  # The whole netns goes away, so there is nothing else to clean up.
}

case "${1:-}" in
up) up ;;
down) down ;;
*) die "usage: $(basename "$0") up|down" ;;
esac
