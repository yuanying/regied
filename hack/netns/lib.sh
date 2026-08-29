# 擬似 WAN テストベッドの共有定義。topo.sh と被試験体のセットアップ
# スクリプトの両方がこれを読む。
#
# ここの値は test/netns の定数と対になっている。片方だけ変えないこと。

# ---- netns ----------------------------------------------------------------

export NS_CLIENT="rg-client"
export NS_ROUTER="rg-router"
export NS_WAN="rg-wan"
export NS_INTERNET="rg-internet"

# ---- 被試験体から見えるインターフェース -----------------------------------
#
# router netns の中に、この 2 本だけが入った状態でセットアップスクリプトが
# 呼ばれる。LAN 側と、実機の eth0 に当たる WAN 側。

export ROUTER_LAN_IF="lan0"
export ROUTER_WAN_IF="wan0"

# ---- LAN ------------------------------------------------------------------

export LAN_CIDR="192.168.0.0/16"
export LAN_PREFIXLEN="16"
export ROUTER_LAN_IP="192.168.0.1"

# client netns が持つ送信元。PBR の振り分けをまたぐように置いてある。
export CLIENT_PPPOE_IP="192.168.1.20"   # PPPoE レンジ
export CLIENT_DSLITE_IP="192.168.1.200" # レンジ外（既定の DS-Lite）
export CLIENT_SERVER_IP="192.168.1.30"  # ポートフォワードの宛先

# PPPoE 側に振り分ける送信元レンジ。境界が CIDR に揃っていないのは意図した
# もので、レンジ照合が効いているかを見るためである。
export PBR_PPPOE_RANGE_START="192.168.1.10"
export PBR_PPPOE_RANGE_END="192.168.1.99"

# ---- PPPoE ----------------------------------------------------------------

export PPPOE_SERVER_IF="sub0" # wan netns 側の加入者向けインターフェース
export PPPOE_LOCAL_IP="198.51.100.1"
export PPPOE_REMOTE_IP="198.51.100.2" # router に払い出すグローバル
export PPPOE_MTU="1454"
export MSS_CLAMP="1414"

# 認証情報はファイル越しにしか渡さない。実運用の設定モデルと同じ扱いにする。
export PPPOE_USER_FILE="${RUNTIME_DIR:-/run/regied-netns}/pppoe-user"
export PPPOE_PASSWORD_FILE="${RUNTIME_DIR:-/run/regied-netns}/pppoe-password"

# ---- DS-Lite --------------------------------------------------------------

export WAN_V6_ROUTER="2001:db8:ff::1" # router の WAN 側 IPv6
export WAN_V6_AFTR="2001:db8:ff::2"   # AFTR の IPv6（トンネルの相手）
export WAN_V6_PREFIXLEN="64"
export DSLITE_B4_IP="192.0.0.2"  # RFC 7335 の B4 側
export DSLITE_AFTR_IP="192.0.0.1"  # RFC 7335 の AFTR 側
export DSLITE_PREFIXLEN="29"
export DSLITE_MTU="1460"
export AFTR_NAT_IP="192.0.2.1" # AFTR が NAT44 に使う外側アドレス

# ---- インターネット側 ------------------------------------------------------

export WAN_UPLINK_IF="net0"
export WAN_UPLINK_IP="203.0.113.1"
export INTERNET_IF="up0"
export INTERNET_IP_A="203.0.113.10"
export INTERNET_IP_B="203.0.113.20" # 宛先を変える検証のための 2 つ目
export INTERNET_PREFIXLEN="24"

export WHOAMI_TCP_PORT="8080"
export WHOAMI_UDP_PORT="9999"

# ---- ポートフォワード ------------------------------------------------------

export FORWARD_WAN_PORT="8022"
export FORWARD_LAN_PORT="22"
export SSH_STUB_BANNER="sshd-stub"

# ---- 実行時の置き場 --------------------------------------------------------

export RUNTIME_DIR="${RUNTIME_DIR:-/run/regied-netns}"
export PID_FILE="${RUNTIME_DIR}/services.pid"

# ---- 共通の道具 ------------------------------------------------------------

log() { printf '[netns] %s\n' "$*" >&2; }

die() {
  printf '[netns] %s\n' "$*" >&2
  exit 1
}

# netns の中でコマンドを実行する。
nse() {
  local ns="$1"
  shift
  ip netns exec "${ns}" "$@"
}

# netns の中の sysctl。全部の netns で同じものを立てるので関数にしてある。
ns_sysctl() {
  local ns="$1"
  shift
  local kv
  for kv in "$@"; do
    nse "${ns}" sysctl -qw "${kv}"
  done
}

# 条件が満たされるまで待つ。満たされなければ 0 以外で返る。
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
  log "${what} が ${timeout} 秒以内に整わなかった"
  return 1
}

# バックグラウンドで起こしたサービスの pid を控える。後始末で使う。
record_pid() {
  echo "$1" >>"${PID_FILE}"
}
