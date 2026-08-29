#!/usr/bin/env bash
#
# 擬似 WAN トポロジの構築と破棄。
#
#   [client] --- [router（被試験体）] --- [wan] --- [internet]
#                                          |
#                            PPPoE サーバー / DS-Lite の AFTR
#
# このスクリプトは被試験体に触らない。router netns には LAN 側と WAN 側の
# インターフェースを入れるところまでを行い、その中身の設定は
# REGIED_NETNS_ROUTER_SETUP で指されたスクリプトに任せる（ADR 0010）。
#
# 使い方: topo.sh up | down | status
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "${here}/lib.sh"

ROUTER_SETUP="${REGIED_NETNS_ROUTER_SETUP:-${here}/router/reference.sh}"

require_prerequisites() {
  [[ "$(id -u)" -eq 0 ]] || die "root で実行すること（netns の作成に CAP_NET_ADMIN が要る）"

  local bin
  for bin in ip nft pppd pppoe-server socat; do
    command -v "${bin}" >/dev/null 2>&1 || die "${bin} が見つからない。make test-netns-docker を使うこと"
  done
  [[ -c /dev/ppp ]] || die "/dev/ppp が無い。PPPoE を張れない"
  [[ -x "${ROUTER_SETUP}" ]] || die "被試験体のセットアップスクリプトが実行できない: ${ROUTER_SETUP}"
}

load_modules() {
  # ip6_tunnel と veth はホストで読み込み済みのことが多いが、PPPoE は
  # 使われていないと入っていない。コンテナから読み込むには /lib/modules を
  # 渡してもらう必要がある（run-in-docker.sh がやっている）。
  local mod
  for mod in pppoe ip6_tunnel veth; do
    if ! grep -qE "^${mod} " /proc/modules; then
      modprobe "${mod}" 2>/dev/null || log "${mod} を読み込めなかった。組み込みなら問題ない"
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

  # router <-> wan（実機の eth0 に当たる側）
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
  # AFTR が NAT44 の送信元に使うアドレス。この 1 つで DS-Lite 経由の通信を
  # PPPoE 経由と外から見分けられる。
  nse "${NS_WAN}" ip addr add "${AFTR_NAT_IP}/32" dev "${WAN_UPLINK_IF}"
  nse "${NS_WAN}" ip link set "${WAN_UPLINK_IF}" up

  nse "${NS_WAN}" ip -6 addr add "${WAN_V6_AFTR}/${WAN_V6_PREFIXLEN}" dev "${PPPOE_SERVER_IF}"
  nse "${NS_WAN}" ip link set "${PPPOE_SERVER_IF}" up

  setup_aftr
  setup_pppoe_server
}

# DS-Lite の AFTR。router から張られる ipip6 をここで終端し、NAT44 して
# internet netns へ出す。B4（router）側は NAT しないのが DS-Lite である。
setup_aftr() {
  nse "${NS_WAN}" ip link add aftr0 type ip6tnl \
    mode ipip6 local "${WAN_V6_AFTR}" remote "${WAN_V6_ROUTER}" dev "${PPPOE_SERVER_IF}" \
    encaplimit none
  nse "${NS_WAN}" ip addr add "${DSLITE_AFTR_IP}/${DSLITE_PREFIXLEN}" dev aftr0
  nse "${NS_WAN}" ip link set aftr0 mtu "${DSLITE_MTU}" up
  # NAT を戻した後の宛先（LAN の私設アドレス）をトンネルへ返す経路。
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
  # 認証情報はファイルに置く。被試験体には「このファイルを読め」としか
  # 渡さない。設定に直接書けるようにしないという方針の練習でもある。
  umask 077
  printf 'testuser\n' >"${PPPOE_USER_FILE}"
  printf 'testpass\n' >"${PPPOE_PASSWORD_FILE}"

  cat >/etc/ppp/pap-secrets <<SECRETS
# regied の netns テストベッド用。テスト以外では使われない。
"testuser" * "testpass" *
SECRETS
  chmod 600 /etc/ppp/pap-secrets

  # 加入者の認証は pap-secrets だけで行う（OS のアカウントは見ない）。
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

  nse "${NS_WAN}" pppoe-server \
    -I "${PPPOE_SERVER_IF}" \
    -L "${PPPOE_LOCAL_IP}" \
    -R "${PPPOE_REMOTE_IP}" \
    -N 4 -k -F </dev/null >>"${RUNTIME_DIR}/pppoe-server.log" 2>&1 &
  record_pid "$!"
  log "PPPoE サーバーを ${PPPOE_SERVER_IF} で起動した（払い出し ${PPPOE_REMOTE_IP}）"
}

# 到達確認用のサーバー。内容は「見えた接続元」を一行返すだけ。
# NAT を抜けた後の姿を外側から観測するための鏡である。
start_services() {
  # internet 側: 宛先を 2 つ用意する。宛先を変えても外部ポートが変わらない
  # ことを確かめるのに要る。
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

  # LAN 側: ポートフォワードと hairpin の宛先。接続元も返すので、
  # hairpin で送信元が書き換わっているかどうかまで外から分かる。
  nse "${NS_CLIENT}" socat \
    "TCP4-LISTEN:${FORWARD_LAN_PORT},bind=${CLIENT_SERVER_IP},reuseaddr,fork" \
    "SYSTEM:echo \"${SSH_STUB_BANNER} \$SOCAT_PEERADDR\"" </dev/null >>"${RUNTIME_DIR}/ssh-stub.log" 2>&1 &
  record_pid "$!"
}

# 被試験体が PPPoE を張れたかどうかは、加入者側ではなく提供側で見る。
# 被試験体の中を覗かずに済ませるため。
wait_for_pppoe_session() {
  wait_for "PPPoE セッション" 45 \
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

  log "被試験体を組み立てる: ${ROUTER_SETUP}"
  "${ROUTER_SETUP}" up

  wait_for_pppoe_session || die "PPPoE セッションが張られなかった"
  log "トポロジを組み終えた"
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
  # netns の削除に失敗していたときの取りこぼし。
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
    ip netns exec "${ns}" ip -brief addr show 2>/dev/null || printf '（無い）\n'
    ip netns exec "${ns}" ip route show 2>/dev/null || true
  done
}

case "${1:-}" in
up) up ;;
down)
  [[ "$(id -u)" -eq 0 ]] || die "root で実行すること"
  down_quietly
  log "トポロジを片付けた"
  ;;
status) status ;;
*) die "使い方: $(basename "$0") up|down|status" ;;
esac
