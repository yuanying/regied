#!/usr/bin/env bash
#
# Run the netns pseudo-WAN testbed inside a privileged container.
#
# The development machine is itself inside a Docker container and can reach the host's
# docker daemon. What this starts is therefore a sibling container. The repository is
# visible at the same path as on the host, so it can be passed through under the very
# same path.
#
# Usage: hack/netns/run-in-docker.sh <command to run inside the container...>
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${here}/../.." && pwd)"
image="${NETNS_IMAGE:-regied-netns:latest}"

# Without a build cache carried across runs, every run rebuilds from the standard library.
cache_dir="${HOME}/.cache/regied-netns/go-build"
mkdir -p "${cache_dir}"

args=(
  run --rm --privileged
  # PPPoE is a kernel module, so make it loadable from inside the container.
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
if [[ -n "${REGIED_NETNS_ROUTER_CONTEXT:-}" ]]; then
  args+=(--env "REGIED_NETNS_ROUTER_CONTEXT=${REGIED_NETNS_ROUTER_CONTEXT}")
fi
if [[ -t 0 && -t 1 ]]; then
  args+=(--interactive --tty)
fi

exec docker "${args[@]}" "${image}" "$@"
