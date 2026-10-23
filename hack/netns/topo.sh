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
# inside to the script named by REGIED_NETNS_ROUTER_SETUP (ADR 0010). Setting
# REGIED_NETNS_ROUTER_CONTEXT=root leaves the device-side veth ends in the initial
# namespace. This is for a VM whose real systemd and networkd are part of the test.
#
# The provider side also delegates a prefix over DHCPv6. With the topology up, these move
# it, as a provider does:
#
#   topo.sh delegate <prefix>    delegate this prefix from now on; the router learns of it
#                                on its next renewal
#   topo.sh withdraw             stop the DHCPv6 server; what it delegated expires
#   topo.sh concentrator <mac>   move the PPPoE session to an access concentrator with this
#                                MAC address; the router has to dial again
#
# Usage: topo.sh up | down | status | delegate <prefix> | withdraw | concentrator <mac>
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "${here}/lib.sh"

ROUTER_SETUP="${REGIED_NETNS_ROUTER_SETUP:-${here}/router/reference.sh}"

require_prerequisites() {
  [[ "$(id -u)" -eq 0 ]] || die "run as root (creating a netns needs CAP_NET_ADMIN)"

  local bin
  for bin in ip nft pppd pppoe-server socat kea-dhcp6; do
    command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found; use make test-netns-docker"
  done
  [[ -c /dev/ppp ]] || die "/dev/ppp is missing; a PPPoE session cannot be brought up"
  [[ -x "${ROUTER_SETUP}" ]] || die "the setup script of the device under test is not executable: ${ROUTER_SETUP}"

  # Debian confines kea-dhcp6 with an AppArmor profile that lets it open only the
  # distribution's own file names, which a host running its own Kea is already using.
  # The testbed keeps its files under RUNTIME_DIR instead.
  if grep -qs '^kea-dhcp6 (enforce)' /sys/kernel/security/apparmor/profiles; then
    die "the AppArmor profile kea-dhcp6 is enforced, so Kea cannot use ${KEA_DIR}; on a test host, put the profile in complain mode or unload it"
  fi
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
  local namespaces=("${NS_CLIENT}" "${NS_WAN}" "${NS_INTERNET}")
  if [[ "${ROUTER_CONTEXT}" == "netns" ]]; then
    namespaces+=("${NS_ROUTER}")
  elif [[ "${ROUTER_CONTEXT}" != "root" ]]; then
    die "REGIED_NETNS_ROUTER_CONTEXT must be netns or root"
  fi
  for ns in "${namespaces[@]}"; do
    ip netns add "${ns}"
    nse "${ns}" ip link set lo up
  done
}

create_links() {
  # client <-> router
  ip link add "cl0" type veth peer name "${ROUTER_LAN_IF}"
  ip link set "cl0" netns "${NS_CLIENT}"
  if [[ "${ROUTER_CONTEXT}" == "netns" ]]; then
    ip link set "${ROUTER_LAN_IF}" netns "${NS_ROUTER}"
  fi

  # router <-> wan (the side that stands in for eth0 on real hardware)
  ip link add "${ROUTER_WAN_IF}" type veth peer name "${PPPOE_SERVER_IF}"
  if [[ "${ROUTER_CONTEXT}" == "netns" ]]; then
    ip link set "${ROUTER_WAN_IF}" netns "${NS_ROUTER}"
  fi
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
  # The client's IPv6 addresses are placed by place_client_v6, from the delegation. It
  # takes no advertisement from the device under test, so that what it holds does not
  # depend on what the router advertises.
  ns_sysctl "${NS_CLIENT}" \
    net.ipv6.conf.all.accept_dad=0 \
    net.ipv6.conf.default.accept_dad=0 \
    net.ipv6.conf.cl0.accept_ra=0
  nse "${NS_CLIENT}" ip link set cl0 up
  nse "${NS_CLIENT}" ip route add default via "${ROUTER_LAN_IP}"
}

