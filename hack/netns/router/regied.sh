#!/usr/bin/env bash
#
# Run regied as the device under test on a VM with real systemd and networkd.
#
# Use with REGIED_NETNS_ROUTER_CONTEXT=root and point REGIED_NETNS_ROUTER_SETUP at
# this script. The management link is named only to keep SSH and ICMP admitted; networkd's
# earlier configuration remains authoritative for its addresses and routes. Before
# applying, this script renders and dry-runs the declaration.
#
# Usage: regied.sh up | down
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "${here}/../lib.sh"

repo_root="$(cd "${here}/../../.." && pwd)"
binary="/tmp/regied-netns-bin"
declaration="${RUNTIME_DIR}/regied.yaml"
rendered="${RUNTIME_DIR}/rendered.txt"
dry_run="${RUNTIME_DIR}/dry-run.txt"
mgmt_before="${RUNTIME_DIR}/mgmt-before"
serve_pid_file="${RUNTIME_DIR}/regied-serve.pid"

management_state() {
  ip -o addr show dev "${ROUTER_MGMT_IF}" | awk '{ print $3, $4 }'
  ip route show default dev "${ROUTER_MGMT_IF}"
}

management_reachable() {
  local gateway
  gateway="$(ip -6 route show default dev "${ROUTER_MGMT_IF}" | awk '{ for (i = 1; i < NF; i++) if ($i == "via") { print $(i + 1); exit } }')"
  if [[ -n "${gateway}" ]]; then
    ping -6 -I "${ROUTER_MGMT_IF}" -c 1 -W 3 "${gateway}" >/dev/null
    return
  fi
  gateway="$(ip route show default dev "${ROUTER_MGMT_IF}" | awk '{ for (i = 1; i < NF; i++) if ($i == "via") { print $(i + 1); exit } }')"
  [[ -n "${gateway}" ]] && ping -I "${ROUTER_MGMT_IF}" -c 1 -W 3 "${gateway}" >/dev/null
}

remove_firewall() {
  nft delete table inet regied 2>/dev/null || true
}

process_gone() {
  ! kill -0 "$1" 2>/dev/null
}

render_declaration() {
  awk '
    {
      line = $0
      while (match(line, /\$\{[A-Z0-9_]+\}/)) {
        key = substr(line, RSTART + 2, RLENGTH - 3)
        if (!(key in ENVIRON)) {
          print "missing environment value " key > "/dev/stderr"
          exit 1
        }
        line = substr(line, 1, RSTART - 1) ENVIRON[key] substr(line, RSTART + RLENGTH)
      }
      print line
    }
  ' "${here}/regied.yaml.in" >"${declaration}"
}

verify_preview() {
  "${binary}" render -config "${declaration}" >"${rendered}"
  "${binary}" apply -config "${declaration}" -dry-run >"${dry_run}"
  grep -q "${ROUTER_LAN_IF}" "${rendered}" || die "rendering does not contain the test LAN link"
  grep -q "${ROUTER_WAN_IF}" "${rendered}" || die "rendering does not contain the test WAN link"
}

up() {
  [[ "${ROUTER_CONTEXT}" == "root" ]] || die "regied.sh requires REGIED_NETNS_ROUTER_CONTEXT=root"
  [[ -n "${ROUTER_MGMT_IF}" ]] || die "REGIED_NETNS_MGMT_IF is required"
  mkdir -p "${RUNTIME_DIR}"
  management_state >"${mgmt_before}"
  management_reachable || die "the management gateway is not reachable before apply"
  go build -o "${binary}" ./cmd/regied
  render_declaration
  verify_preview
  "${binary}" apply -config "${declaration}"

  local state_after
  state_after="$(management_state)"
  if [[ "${state_after}" != "$(cat "${mgmt_before}")" ]]; then
    remove_firewall
    log "management state before apply: $(tr '\n' ';' <"${mgmt_before}")"
    log "management state after apply: ${state_after//$'\n'/;}"
    die "apply changed the management address or default route; the regied table was removed"
  fi
  if ! management_reachable; then
    remove_firewall
    die "the management gateway was unreachable after apply; the regied table was removed"
  fi

  wait_for "the PPPoE link" 45 ip link show pppoe0 || die "regied did not establish PPPoE"
  wait_for "the DS-Lite tunnel" 20 ip link show dslite0 || die "regied did not establish DS-Lite"

  "${binary}" serve -control "${RUNTIME_DIR}/control.sock" -resync 1m \
    >"${RUNTIME_DIR}/serve.log" 2>&1 &
  echo "$!" >"${serve_pid_file}"
  wait_for "reconciliation of the DS-Lite link" 20 \
    bash -c '[[ "$(< /proc/sys/net/ipv4/conf/dslite0/rp_filter)" == 0 ]]' || \
    die "regied serve did not reconcile the DS-Lite link"
}

down() {
  local before=""
  if [[ -n "${ROUTER_MGMT_IF}" ]] && ip link show "${ROUTER_MGMT_IF}" >/dev/null 2>&1; then
    before="$(management_state)"
  fi
  if [[ -f "${serve_pid_file}" ]]; then
    local serve_pid
    serve_pid="$(cat "${serve_pid_file}")"
    kill -TERM "${serve_pid}" 2>/dev/null || true
    wait_for "regied serve to stop" 10 process_gone "${serve_pid}" || \
      die "regied serve did not stop after SIGTERM"
  fi
  systemctl disable --now regied-pppoe@pppoe0.service 2>/dev/null || true
  systemctl disable --now regied-dnsmasq.service 2>/dev/null || true
  rm -f /etc/systemd/network/50-regied-lan.network \
    /etc/systemd/network/50-regied-wan.network \
    /etc/systemd/network/50-regied-mgmt.network \
    /etc/systemd/network/50-regied-dslite0.netdev \
    /etc/systemd/network/50-regied-dslite0.network \
    /etc/systemd/network/50-regied-pppoe0.network \
    /etc/systemd/system/regied-pppoe@.service \
    /etc/systemd/system/regied-dnsmasq.service \
    /etc/ppp/ip-up.d/regied-uplink-set \
    /etc/ppp/ip-down.d/regied-uplink-set
  rm -rf /etc/regied/ppp /etc/regied/dnsmasq /var/lib/regied
  systemctl daemon-reload 2>/dev/null || true
  networkctl reload 2>/dev/null || true
  ip link del dslite0 2>/dev/null || true
  remove_firewall
  rm -f "${binary}"
  if [[ -n "${before}" ]] && [[ "$(management_state)" != "${before}" ]]; then
    die "teardown changed the management address or default route"
  fi
}

case "${1:-}" in
up) up ;;
down) down ;;
*) die "usage: $(basename "$0") up|down" ;;
esac
