# regied

A daemon that builds one Linux host's network policy from a single declarative YAML file.

The name comes from the French *régie* — the control room. **It does not perform; it cues
the performers.** systemd-networkd owns links and routes, dnsmasq owns DHCP and DNS, pppd
owns PPPoE. regied directs them, owns the layers nobody else does (the nftables firewall,
NAT, and the matching half of policy routing), and ties the whole thing into one
declaration.

A router is one kind of host regied applies to, not the only one.

> **Status: built, not yet deployed.** The engine, the resident loop and the confirmation
> workflow are implemented and pass their tests. regied has not yet run on real hardware
> with a real uplink. The configuration schema is settled in shape; individual field
> names may still move before the first release.

## Language

English is canonical. 日本語版は [README.ja.md](README.ja.md) にあります。

## What it owns, and what it delegates

This split is the point of the project, so it comes first. The reasoning is in
[ADR 0008](docs/adr/0008-delegate-to-existing-implementations.md).

| Area | Owner |
|---|---|
| Interfaces, addresses, MTU, bridges | systemd-networkd |
| Static routes (IPv4 / IPv6, with table selection) | systemd-networkd |
| Routing policy rules (firewall mark → table) | systemd-networkd |
| ip6tnl tunnels (DS-Lite) | systemd-networkd |
| DHCPv6 prefix delegation, RA / SLAAC advertisement | systemd-networkd |
| DHCP server, RA options, conditional DNS forwarding | dnsmasq |
| PPPoE session | pppd |
| **nftables firewall (IPv4 / IPv6)** | **regied** |
| **NAT: masquerade, port forwarding, hairpin** | **regied** |
| **Policy-routing match: source ranges, destination exclusions, sets** | **regied** |
| **Generating and supervising pppd and dnsmasq configuration** | **regied** |
| **One declaration over all of the above: dry-run, a record of what was accepted, a loop that keeps the host at it, and applying with a deadline** | **regied** |

Two consequences of that split are worth stating up front.

**regied owns only what it declared.** It rebuilds its own nftables tables rather than
flushing the ruleset, and it removes only routes it installed. Routes learned by a
routing daemon, and state owned by a container runtime or a CNI, are left alone
([ADR 0009](docs/adr/0009-ownership-boundary.md)).

**Distribution is not regied's job.** regied looks after one node. Getting configuration
files to nodes belongs to something else.

## Target configuration

regied is being built against a working configuration with these seven areas:

- **PPPoE** — the primary uplink
- **DS-Lite** — ipip6 tunnel, IPv4 over IPv6
- **Policy routing** — pick an uplink by source range, with destination exclusions
- **NAT** — masquerade, port forwarding, hairpin
- **nftables firewall** — IPv4 and IPv6, with named address sets
- **DHCP and DNS** — static leases, RA / DHCPv6, conditional forwarding
- **Static routes** — IPv4 and IPv6