# The LAN hosts inside the delegation: the published one, the hidden one beside it, and
# the address of the host behind the published one, held on its loopback so that only the
# router's route to that subnet can reach it. They stand in for hosts that renumber
# themselves when the router's prefix moves, so they move with the delegation. Moving them
# is the testbed's job; following is the router's.
place_client_v6() {
  local old="$1" new="$2"
  if [[ -n "${old}" ]]; then
    nse "${NS_CLIENT}" ip -6 addr del "$(delegated_address "${old}" "${PD_LAN_SUBNET_ID}" "${PD_PUBLISHED_TOKEN}")/64" dev cl0 2>/dev/null || true
    nse "${NS_CLIENT}" ip -6 addr del "$(delegated_address "${old}" "${PD_LAN_SUBNET_ID}" "${PD_HIDDEN_TOKEN}")/64" dev cl0 2>/dev/null || true
    nse "${NS_CLIENT}" ip -6 addr del "$(delegated_address "${old}" "${PD_DOWNSTREAM_SUBNET_ID}" "${PD_DOWNSTREAM_TOKEN}")/128" dev lo 2>/dev/null || true
  fi
  nse "${NS_CLIENT}" ip -6 addr add "$(delegated_address "${new}" "${PD_LAN_SUBNET_ID}" "${PD_PUBLISHED_TOKEN}")/64" dev cl0
  nse "${NS_CLIENT}" ip -6 addr add "$(delegated_address "${new}" "${PD_LAN_SUBNET_ID}" "${PD_HIDDEN_TOKEN}")/64" dev cl0
  nse "${NS_CLIENT}" ip -6 addr add "$(delegated_address "${new}" "${PD_DOWNSTREAM_SUBNET_ID}" "${PD_DOWNSTREAM_TOKEN}")/128" dev lo
  nse "${NS_CLIENT}" ip -6 route replace default \
    via "$(delegated_address "${new}" "${PD_LAN_SUBNET_ID}" "${PD_ROUTER_LAN_TOKEN}")" dev cl0
}

setup_internet() {
  nse "${NS_INTERNET}" sysctl -qw net.ipv6.conf.all.accept_dad=0 net.ipv6.conf.default.accept_dad=0
  nse "${NS_INTERNET}" ip addr add "${INTERNET_IP_A}/${INTERNET_PREFIXLEN}" dev "${INTERNET_IF}"
  nse "${NS_INTERNET}" ip addr add "${INTERNET_IP_B}/${INTERNET_PREFIXLEN}" dev "${INTERNET_IF}"
  nse "${NS_INTERNET}" ip -6 addr add "${INTERNET_V6}/${INTERNET_V6_PREFIXLEN}" dev "${INTERNET_IF}"
  nse "${NS_INTERNET}" ip link set "${INTERNET_IF}" up
  nse "${NS_INTERNET}" ip route add default via "${WAN_UPLINK_IP}"
  nse "${NS_INTERNET}" ip -6 route add default via "${WAN_UPLINK_V6}"
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
  nse "${NS_WAN}" ip -6 addr add "${WAN_UPLINK_V6}/${INTERNET_V6_PREFIXLEN}" dev "${WAN_UPLINK_IF}"
  nse "${NS_WAN}" ip link set "${WAN_UPLINK_IF}" up

  nse "${NS_WAN}" ip -6 addr add "${WAN_V6_AFTR}/${WAN_V6_PREFIXLEN}" dev "${PPPOE_SERVER_IF}"
  nse "${NS_WAN}" ip link set "${PPPOE_SERVER_IF}" up

  # The provider routes what it delegates to the router. Both prefixes are routed all the
  # time; which one the router actually holds is what the checks look at.
  local prefix
  for prefix in "${PD_FIRST}" "${PD_SECOND}"; do
    nse "${NS_WAN}" ip -6 route add "${prefix}" via "${WAN_V6_ROUTER}" dev "${PPPOE_SERVER_IF}"
  done

  setup_aftr
  setup_pppoe_server
}

# The router's B4 address under a delegation: the address it holds on the LAN.
b4_address() {
  delegated_address "$1" "${PD_LAN_SUBNET_ID}" "${PD_ROUTER_LAN_TOKEN}"
}

