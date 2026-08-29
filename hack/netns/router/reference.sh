#!/usr/bin/env bash
#
# 被試験体の参照実装。手書きの ip / nft で、テストベッドが検証する 7 項目を
# 満たすルーターを router netns の中に組み立てる。
#
# ここはテストベッドの一部ではなく「差し替えられる側」である。同じ約束
# （ADR 0010 の受け渡し）を満たすスクリプトを REGIED_NETNS_ROUTER_SETUP で
# 指せば、既存の実装でも自作の実装でも同じテストを掛けられる。
#
# 使い方: reference.sh up | down
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "${here}/../lib.sh"

# PPPoE の口。名前を固定しておかないと、提供側の pppd と番号を取り合って
# ppp0 になったり ppp1 になったりする。
PPP_IF="ppp-wan"
PPP_LINKNAME="regied-netns"
PPP_OPTIONS="${RUNTIME_DIR}/router-pppd-options"
PPP_PID_FILE="/var/run/ppp-${PPP_LINKNAME}.pid"

# DS-Lite のトンネル。
DSLITE_IF="dslite0"

# 経路表。PBR で送信元レンジごとに使い分ける。
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

# WAN 側の IPv6。実回線では RA と DHCPv6-PD で得るものを、テストベッドでは
# 固定で置いている。DS-Lite はこの IPv6 到達性の上に乗る。
setup_wan_v6() {
  r ip -6 addr add "${WAN_V6_ROUTER}/${WAN_V6_PREFIXLEN}" dev "${ROUTER_WAN_IF}"
  r ip link set "${ROUTER_WAN_IF}" up
  r ip -6 route replace default via "${WAN_V6_AFTR}" dev "${ROUTER_WAN_IF}"
}

# PPPoE。認証情報は設定に書かずファイルから読む。
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

  # updetach なので、この呼び出しは IP が上がってから返る。
  r pppd file "${PPP_OPTIONS}"
}

# DS-Lite の B4 側。ここでは NAT しない。NAT44 は AFTR の仕事である。
setup_dslite() {
  r ip link add "${DSLITE_IF}" type ip6tnl \
    mode ipip6 local "${WAN_V6_ROUTER}" remote "${WAN_V6_AFTR}" dev "${ROUTER_WAN_IF}" \
    encaplimit none
  r ip addr add "${DSLITE_B4_IP}/${DSLITE_PREFIXLEN}" dev "${DSLITE_IF}"
  r ip link set "${DSLITE_IF}" mtu "${DSLITE_MTU}" up
}

# 経路。既定は DS-Lite で、PPPoE 側はレンジで指定された端末だけが使う。
#
# 各テーブルに LAN の経路も入れてある。入れておかないと、印を付けた後に
# 宛先が LAN に書き換わる通信（hairpin）が、既定経路に引きずられて外へ
# 出て行ってしまう。
setup_routing() {
  r ip route replace "${LAN_CIDR}" dev "${ROUTER_LAN_IF}" table "${TABLE_PPPOE}"
  r ip route replace default dev "${PPP_IF}" table "${TABLE_PPPOE}"

  r ip route replace "${LAN_CIDR}" dev "${ROUTER_LAN_IF}" table "${TABLE_DSLITE}"
  r ip route replace default dev "${DSLITE_IF}" table "${TABLE_DSLITE}"

  r ip route replace default dev "${DSLITE_IF}"

  r ip rule add fwmark "${MARK_PPPOE}" lookup "${TABLE_PPPOE}" pref 10
  r ip rule add fwmark "${MARK_DSLITE}" lookup "${TABLE_DSLITE}" pref 20
}

# PBR / NAT / ファイアウォール / MSS clamp。
#
# PPPoE で払い出されたアドレスは接続のたびに変わりうるので、ここで読み取って
# 規則に埋める。適用は table 単位の置き換えにしてあり、二度流しても同じ
# 状態になる。
setup_nftables() {
  local global_ip
  global_ip="$(r ip -4 -oneline addr show dev "${PPP_IF}" | awk '{print $4}' | cut -d/ -f1)"
  [[ -n "${global_ip}" ]] || die "PPPoE のアドレスを読めなかった"
  log "PPPoE のグローバルは ${global_ip}"

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
		# AFTR から来る ipip6（プロトコル 4）。DS-Lite の戻りはこれで入る。
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
  log "被試験体（参照実装）を組み立てた"
}

down() {
  if [[ -f "${PPP_PID_FILE}" ]]; then
    kill "$(cat "${PPP_PID_FILE}")" 2>/dev/null || true
    rm -f "${PPP_PID_FILE}"
  fi
  # netns ごと消えるので、それ以外の後始末は要らない。
}

case "${1:-}" in
up) up ;;
down) down ;;
*) die "使い方: $(basename "$0") up|down" ;;
esac
