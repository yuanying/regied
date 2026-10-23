#!/usr/bin/env bash
#
# The reference router's dhcpcd hook. dhcpcd holds the delegation; this puts what the
# reference derives from it in place, the way a declaration written with
# fromDelegatedPrefix asks for:
#
#   - the LAN address, subnet PD_LAN_SUBNET_ID, token PD_ROUTER_LAN_TOKEN
#   - DS-Lite's local address, which is that LAN address
#   - a route to subnet PD_DOWNSTREAM_SUBNET_ID via the published LAN host
#   - the set of addresses the firewall lets in: the published host and that subnet
#
# When nothing is delegated, the address, the route and the set's elements go and the
# tunnel is left as it is: a tunnel cannot exist without a local address, so it waits
# for the next delegation.
#
# dhcpcd runs this inside the router netns, with the lease in its environment.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "${here}/../lib.sh"

# What the reference last put in place, so that the next call can take it away.
STATE_FILE="${RUNTIME_DIR}/router-delegation"
DSLITE_IF="dslite0"

# The delegated prefix in the lease that still has a lifetime. A renewal that moves the
# delegation carries the old prefix too, with zero lifetimes.
current_delegation() {
  local i prefix length vltime
  for ((i = 1; ; i++)); do
    prefix="new_dhcp6_ia_pd1_prefix${i}"
    [[ -n "${!prefix:-}" ]] || return 0
    length="${prefix}_length"
    vltime="${prefix}_vltime"
    if [[ "${!vltime:-0}" != 0 ]]; then
      printf '%s/%s\n' "${!prefix}" "${!length}"
      return 0
    fi
  done
}

remove() {
  local old="$1"
  ip -6 addr del "$(delegated_address "${old}" "${PD_LAN_SUBNET_ID}" "${PD_ROUTER_LAN_TOKEN}")/64" \
    dev "${ROUTER_LAN_IF}" 2>/dev/null || true
  ip -6 route del "$(delegated_address "${old}" "${PD_DOWNSTREAM_SUBNET_ID}")" 2>/dev/null || true
  nft flush set inet filter published 2>/dev/null || true
}

apply() {
  local prefix="$1"
  local b4 published downstream
  b4="$(delegated_address "${prefix}" "${PD_LAN_SUBNET_ID}" "${PD_ROUTER_LAN_TOKEN}")"
  published="$(delegated_address "${prefix}" "${PD_LAN_SUBNET_ID}" "${PD_PUBLISHED_TOKEN}")"
  downstream="$(delegated_address "${prefix}" "${PD_DOWNSTREAM_SUBNET_ID}")"

  ip -6 addr replace "${b4}/64" dev "${ROUTER_LAN_IF}"
  ip -6 tunnel change "${DSLITE_IF}" local "${b4}"
  ip -6 route replace "${downstream}" via "${published}" dev "${ROUTER_LAN_IF}"
  nft flush set inet filter published
  nft add element inet filter published "{ ${published}, ${downstream} }"
}

old=""
[[ -f "${STATE_FILE}" ]] && old="$(cat "${STATE_FILE}")"

case "${reason:-}" in
BOUND6 | RENEW6 | REBIND6 | REBOOT6)
  new="$(current_delegation)"
  if [[ -z "${new}" ]]; then
    [[ -z "${old}" ]] || remove "${old}"
    rm -f "${STATE_FILE}"
  elif [[ "${new}" != "${old}" ]]; then
    [[ -z "${old}" ]] || remove "${old}"
    apply "${new}"
    echo "${new}" >"${STATE_FILE}"
    log "the reference router follows the delegation ${new}"
  fi
  ;;
EXPIRE6 | STOP6 | RELEASE6 | NAK6 | DEPARTED | NOCARRIER | STOPPED)
  if [[ -n "${old}" ]]; then
    remove "${old}"
    rm -f "${STATE_FILE}"
    log "the reference router lost the delegation ${old}"
  fi
  ;;
esac
