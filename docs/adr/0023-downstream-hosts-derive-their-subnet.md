# ADR 0023: A downstream host derives its subnet from the prefix it is advertised

- Status: Accepted (2026-10-23)
- Builds on [ADR 0021](0021-values-derived-from-the-delegated-prefix.md)

## Context

### The deployment

A router receives a delegated prefix — say `2001:db8:0:100::/56` — and advertises one subnet
of it on its LAN, `2001:db8:0:101::/64`. A host on that LAN runs a container runtime and
gives the containers a bridge of their own. IPv6 has no NAT, so the bridge needs a subnet
of its own out of the same delegation, `2001:db8:0:102::/64`, which the host advertises to
its containers. The router needs a route sending that subnet to the host's LAN address.

Today both hosts write it down as literals:

| Where | What is written | Literal prefix in it |
|---|---|---|
| The router's LAN `Interface.routes[]` | `2001:db8:0:102::/64 via 2001:db8:0:101::151` | Two |
| The downstream host's bridge `Interface.addresses[]` | `2001:db8:0:102::1/64`, advertised by `ipv6.advertise` | One |
| The downstream host's `IPAddressSet` for a container | `2001:db8:0:102::151` | One |
| The router's `IPAddressSet` for the same container | `2001:db8:0:102::151` | One |

The downstream host's own LAN address is already derived — `fromRouterAdvertisement` with
token `::151` — and followed the last prefix change on its own. Nothing else did.

### The question

**Can a downstream host's declaration hold no literal prefix at all, and can the router's
route to it and the host's own declaration stop being two hand-written statements of one
fact?** These are two questions. The first is about what changes when the prefix does; the
second is about what has to be kept in step when somebody re-plans which subnet goes where.

### Hierarchical delegation, checked against the platform

The textbook answer is for the router to delegate onward: run a DHCPv6 server that hands
the downstream host a prefix out of the router's delegation (an IA_PD), install the route
to it with the downstream host's address as next hop, and withdraw the route when the lease
ends. The downstream host would then hold exactly what regied already expresses on a
router — `dhcpv6.prefixDelegation` on its LAN, `fromDelegatedPrefix` on its bridge — and
the allocation would be a fact held in one place, the server's lease.

What the platform offers was checked in the sources of the versions the platform ships.

| Candidate | What it can do | Source |
|---|---|---|
| systemd-networkd 257 | **No DHCPv6 server.** `DHCPServer=` starts a DHCPv4 server, and the server library behind it opens an IPv4 socket only. On the IPv6 side networkd is a client (`DHCP=`, `[DHCPv6]`), a consumer of a delegation on downstream links (`[DHCPPrefixDelegation]`), and a router advertiser (`[IPv6SendRA]`) | `systemd.network(5)` for 257; `sd-dhcp-server.c` |
| dnsmasq | **No prefix delegation as a server.** Its DHCPv6 server allocates IA_NA and IA_TA only. IA_PD appears in exactly one place: as a relay, it snoops prefixes another server delegated through it, to install routes for them. Its "dynamically delegated subnets" are prefixes *this* host was delegated, used as the template for addresses and advertisements on its own links | `dnsmasq(8)`; `rfc3315.c` |
| Kea (`kea-dhcp6-server`, in Debian 13) | Delegates prefixes from pools, with reservations by DUID. It installs no route to what it delegates; that would take its script hook, or something reading its leases | The package and its documentation |

So hierarchical delegation is not available from anything regied already drives. It would
take a fifth backend, and a route lifecycle — install on lease, withdraw on expiry — that
regied would have to build or have Kea's hook build. That is the lifecycle
[ADR 0008](0008-delegate-to-existing-implementations.md) and
[ADR 0011](0011-target-platform.md) decline to write, for one deployment.

## Decision

### The downstream host derives its subnets from the advertisement it receives

**An `Interface` that accepts router advertisements may declare that the prefix advertised
to it is a subnet of a delegation of a stated length.** From that, the host knows the
delegated prefix as well as the router does: the advertised /64, cut back to the declared
length. `fromDelegatedPrefix` then means on this host exactly what it means on the router.

