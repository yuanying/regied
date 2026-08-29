#!/usr/bin/env bash
#
# netns 擬似 WAN テストベッドを特権コンテナの中で走らせる。
#
# 開発機は Docker コンテナの中にあり、ホストの docker daemon に届く。
# ここで起動するのは兄弟コンテナである。リポジトリはホストと同じパスで
# 見えるので、同名パスでそのまま渡せる。
#
# 使い方: hack/netns/run-in-docker.sh <コンテナの中で実行するコマンド...>
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${here}/../.." && pwd)"
image="${NETNS_IMAGE:-regied-netns:latest}"

# ビルドキャッシュを持ち回らないと毎回標準ライブラリから作り直しになる。
cache_dir="${HOME}/.cache/regied-netns/go-build"
mkdir -p "${cache_dir}"

args=(
  run --rm --privileged
  # PPPoE はモジュールなので、コンテナの中から読み込めるようにする。
  --volume /lib/modules:/lib/modules:ro
  --volume "${repo_root}:${repo_root}"
  --volume "${cache_dir}:/root/.cache/go-build"
  --workdir "${repo_root}"
  --env REGIED_NETNS_REQUIRE=1
)

if [[ -n "${REGIED_NETNS_KEEP:-}" ]]; then
  args+=(--env "REGIED_NETNS_KEEP=${REGIED_NETNS_KEEP}")
fi
if [[ -n "${REGIED_NETNS_ROUTER_SETUP:-}" ]]; then
  args+=(--env "REGIED_NETNS_ROUTER_SETUP=${REGIED_NETNS_ROUTER_SETUP}")
fi
if [[ -t 0 && -t 1 ]]; then
  args+=(--interactive --tty)
fi

exec docker "${args[@]}" "${image}" "$@"
