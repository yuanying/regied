#!/usr/bin/env bash
#
# Build and tear down the pseudo-WAN topology.
#
#   [client] --- [router (device under test)] --- [wan] --- [internet]
#                                                  |
#                                PPPoE server / DS-Lite AFTR
#
# This script does not touch the device under test. It goes as far as placing the LAN-
# side and WAN-side interfaces into the router netns, and leaves configuring what is
# inside to the script named by REGIED_NETNS_ROUTER_SETUP (ADR 0010).
#
# Usage: topo.sh up | down | status
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "${here}/lib.sh"

ROUTER_SETUP="${REGIED_NETNS_ROUTER_SETUP:-${here}/router/reference.sh}"

require_prerequisites() {
  [[ "$(id -u)" -eq 0 ]] || die "run as root (creating a netns needs CAP_NET_ADMIN)"

  local bin
  for bin in ip nft pppd pppoe-server socat; do
    command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found; use make test-netns-docker"
  done
  [[ -c /dev/ppp ]] || die "/dev/ppp is missing; a PPPoE session cannot be brought up"
  [[ -x "${ROUTER_SETUP}" ]] || die "the setup script of the device under test is not executable: ${ROUTER_SETUP}"
}

load_modules() {
  # ip6_tunnel and veth are usually already loaded on the host, but PPPoE is not there
  # unless something uses it. Loading it from inside the container requires /lib/modules
  # to be passed in (which run-in-docker.sh does).
  local mod
  for mod in pppoe ip6_tunnel veth; do
    if ! grep -qE "^${mod} " /proc/modules; then
      modprobe "${mod}" 2>/dev/null || log "could not load ${mod}; fine if it is built in"
    fi
  done
}

create_namespaces() {
  local ns
  for ns in "${NS_CLIENT}" "${NS_ROUTER}" "${NS_WAN}" "${NS_INTERNET}"; do
    ip netns add "${ns}"
    nse "${ns}" ip link set lo up
  done
}

create_links() {
  # client <-> router
  ip link add "cl0" type veth peer name "${ROUTER_LAN_IF}"
  ip link set "cl0" netns "${NS_CLIENT}"
  ip link set "${ROUTER_LAN_IF}" netns "${NS_ROUTER}"

  # router <-> wan (the side that stands in for eth0 on real hardware)
  ip link add "${ROUTER_WAN_IF}" type veth peer name "${PPPOE_SERVER_IF}"
  ip link set "${ROUTER_WAN_IF}" netns "${NS_ROUTER}"
  ip link set "${PPPOE_SERVER_IF}" netns "${NS_WAN}"

  # wan <-> internet
  ip link add "${WAN_UPLINK_IF}" type veth peer name "${INTERNET_IF}"
  ip link set "${WAN_UPLINK_IF}" netns "${NS_WAN}"
  ip link set "${INTERNET_IF}" netns "${NS_INTERNET}"
}

setup_client() {
  local ip_addr
  for ip_addr in "${CLIENT_PPPOE_IP}" "${CLIENT_DSLITE_IP}" "${CLIENT_SERVER_IP}"; do
    nse "${NS_CLIENT}" ip addr add "${ip_addr}/${LAN_PREFIXLEN}" dev cl0
  done
  nse "${NS_CLIENT}" ip link set cl0 up
  nse "${NS_CLIENT}" ip route add default via "${ROUTER_LAN_IP}"
}

setup_internet() {
  nse "${NS_INTERNET}" ip addr add "${INTERNET_IP_A}/${INTERNET_PREFIXLEN}" dev "${INTERNET_IF}"
  nse "${NS_INTERNET}" ip addr add "${INTERNET_IP_B}/${INTERNET_PREFIXLEN}" dev "${INTERNET_IF}"
  nse "${NS_INTERNET}" ip link set "${INTERNET_IF}" up
  nse "${NS_INTERNET}" ip route add default via "${WAN_UPLINK_IP}"
}