# The DS-Lite AFTR. It terminates the ipip6 tunnel the router brings up, applies NAT44,
# and sends the traffic out to the internet netns. Not translating on the B4 (router)
# side is what makes this DS-Lite.
#
# The far end is the address the delegation puts on the router's LAN, and it moves when
# the delegation does. A real AFTR takes a tunnel from any address its provider routes
# to a subscriber, and a tunnel from a prefix the provider no longer routes to that
# subscriber gets nothing back. Pinning the far end is how the testbed says the same: a
# tunnel whose local address did not follow the delegation carries nothing.
setup_aftr() {
  nse "${NS_WAN}" ip link add aftr0 type ip6tnl \
    mode ipip6 local "${WAN_V6_AFTR}" remote "$(b4_address "${PD_FIRST}")" dev "${PPPOE_SERVER_IF}" \
    encaplimit none
  nse "${NS_WAN}" ip addr add "${DSLITE_AFTR_IP}/${DSLITE_PREFIXLEN}" dev aftr0
  nse "${NS_WAN}" ip link set aftr0 mtu "${DSLITE_MTU}" up
  # The route that sends replies back through the tunnel once NAT has been undone and
  # the destination is a private LAN address again.
  nse "${NS_WAN}" ip route add "${LAN_CIDR}" dev aftr0

  # A real AFTR is a separate box with a connection table of its own. Here it shares
  # this namespace, and its conntrack, with the PPPoE server. Without the zone below, a
  # reply that arrived over PPPoE and was sent back through the tunnel by mistake would
  # be matched to the connection the PPPoE side had already seen, skip the NAT44, and
  # reach the client untranslated -- and a wrong return path would look right from
  # outside. So everything the tunnel carries is tracked in a zone of its own, and a
  # packet the AFTR cannot attribute to a connection of its own is dropped, which is
  # what a separate box would do.
  nse "${NS_WAN}" nft -f - <<NFT
table ip aftr {
	chain zone {
		type filter hook prerouting priority raw; policy accept;
		iifname "aftr0" ct zone set 1
		iifname "${WAN_UPLINK_IF}" ip daddr ${AFTR_NAT_IP} ct zone set 1
	}

	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname "aftr0" ct state invalid drop
	}

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

  start_pppoe_server
}

