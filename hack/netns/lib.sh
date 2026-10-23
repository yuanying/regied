# Shared definitions for the pseudo-WAN testbed. Both topo.sh and the setup script
# of the device under test source this file.
#
# The values here are paired with the constants in test/netns. Do not change one side
# without the other.

# ---- netns ----------------------------------------------------------------

export NS_CLIENT="rg-client"
export NS_ROUTER="rg-router"
export NS_WAN="rg-wan"
export NS_INTERNET="rg-internet"

# The reference device lives in NS_ROUTER. A systemd-backed device can instead use the
# initial namespace, where the real service manager and networkd are running.
export ROUTER_CONTEXT="${REGIED_NETNS_ROUTER_CONTEXT:-netns}"
export ROUTER_MGMT_IF="${REGIED_NETNS_MGMT_IF:-}"

# ---- Interfaces the device under test sees --------------------------------
#
# The setup script is called with the router netns holding exactly these two links:
# the LAN side, and the WAN side that stands in for eth0 on real hardware.

export ROUTER_LAN_IF="lan0"
export ROUTER_WAN_IF="wan0"

# ---- LAN ------------------------------------------------------------------

export LAN_CIDR="192.168.0.0/16"
export LAN_PREFIXLEN="16"
export ROUTER_LAN_IP="192.168.0.1"

# The source addresses the client netns owns. They are placed so that they straddle
# the policy-routing split.
export CLIENT_PPPOE_IP="192.168.1.20"   # inside the PPPoE range
export CLIENT_DSLITE_IP="192.168.1.200" # outside the range (the default, DS-Lite)
export CLIENT_SERVER_IP="192.168.1.30"  # the destination of the port forward

# The source range routed over PPPoE. Its bounds deliberately do not line up with a
# CIDR block, so that the test shows whether range matching really works.
export PBR_PPPOE_RANGE_START="192.168.1.10"
export PBR_PPPOE_RANGE_END="192.168.1.99"

# ---- PPPoE ----------------------------------------------------------------

export PPPOE_SERVER_IF="sub0" # the subscriber-facing interface on the wan netns side
export PPPOE_LOCAL_IP="198.51.100.1"
export PPPOE_REMOTE_IP="198.51.100.2" # the global address handed out to the router
export PPPOE_MTU="1454"
export MSS_CLAMP="1414"

# Credentials are passed only through files, the same way the production configuration
# model treats them.
export PPPOE_USER_FILE="${RUNTIME_DIR:-/run/regied-netns}/pppoe-user"
export PPPOE_PASSWORD_FILE="${RUNTIME_DIR:-/run/regied-netns}/pppoe-password"

# ---- DS-Lite --------------------------------------------------------------

export WAN_V6_ROUTER="2001:db8:ff::1" # the router's WAN-side IPv6
export WAN_V6_AFTR="2001:db8:ff::2"   # the AFTR's IPv6 (the far end of the tunnel)
export WAN_V6_PREFIXLEN="64"
export DSLITE_B4_IP="192.0.0.2"  # the B4 side, per RFC 7335
export DSLITE_AFTR_IP="192.0.0.1"  # the AFTR side, per RFC 7335
export DSLITE_PREFIXLEN="29"
export DSLITE_MTU="1460"
export AFTR_NAT_IP="192.0.2.1" # the outside address the AFTR uses for NAT44

# ---- The internet side ----------------------------------------------------

export WAN_UPLINK_IF="net0"
export WAN_UPLINK_IP="203.0.113.1"
export INTERNET_IF="up0"
export INTERNET_IP_A="203.0.113.10"
export INTERNET_IP_B="203.0.113.20" # a second one, for checks that vary the destination
export INTERNET_PREFIXLEN="24"

export WHOAMI_TCP_PORT="8080"
export WHOAMI_UDP_PORT="9999"

# The internet side over IPv6, for the checks that reach into the delegated prefix from
# outside.
export WAN_UPLINK_V6="2001:db8:fe::1"
export INTERNET_V6="2001:db8:fe::10"
export INTERNET_V6_PREFIXLEN="64"

# ---- Prefix delegation ----------------------------------------------------
#
# A DHCPv6 server on the subscriber-facing interface delegates one of two /56s at a time,
# as a provider does over the same line that carries PPPoE. Moving from one to the other
# is the prefix change the checks reproduce. The provider side routes both to the router.
export PD_FIRST="2001:db8:100::/56"
export PD_SECOND="2001:db8:200::/56"
export PD_LENGTH="56"

# Short lifetimes, so that a renewal carries a new prefix within seconds and a withdrawn
# one expires within half a minute. A real line uses hours.
export PD_RENEW_TIMER="5"
export PD_REBIND_TIMER="8"
export PD_PREFERRED_LIFETIME="15"
export PD_VALID_LIFETIME="20"

# The lifetimes of a real line, for the checks that need the router to keep what it holds
# unless it asks again. Nothing is renewed within the length of a check.
export PD_LONG_RENEW_TIMER="3600"
export PD_LONG_REBIND_TIMER="5400"
export PD_LONG_PREFERRED_LIFETIME="7000"
export PD_LONG_VALID_LIFETIME="7200"