setup_wan() {
  ns_sysctl "${NS_WAN}" \
    net.ipv4.ip_forward=1 \
    net.ipv6.conf.all.forwarding=1 \
    net.ipv4.conf.all.rp_filter=0 \
    net.ipv6.conf.all.accept_dad=0 \
    net.ipv6.conf.default.accept_dad=0

  nse "${NS_WAN}" ip addr add "${WAN_UPLINK_IP}/${INTERNET_PREFIXLEN}" dev "${WAN_UPLINK_IF}"
  # The address the AFTR uses as the NAT44 source. This one address is what lets an
  # outside observer tell traffic that went over DS-Lite from traffic that went over
  # PPPoE.
  nse "${NS_WAN}" ip addr add "${AFTR_NAT_IP}/32" dev "${WAN_UPLINK_IF}"
  nse "${NS_WAN}" ip link set "${WAN_UPLINK_IF}" up

  nse "${NS_WAN}" ip -6 addr add "${WAN_V6_AFTR}/${WAN_V6_PREFIXLEN}" dev "${PPPOE_SERVER_IF}"
  nse "${NS_WAN}" ip link set "${PPPOE_SERVER_IF}" up

  setup_aftr
  setup_pppoe_server
}

# The DS-Lite AFTR. It terminates the ipip6 tunnel the router brings up, applies NAT44,
# and sends the traffic out to the internet netns. Not translating on the B4 (router)
# side is what makes this DS-Lite.
setup_aftr() {
  nse "${NS_WAN}" ip link add aftr0 type ip6tnl \
    mode ipip6 local "${WAN_V6_AFTR}" remote "${WAN_V6_ROUTER}" dev "${PPPOE_SERVER_IF}" \
    encaplimit none
  nse "${NS_WAN}" ip addr add "${DSLITE_AFTR_IP}/${DSLITE_PREFIXLEN}" dev aftr0
  nse "${NS_WAN}" ip link set aftr0 mtu "${DSLITE_MTU}" up
  # The route that sends replies back through the tunnel once NAT has been undone and
  # the destination is a private LAN address again.
  nse "${NS_WAN}" ip route add "${LAN_CIDR}" dev aftr0

  nse "${NS_WAN}" nft -f - <<NFT
table ip aftr {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		iifname "aftr0" oifname "${WAN_UPLINK_IF}" snat to ${AFTR_NAT_IP}
	}
}
NFT
}

setup_pppoe_server() {
  # Credentials go into files. The device under test is told nothing but "read this
  # file". This is also a rehearsal of the policy that they must never be writable
  # directly into the configuration.
  umask 077
  printf 'testuser\n' >"${PPPOE_USER_FILE}"
  printf 'testpass\n' >"${PPPOE_PASSWORD_FILE}"

  cat >/etc/ppp/pap-secrets <<SECRETS
# For the regied netns testbed. Not used outside the tests.
"testuser" * "testpass" *
SECRETS
  chmod 600 /etc/ppp/pap-secrets

  # Authenticate subscribers out of pap-secrets alone (never against OS accounts).
  cat >/etc/ppp/pppoe-server-options <<OPTS
require-pap
lcp-echo-interval 10
lcp-echo-failure 5
mtu ${PPPOE_MTU}
mru ${PPPOE_MTU}
noipv6
nodefaultroute
noipdefault
OPTS

  # The server stays in user space. In kernel mode (-k) on Debian 13 the session comes up
  # and then LCP goes nowhere: the client sends Config-Requests until it times out. That
  # is ppp 2.5.2 with pppoe 4.0; the same invocation worked with 2.4.9 and 3.15. User
  # space costs nothing here, where the whole testbed carries a few hundred packets.
  nse "${NS_WAN}" pppoe-server \
    -I "${PPPOE_SERVER_IF}" \
    -L "${PPPOE_LOCAL_IP}" \
    -R "${PPPOE_REMOTE_IP}" \
    -N 4 -F </dev/null >>"${RUNTIME_DIR}/pppoe-server.log" 2>&1 &
  record_pid "$!"
  log "started the PPPoE server on ${PPPOE_SERVER_IF} (handing out ${PPPOE_REMOTE_IP})"
}