| Field | Where | Meaning |
|---|---|---|
| `ipv6.advertisedDelegation.prefixLength` | The downstream host's LAN `Interface` | The length of the delegation the advertised prefix belongs to, e.g. `56` |
| `fromDelegatedPrefix.interfaceRef` | Any field of [ADR 0021](0021-values-derived-from-the-delegated-prefix.md)'s table | May name an interface declaring `advertisedDelegation`, as well as one running a prefix-delegation client |

The interface must also hold a `fromRouterAdvertisement` address, and the advertised prefix
is read from it: the address networkd configured from the advertisement with the declared
token, as networkd describes the link ([ADR 0021](0021-values-derived-from-the-delegated-prefix.md)).
Its /64, cut back to `prefixLength`, is the delegated prefix on this host.

On the downstream host a resolved `fromDelegatedPrefix` reaches the host as a literal in the
files regied writes: networkd has no delegation here to derive from, so the bridge's address
is rendered as an `Address=` and the advertisement of it as an advertisement of a literal
prefix. The name of the form does not change with that: the declaration says the same thing
on both hosts, and which mechanism carries it is the backend's.

### What disappears, and what remains

Written this way, the four rows of the table at the top, and the record naming the container,
become:

| Where | Written as | Literal prefix |
|---|---|---|
| Router: the route | destination `fromDelegatedPrefix` subnet `2`; via `fromDelegatedPrefix` subnet `1`, token `::151` | **None** |
| Downstream host: the bridge | `fromDelegatedPrefix` through the LAN interface, subnet `2`, token `::1` | **None** |
| Downstream host: the container's set | `fromDelegatedPrefix` through the LAN interface, subnet `2`, token `::151` | **None** |
| Router: the container's set | `fromDelegatedPrefix` subnet `2`, token `::151` | **None** |
| Downstream host: the container's AAAA ([ADR 0024](0024-dns-records-inside-the-delegation.md)) | `fromDelegatedPrefix` through the LAN interface, subnet `2`, token `::151` | **None** |

**Every literal prefix leaves both declarations, and a prefix change needs no edit to
either host.** What is still written is prefix-independent:

| Still written | Where | Changes when |
|---|---|---|
| The delegation's length | The downstream host's LAN interface, once | The provider changes the length it delegates |
| The subnet ID handed to the downstream host (`2`) | **Both**: the router's route, and the downstream host's bridge | Somebody re-plans the subnets |
| The downstream host's LAN token (`::151`) | **Both**: the router's route as next hop, and the host's own `fromRouterAdvertisement` | Somebody re-numbers the host |
| A container's token | Every set and record naming it, on either host | Somebody re-numbers the container |

**So the allocation is still two hand-written statements.** "Subnet 2 belongs to the host at
`::151`" is said by the router's route and by the host's bridge, and nothing in regied can
check one against the other: they are two declarations, submitted to two hosts, and regied
looks after one ([`docs/scope.md`](../scope.md)). What this record removes is the part that
breaks on its own — the prefix, which the provider changes without asking. What remains
changes only when a person decides to change it, and that person is editing both files
anyway.

The step that would remove the pairing as well is hierarchical delegation, and the section
above is why it is not this record. Until then, the pairing is a property of the
configuration repository, which holds both declarations: a test there that every route the
router declares to a delegated subnet matches the bridge and the token of the host it points
at is cheap, and it is where the two files meet. That is distribution, and outside regied by
the same scope line.

### Its premises, and its weak points

The derivation rests on three premises, each of which can be false.

- **The advertised prefix is a subnet of the delegation.** True when the router advertises a
  subnet of its own delegation on the LAN, which is what `fromDelegatedPrefix` with
  `ipv6.advertise` does. A LAN that is also advertised a ULA, or a second provider's prefix,
  has more than one candidate; the token's address in each is still distinct, and regied
  takes the one that is a global unicast address. Two global candidates is a reported
  ambiguity, and the turn waits.
- **The delegation's length is what was declared.** A provider that changes it — a /56
  becoming a /60 or a /48 — moves every derived subnet on this host to the wrong place, and
  nothing on this host can tell. On the router, a `subnetID` that no longer fits is reported
  ([ADR 0021](0021-values-derived-from-the-delegated-prefix.md)); on the downstream host the
  computed prefix is just wrong. It is the one value here a provider can still break, and it
  is one line on one host.