# Where things sit inside the delegation: what a declaration writes as
# fromDelegatedPrefix. The router's LAN address is also DS-Lite's local address, so the
# AFTR accepts the tunnel from that address only.
export PD_LAN_SUBNET_ID="1"
export PD_ROUTER_LAN_TOKEN="::1"
export PD_PUBLISHED_TOKEN="::30" # a LAN host the firewall lets the outside reach
export PD_HIDDEN_TOKEN="::31"    # a LAN host on the same /64 that it does not
export PD_DOWNSTREAM_SUBNET_ID="2" # a /64 routed to the published host, as to a host behind it
export PD_DOWNSTREAM_TOKEN="::1"

# The port a LAN host answers on over IPv6. The answer is the address it was reached at.
export INBOUND_V6_PORT="8030"

# ---- Port forwarding ------------------------------------------------------

export FORWARD_WAN_PORT="8022"
export FORWARD_LAN_PORT="22"
export SSH_STUB_BANNER="sshd-stub"

# A second forward, published on PPPoE like the first, whose target is the host that
# policy routing sends out DS-Lite. Its reply has to leave by the uplink the connection
# arrived on, not by the uplink the target's own traffic leaves by.
export FORWARD_DSLITE_HOST_WAN_PORT="8023"
export FORWARD_DSLITE_HOST_LAN_PORT="22"
export DSLITE_HOST_STUB_BANNER="sshd-stub-dslite-host"

# ---- Run-time state -------------------------------------------------------

export RUNTIME_DIR="${RUNTIME_DIR:-/run/regied-netns}"
export PID_FILE="${RUNTIME_DIR}/services.pid"
export KEA_DIR="${RUNTIME_DIR}/kea"
# The prefix the server delegates now, for the commands that move it.
export DELEGATION_FILE="${RUNTIME_DIR}/delegation"
# The lifetimes it hands out, short or long.
export LIFETIMES_FILE="${RUNTIME_DIR}/lifetimes"

# ---- Shared helpers -------------------------------------------------------

log() { printf '[netns] %s\n' "$*" >&2; }

die() {
  printf '[netns] %s\n' "$*" >&2
  exit 1
}

# Run a command inside a network namespace.
nse() {
  local ns="$1"
  shift
  ip netns exec "${ns}" "$@"
}

router_exec() {
  if [[ "${ROUTER_CONTEXT}" == "root" ]]; then
    "$@"
  else
    nse "${NS_ROUTER}" "$@"
  fi
}

# sysctl inside a network namespace. The same settings go up in every netns, which is
# why this is a function.
ns_sysctl() {
  local ns="$1"
  shift
  local kv
  for kv in "$@"; do
    nse "${ns}" sysctl -qw "${kv}"
  done
}

# Wait until a condition holds. Returns non-zero if it never does.
wait_for() {
  local what="$1" timeout="$2"
  shift 2

  local deadline=$((SECONDS + timeout))
  while ((SECONDS < deadline)); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  log "${what} was not ready within ${timeout}s"
  return 1
}

# Record the pid of a service started in the background, for the teardown to use.
record_pid() {
  echo "$1" >>"${PID_FILE}"
}

# Print the eight groups of an IPv6 address as decimal numbers, "::" expanded.
ipv6_groups() {
  local addr="$1" head="$1" tail=""
  local -a h=() t=() g=()
  if [[ "${addr}" == *::* ]]; then
    head="${addr%%::*}"
    tail="${addr#*::}"
  fi
  [[ -z "${head}" ]] || IFS=: read -ra h <<<"${head}"
  [[ -z "${tail}" ]] || IFS=: read -ra t <<<"${tail}"
  g=("${h[@]}")
  local i
  for ((i = ${#h[@]} + ${#t[@]}; i < 8; i++)); do
    g+=(0)
  done
  g+=("${t[@]}")
  for i in "${g[@]}"; do
    printf '%d ' "$((16#${i}))"
  done
}

# delegated_address <prefix/len> <subnet ID> [token]
#
# What fromDelegatedPrefix means: the subnet's /64 inside a delegated prefix, or, given a
# token, the address in it. The arithmetic is the one test/netns pins down with unit
# tests (delegation.go); the subnet ID is assumed to fit.
delegated_address() {
  local prefix="${1%/*}" subnet_id="$2" token="${3:-}"
  local -a p=() h=(0 0 0 0 0 0 0 0)
  read -ra p <<<"$(ipv6_groups "${prefix}")"
  [[ -z "${token}" ]] || read -ra h <<<"$(ipv6_groups "${token}")"

  local net=$(((p[0] << 48) | (p[1] << 32) | (p[2] << 16) | p[3] | subnet_id))
  local out
  out="$(printf '%x:%x:%x:%x' $(((net >> 48) & 0xffff)) $(((net >> 32) & 0xffff)) \
    $(((net >> 16) & 0xffff)) $((net & 0xffff)))"
  if [[ -z "${token}" ]]; then
    printf '%s::/64\n' "${out}"
  else
    printf '%s:%x:%x:%x:%x\n' "${out}" "${h[4]}" "${h[5]}" "${h[6]}" "${h[7]}"
  fi
}