There is no HTTP API. The design for a read-only one is recorded and deferred
([ADR 0007](docs/adr/0007-resident-process.md)); what the host is doing is answered by the
report of the last turn and by the journal, described under [Operating](#operating).

The assumed deployment has **exactly one uplink and one machine**. There is no
redundancy, and the operator reaches the host over the line it is reconfiguring. That
assumption drives the safety requirements
([ADR 0016](docs/adr/0016-converging-on-the-accepted-declaration.md)):

- **Apply is idempotent.** A turn that finds nothing to change runs no command.
- **`--dry-run` shows what would change** before anything is touched.
- **Nothing rolls back on its own.** A submission that fails stops at the step that
  failed, leaves the host at whatever prefix of the work got done, and says so. Every
  prefix is a safe state: the firewall goes in before forwarding is enabled. Going back is
  a person applying the previous file.
- **The host converges on what was accepted, not on the file.** Only `regied apply` reads
  the configuration file. The loop and the boot unit read the declaration the host
  accepted, so an unfinished edit never reaches the host on a timer.
- **A change can be applied with a deadline.** `regied apply --confirm` reverts to the
  previous declaration unless the operator, still able to reach the host, confirms it.
  This is the one automatic revert in the design, and it exists for lockout.

What regied deliberately does not do is in [docs/scope.md](docs/scope.md).

## Platform

regied is built and run on **Debian 13 (trixie)**. It installs nothing: systemd-networkd,
dnsmasq, pppd and nftables come from the distribution, networkd has to be enabled, and
nothing else may own the router's links. [ADR 0011](docs/adr/0011-target-platform.md)
records the versions that assumes, and the one networkd directive that is not in trixie
yet.

## Configuration

Configuration is a single YAML file listing resources, in the style of Kubernetes custom
resources: `kind: NetworkConfig`, host-wide switches in `spec.global`, and eleven
resource kinds in `spec.resources[]`.

- [`docs/spec/configuration.md`](docs/spec/configuration.md) — the document, references
  between resources, and what lands in which backend
- [`docs/spec/kinds.md`](docs/spec/kinds.md) — the kinds, field by field
- [`config/example.yaml`](config/example.yaml) — a worked example of a two-uplink host

The kinds and the division of responsibility are settled
([ADR 0002](docs/adr/0002-configuration-schema.md)); individual field names may still move
before the first release, which is what `v1alpha1` says.

Two properties of the schema are worth knowing before reading it.

**No field can hold a secret.** Credentials are named by the path of a file that holds
them, so a configuration file can be published or pasted into a bug report without review
([ADR 0003](docs/adr/0003-secrets-out-of-configuration.md)).

**No field accepts an uplink's own global address.** NAT and port forwarding refer to the
uplink resource instead, and the chain that marks traffic for policy routing runs after
destination translation. Together those mean hairpin works without anybody writing down
an address that changes.

## Build

```sh
make build              # build for the host
make build-arm64        # cross-compile for an arm64 SBC
```

The deployment target is an arm64 single-board computer running Debian 13
([ADR 0011](docs/adr/0011-target-platform.md)). Development and testing happen on amd64
Linux. The cross-build is static (`CGO_ENABLED=0`), so the binary copies onto the target
with no runtime dependency.

## Test

Tests are split by the privileges they need.

| target | command | requires |
|---|---|---|
| unit tests | `make test` | the Go toolchain, nothing else |
| netns integration tests | `make test-netns-docker` | Docker (starts a privileged container) |
| netns integration tests, directly | `make test-netns` | root / CAP_NET_ADMIN and `nft`, `pppd`, `pppoe-server`, `socat` |

The integration tests build a pseudo-WAN out of network namespaces — a PPPoE server,
a DS-Lite AFTR, and reachability servers — run a router inside it, and check the
following seven things from the outside.

1. Outbound traffic gets through over PPPoE
2. Outbound traffic gets through over DS-Lite
3. Policy routing picks an uplink per source range
4. A port forward reaches a host inside from outside
5. Hairpin NAT reaches it from inside, against the router's own global address
6. The firewall drops traffic it does not allow
7. NAT mapping is endpoint-independent

They need root and commands that a development environment normally lacks, so they
sit behind the `netns` build tag; `go test ./...` does not pick them up. Use
`make test-netns-docker` in the usual case: it prepares a container with the tooling
and calls `make test-netns` inside it.

Where the tooling is already present, `make test-netns` runs directly. A missing
prerequisite fails the run and names what is missing, because a skip that prints `ok` is
indistinguishable from a pass. A bare `go test -tags netns` skips instead, and
`make test-netns REGIED_NETNS_REQUIRE=` asks the target for the same.

### Swapping the device under test

Everything that assembles the router lives in one script, selected by the
`REGIED_NETNS_ROUTER_SETUP` environment variable. The default is a reference router
assembled by hand out of `ip` and `nft`. The contract is in
`docs/adr/0010-netns-testbed.md`.

To put a different implementation under test, write one script that satisfies that
contract and point the variable at it. The tests stay as they are.

### Looking around

`REGIED_NETNS_KEEP=1` leaves the topology up after a run. `make netns-shell` drops
into a shell in the same container, where `hack/netns/topo.sh up` builds it and
`hack/netns/topo.sh status` shows the addresses and routes of each namespace.
`hack/netns/topo.sh down` removes it.

## Documentation

- [`docs/spec/`](docs/spec/) — the configuration format and the resource kinds
- [`docs/scope.md`](docs/scope.md) — what regied does not do, and why
- [`docs/adr/`](docs/adr/) — architecture decision records. Read these before making
  changes; they record decisions that implementations should not quietly reverse.

## Prior art

The configuration model borrows its resource-kind naming and schema idioms from EdgeOS
and from [imksoo/routerd](https://github.com/imksoo/routerd). ADR 0001 records what was
measured and why we ended up building our own instead of adopting either.