# The reachability servers. All they do is answer with one line naming the peer they
# saw. They are the mirror through which the shape of traffic after NAT is observed
# from the outside.
start_services() {
  # The internet side: two destinations. These are needed to confirm that the external
  # port does not change when the destination does.
  local ip_addr
  for ip_addr in "${INTERNET_IP_A}" "${INTERNET_IP_B}"; do
    nse "${NS_INTERNET}" socat \
      "TCP4-LISTEN:${WHOAMI_TCP_PORT},bind=${ip_addr},reuseaddr,fork" \
      'SYSTEM:echo "$SOCAT_PEERADDR:$SOCAT_PEERPORT"' </dev/null >>"${RUNTIME_DIR}/whoami.log" 2>&1 &
    record_pid "$!"

    nse "${NS_INTERNET}" socat \
      "UDP4-RECVFROM:${WHOAMI_UDP_PORT},bind=${ip_addr},reuseaddr,fork" \
      'SYSTEM:echo "$SOCAT_PEERADDR:$SOCAT_PEERPORT"' </dev/null >>"${RUNTIME_DIR}/whoami.log" 2>&1 &
    record_pid "$!"
  done

  # The LAN side: the destination of both the port forward and the hairpin. It answers
  # with the peer as well, so even whether the hairpin rewrote the source address is
  # visible from outside.
  nse "${NS_CLIENT}" socat \
    "TCP4-LISTEN:${FORWARD_LAN_PORT},bind=${CLIENT_SERVER_IP},reuseaddr,fork" \
    "SYSTEM:echo \"${SSH_STUB_BANNER} \$SOCAT_PEERADDR\"" </dev/null >>"${RUNTIME_DIR}/ssh-stub.log" 2>&1 &
  record_pid "$!"
}

# Whether the device under test managed to bring up PPPoE is judged from the provider
# side, not the subscriber side, so that nothing has to look inside the device under
# test.
wait_for_pppoe_session() {
  wait_for "the PPPoE session" 45 \
    bash -c "ip netns exec ${NS_WAN} ip -4 route show ${PPPOE_REMOTE_IP} | grep -q ."
}

up() {
  require_prerequisites
  down_quietly
  load_modules

  mkdir -p "${RUNTIME_DIR}"
  : >"${PID_FILE}"

  create_namespaces
  create_links
  setup_internet
  setup_wan
  setup_client
  start_services

  log "building the device under test: ${ROUTER_SETUP}"
  "${ROUTER_SETUP}" up

  wait_for_pppoe_session || die "the PPPoE session was never established"
  log "the topology is up"
}

kill_recorded_services() {
  [[ -f "${PID_FILE}" ]] || return 0
  local pid
  while read -r pid; do
    [[ -n "${pid}" ]] || continue
    kill "${pid}" 2>/dev/null || true
  done <"${PID_FILE}"
}

down_quietly() {
  if [[ -x "${ROUTER_SETUP}" ]]; then
    "${ROUTER_SETUP}" down 2>/dev/null || true
  fi
  kill_recorded_services
  pkill -f "pppoe-server -I ${PPPOE_SERVER_IF}" 2>/dev/null || true

  local ns
  for ns in "${NS_CLIENT}" "${NS_ROUTER}" "${NS_WAN}" "${NS_INTERNET}"; do
    ip netns del "${ns}" 2>/dev/null || true
  done
  # A safety net for links left behind when deleting a netns failed.
  local link
  for link in cl0 "${ROUTER_LAN_IF}" "${ROUTER_WAN_IF}" "${PPPOE_SERVER_IF}" "${WAN_UPLINK_IF}" "${INTERNET_IF}"; do
    ip link del "${link}" 2>/dev/null || true
  done
  rm -rf "${RUNTIME_DIR}"
}

status() {
  local ns
  for ns in "${NS_CLIENT}" "${NS_ROUTER}" "${NS_WAN}" "${NS_INTERNET}"; do
    printf '=== %s ===\n' "${ns}"
    ip netns exec "${ns}" ip -brief addr show 2>/dev/null || printf '(absent)\n'
    ip netns exec "${ns}" ip route show 2>/dev/null || true
  done
}

case "${1:-}" in
up) up ;;
down)
  [[ "$(id -u)" -eq 0 ]] || die "run as root"
  down_quietly
  log "the topology is down"
  ;;
status) status ;;
*) die "usage: $(basename "$0") up|down|status" ;;
esac