- **The downstream host sees the change.** It learns the new prefix when the router
  advertises it and its own address in it appears; the address event wakes a turn, as
  on the router.

And two weak points that are not premises.

- **During a change, two prefixes are advertised for a while.** The old one stays on the
  downstream host's LAN address until its lifetime runs out. regied takes the address whose
  preferred lifetime is longest, which is the new one as soon as it has arrived, and reports
  the choice when there are two.
- **The old subnet lingers on the containers.** networkd 257, told to stop advertising a
  prefix, removes it from its advertisements; it does not announce it once more with a
  lifetime of zero (`sd_radv_remove_prefix` removes the option and sends nothing). A
  container keeps the old address until the lifetime it was last advertised expires — 24
  hours for a literal prefix by default — and may pick it as a source meanwhile. So **a
  prefix derived this way is advertised with short lifetimes by default**: a valid lifetime
  of one hour and a preferred lifetime of half that, which the advertisement refreshes
  every few minutes while it is current. `validLifetime` and `preferredLifetime` still
  apply, as to any literal prefix.

### The scope of this series

This is the boundary of ADR 0021 through this record, written down because the series is
large and the boundary decides what each unit owes.

| In | Out |
|---|---|
| The router's tunnel local address, routes and address sets | Hierarchical delegation; a DHCPv6 server of any kind |
| A downstream host running regied: its bridge's address and advertisement, its address sets | A host not running regied — one configured by netplan, a cluster's load-balancer pool. They hold their own literals, and the router's sets and records can name their addresses by derivation |
| AAAA records naming addresses inside the delegation ([ADR 0024](0024-dns-records-inside-the-delegation.md)) | An address a container picks with its own token: it is already derived |
| | The AFTR address, firewall rule CIDRs, resolver addresses, static host overrides ([ADR 0021](0021-values-derived-from-the-delegated-prefix.md)) |

## Alternatives considered

**Hierarchical delegation through Kea.** The only option that makes the allocation a single
fact. Rejected for now, not for good: it adds a fifth backend and a route lifecycle regied
would own, to remove a pairing that changes only when a person re-plans the network. If a
deployment grows enough downstream routers that the pairing is the thing that breaks, this
is the record to supersede.

**Advertising the downstream subnet from the router**, on the LAN, with neither the
on-link nor the autonomous flag, for the downstream host to pick up. Every other host on the
LAN would see it too, the semantics of such a prefix are not "this is yours", and networkd
has no way to consume it.

**Proxying neighbour discovery** so that the containers sit in the router's LAN subnet.
No route and no second subnet, at the price of a proxy daemon answering for addresses on a
segment that is not theirs. It is the arrangement a prefix change has already shown to be
forgotten, because nothing declares it.

**A subnet ID on the downstream host only, with the router routing the whole delegation
toward the LAN.** The router cannot route its whole delegation to one next hop and still
own the subnets it advertises itself.

## Consequences

- **A downstream host's declaration holds no literal prefix**, and neither does the router's
  route to it. A prefix change needs no edit on either host.
- **The subnet allocation is still written on both hosts**, and the configuration
  repository is where it can be checked. The value a provider can still change behind
  everybody's back is the delegation's length, written once.
- **A downstream host derives its prefix from what networkd configured from an
  advertisement.** That is reading an address networkd placed because of a file regied wrote,
  which [ADR 0021](0021-values-derived-from-the-delegated-prefix.md) refused as the router's
  source; here there is no lease to read, and the token is the only part of the address that
  came from regied.
- **A downstream host's bridge address becomes an address regied computes and networkd
  holds.** When it changes, the file changes and networkd is reloaded; whether networkd
  257 drops the old static address on that reload is confirmed by the unit that builds this.
- Derived prefixes on a downstream host are advertised with short lifetimes by default.

## Implementation units

Unit 5 of [ADR 0021](0021-values-derived-from-the-delegated-prefix.md): the
`ipv6.advertisedDelegation` field, `fromDelegatedPrefix` naming such an interface, rendering
the resolved address as a literal, and the default lifetimes. It follows unit 3, so that by
the time a downstream host derives its subnet, the router's route to it already does. The
downstream host's own records follow it, in unit 6.