start_pppoe_server() {
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

# Another access concentrator takes over the line: the server goes away, taking the
# session with it, and comes back under a different MAC address. The router has to dial
# again, and what it can tell about the change is the concentrator's MAC. The link itself
# stays up, as the subscriber's Ethernet does when the provider moves the line.
#
# The check runs in this shell: a pattern handed to bash -c would find that shell's own
# command line.
pppoe_server_gone() {
  ! pgrep -f "pppoe-server -I ${PPPOE_SERVER_IF}" >/dev/null
}

move_concentrator() {
  local mac="$1"
  pkill -f "pppoe-server -I ${PPPOE_SERVER_IF}" 2>/dev/null || true
  wait_for "the PPPoE server to stop" 10 pppoe_server_gone || die "the PPPoE server did not stop"
  nse "${NS_WAN}" ip link set "${PPPOE_SERVER_IF}" address "${mac}"
  start_pppoe_server
  log "the access concentrator is now ${mac}"
}

# ---- The DHCPv6 server ----------------------------------------------------
#
# Kea, the server the distribution ships, delegating one /56 at a time from a pool of
# exactly that /56. It answers on the subscriber-facing interface, where the router's
# prefix-delegation client asks, and hands out no address (IA_NA), as such providers do.
#
# Its leases persist across restarts, as a provider's do.
#
# Debian's Kea refuses log and data paths outside its own directories unless told
# otherwise, so the log goes to standard output and the data directory comes from the
# environment. It opens its socket only once the interface has a carrier, which arrives
# when the device under test brings its end up, so it is told to keep trying.

write_kea_config() {
  local prefix="${1%/*}"
  mkdir -p "${KEA_DIR}"
  cat >"${KEA_DIR}/kea-dhcp6.conf" <<KEA
{
  "Dhcp6": {
    "interfaces-config": {
      "interfaces": [ "${PPPOE_SERVER_IF}" ],
      "service-sockets-max-retries": 100000,
      "service-sockets-retry-wait-time": 1000
    },
    "lease-database": {
      "type": "memfile",
      "persist": true,
      "name": "kea-leases6.csv",
      "lfc-interval": 0
    },
    "renew-timer": ${PD_RENEW_TIMER},
    "rebind-timer": ${PD_REBIND_TIMER},
    "preferred-lifetime": ${PD_PREFERRED_LIFETIME},
    "valid-lifetime": ${PD_VALID_LIFETIME},
    "subnet6": [
      {
        "id": 1,
        "subnet": "${WAN_V6_ROUTER%::*}::/${WAN_V6_PREFIXLEN}",
        "interface": "${PPPOE_SERVER_IF}",
        "rapid-commit": true,
        "pd-pools": [
          { "prefix": "${prefix}", "prefix-len": ${PD_LENGTH}, "delegated-len": ${PD_LENGTH} }
        ]
      }
    ],
    "loggers": [
      { "name": "kea-dhcp6", "severity": "INFO", "output-options": [ { "output": "stdout" } ] }
    ]
  }
}
KEA
}

# Kea writes its own pid file, named after the configuration file, and that is what
# stop_kea reads.
KEA_PID_FILE="${KEA_DIR}/kea-dhcp6.kea-dhcp6.pid"

start_kea() {
  local prefix="$1"
  write_kea_config "${prefix}"
  KEA_PIDFILE_DIR="${KEA_DIR}" KEA_LOCKFILE_DIR="${KEA_DIR}" KEA_DHCP_DATA_DIR="${KEA_DIR}" \
    ip netns exec "${NS_WAN}" kea-dhcp6 -c "${KEA_DIR}/kea-dhcp6.conf" \
    </dev/null >>"${RUNTIME_DIR}/kea-dhcp6.log" 2>&1 &
  wait_for "the DHCPv6 server to start" 10 test -s "${KEA_PID_FILE}" || die "the DHCPv6 server did not start"
  echo "${prefix}" >"${DELEGATION_FILE}"
  log "started the DHCPv6 server on ${PPPOE_SERVER_IF} (delegating ${prefix})"
}

stop_kea() {
  [[ -s "${KEA_PID_FILE}" ]] || return 0
  local pid
  pid="$(cat "${KEA_PID_FILE}")"
  kill "${pid}" 2>/dev/null || true
  wait_for "the DHCPv6 server to stop" 10 bash -c "! kill -0 ${pid}" || die "the DHCPv6 server did not stop"
  rm -f "${KEA_PID_FILE}"
}

# The provider moves the subscriber to another prefix. The server restarts with only the
# new prefix in its pool and keeps its bindings, and on the client's next renewal it
# hands back the old prefix with zero lifetimes and the new one beside it. Its AFTR then
# accepts the tunnel from the new prefix only, and the LAN hosts renumber into it.
delegate() {
  local prefix="$1" old=""
  [[ -f "${DELEGATION_FILE}" ]] && old="$(cat "${DELEGATION_FILE}")"
  stop_kea
  start_kea "${prefix}"
  nse "${NS_WAN}" ip -6 tunnel change aftr0 remote "$(b4_address "${prefix}")"
  if [[ "${old}" != "${prefix}" ]]; then
    place_client_v6 "${old}" "${prefix}"
  fi
}

withdraw() {
  stop_kea
  log "stopped the DHCPv6 server; what it delegated expires within ${PD_VALID_LIFETIME}s"
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

  # The same again on the host policy routing sends out DS-Lite. A forward to it is
  # reachable only if the reply goes back by the uplink the connection arrived on.
  nse "${NS_CLIENT}" socat \
    "TCP4-LISTEN:${FORWARD_DSLITE_HOST_LAN_PORT},bind=${CLIENT_DSLITE_IP},reuseaddr,fork" \
    "SYSTEM:echo \"${DSLITE_HOST_STUB_BANNER} \$SOCAT_PEERADDR\"" </dev/null >>"${RUNTIME_DIR}/ssh-stub.log" 2>&1 &
  record_pid "$!"

  # The LAN hosts inside the delegation answer over IPv6 with the address they were
  # reached at, whichever of the client's addresses that is.
  nse "${NS_CLIENT}" socat \
    "TCP6-LISTEN:${INBOUND_V6_PORT},ipv6only=1,reuseaddr,fork" \
    'SYSTEM:echo "$SOCAT_SOCKADDR"' </dev/null >>"${RUNTIME_DIR}/v6-stub.log" 2>&1 &
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
  place_client_v6 "" "${PD_FIRST}"
  start_services
  start_kea "${PD_FIRST}"

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
  pkill -f "kea-dhcp6 -c ${KEA_DIR}/" 2>/dev/null || true

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
  printf '=== delegation ===\n%s\n' "$(cat "${DELEGATION_FILE}" 2>/dev/null || echo "(none)")"
  if [[ "${ROUTER_CONTEXT}" == "root" ]]; then
    printf '=== root device under test ===\n'
    ip -brief addr show dev "${ROUTER_LAN_IF}" 2>/dev/null || printf '(absent)\n'
    ip -brief addr show dev "${ROUTER_WAN_IF}" 2>/dev/null || printf '(absent)\n'
  fi
}

case "${1:-}" in
up) up ;;
down)
  [[ "$(id -u)" -eq 0 ]] || die "run as root"
  down_quietly
  log "the topology is down"
  ;;
status) status ;;
delegate)
  [[ -n "${2:-}" ]] || die "usage: $(basename "$0") delegate <prefix>"
  delegate "$2"
  ;;
withdraw) withdraw ;;
concentrator)
  [[ -n "${2:-}" ]] || die "usage: $(basename "$0") concentrator <mac>"
  move_concentrator "$2"
  ;;
*) die "usage: $(basename "$0") up|down|status|delegate <prefix>|withdraw|concentrator <mac>" ;;
esac
