# ADR 0018: A DNSForwarder listening on loopback is the host's own resolver

- Status: Accepted (2026-09-11)

## Context

A `DNSForwarder` names the links dnsmasq answers on, and `loopback` is a reserved name in
that list: the host itself is one of the segments served
([`kinds.md`](../spec/kinds.md#dnsforwarder)). A declaration that lists it, and that
refuses the provider's resolvers with `useDNS: false` on its uplinks, says that the host
resolves names through its own dnsmasq.

Nothing regied wrote made that true. The host's stub resolver, systemd-resolved, learns
its servers from its own configuration file and from what networkd reports per link.
With the uplinks told to take no resolvers, and no file naming any, resolved was left with
no server at all, and the host lost name resolution the moment the declaration was
accepted. On the first host this was tried on, package installation stopped working, and
so did time synchronisation, because the time servers are given by name. dnsmasq was
running and answering on the loopback the whole time; nobody had told the host to ask it.

The gap is regied's to close, and it is closed through networkd. On a host that hands its
network to networkd, resolved is what reads the resolver configuration, and a server
declared per link in a `.network` file reaches it without any other file being written.
[ADR 0009](0009-ownership-boundary.md) draws the boundary: regied writes networkd's files
under its own prefix and touches nothing else in that area. resolved's own configuration
file and `/etc/resolv.conf` are the operator's, like `sysctl.d`
([`configuration.md`](../spec/configuration.md)).

Two facts about resolved fix what a solution can look like. Both are read from its source
at the version [ADR 0011](0011-target-platform.md) fixes.

- **A per-link server is used only while the link is relevant**: up, with carrier, and
  holding at least one address that is not link-local. On a link networkd manages,
  networkd's own state has to be at least *degraded* as well. A bridge whose members are
  all disconnected has no carrier — the kernel derives a bridge's carrier from its ports —
  so a server attached to it goes unused exactly when nothing else is on the segment
  either. The loopback link is never relevant.
- **A server at a loopback address is reached through the loopback link, whichever link
  it was learnt on.** A `DNS=127.0.0.1` in the LAN bridge's file does not try to send
  through the bridge.

The same field trial showed a second thing. The bridge had no address until a client was
plugged in. networkd configures a link only once it has carrier, unless told otherwise,
and a bridge with no connected member has none. The segment's gateway address, the address
the host's own services are reached at, the routes through it, and the address derived
from the delegated prefix all came and went with the first client's cable.

## Decision

**When a `DNSForwarder`'s `listenOn` names `loopback`, every `.network` regied writes for
an `Interface` that is not a bridge port carries `DNS=127.0.0.1` and
`DNSDefaultRoute=yes`.** resolved takes these from networkd and asks dnsmasq for every name
no other link claims. There is no new field: `loopback` already said that the host answers
itself, and this completes what it says. Nothing is written when `loopback` is absent — the
forwarder then serves the segments below it, and the host keeps whatever resolver it had.

**Which files carry it.** The direct reading — the links `listenOn` names — fails at the
worst moment. resolved uses a per-link server only while the link is relevant, and a
bridge with nobody plugged in is not, so a host being set up over its console before the
first client would have no resolver, and neither would one whose segment emptied. The
loopback link cannot carry it at all. So the entry rides on every link regied gives an
address to: each `Interface` that is not enslaved, whatever `listenOn` says. Bridge ports
and the DS-Lite tunnel hold no address resolved would count, and the PPPoE link's file
stays what [ADR 0012](0012-networkd-rendering.md) made it — routes and
`KeepConfiguration=yes`, nothing else. The host then has its resolver while any addressed
link is up. That covers whichever link an operator reaches the host over, and a host on
which none of them is up has no upstream to forward to anyway. A name is asked once per
link that carries the entry; with one process behind all of them, that is a repeat, not a
disagreement.

**`127.0.0.1` alone, not `::1` as well.** dnsmasq listens on both — naming the loopback
link binds every address it has — but they are one process. resolved uses one server per
link at a time and moves to the next on failure, so a second address for the same process
is a failover to itself that only lengthens an outage. `127.0.0.1` is there on every Linux
host whatever its IPv6 settings say.

**`DNSDefaultRoute=yes`, not `Domains=~.`.** Both send unclaimed names to this link. The
routing domain is the mechanism an operator uses to steer zones to links, and regied has
no business declaring a domain on a link nobody declared one on. The boolean says the one
thing meant. Writing it out also keeps the effect from depending on resolved's automatic
rule, which any routing domain an operator's drop-in adds to the link switches off.

**A bridge is configured without carrier.** The `.network` of an `Interface` with
`bridge.members` carries `ConfigureWithoutCarrier=yes`. The bridge is a device the
declaration created. Its address is the gateway of the segment and the address the host's
own services answer at; the routes through it and the slice of the delegated prefix
assigned on it hang off that address. None of this depends on a member having link, so
none of it should come and go with one. networkd's default is right for a physical port,
where no link means nothing is reachable there, and wrong for a bridge, where no carrier
means no client yet. Carrier loss is ignored along with it, by networkd's own default for
that setting, so the last client unplugging removes nothing either.

This is not what makes the resolver decision work — resolved judges carrier for itself,
and a bridge without a connected member still has none — but it is what makes the
bridge's own configuration, and what dnsmasq binds on it, independent of whether anyone
is plugged in.

## Consequences

- A host that lists `loopback` resolves names through its own dnsmasq from the moment one
  addressed link is up, without a field for it and without regied writing resolved's
  configuration or `/etc/resolv.conf`. Taking `loopback` out takes the two lines out of
  every file with the next apply; they are reclaimed with the files (ADR 0009).
- The lines are inert where resolved is not the host's resolver. A host with a static
  `/etc/resolv.conf` keeps it, and the boundary stands. ADR 0011 does not list resolved
  among what is assumed present, because nothing else in regied needs it; this one effect
  does, and a host without it gets exactly what it had before.
- A declaration that lists `loopback` and declares no `Interface` that could carry the
  entry gets a warning from `render` and `--dry-run`, in the sense ADR 0012 gives
  warnings: dnsmasq will answer on the loopback, and the host will not be told to ask it.
- What is still not covered: the host's resolver goes with the last addressed link. A
  router whose provider gives it nothing but a PPPoE session, and whose segment is empty,
  has no link resolved will use. Such a host is being reached over its console; as soon as
  it is reached over a link, that link carries the entry.
- Time synchronisation is out of scope. The failure that surfaced this was a clock
  drifting because timesyncd could not resolve its servers. With names resolving again,
  timesyncd's own defaults do the rest, and regied neither configures nor watches it.
- The golden rendering of `config/example.yaml` changes: every addressed link gains the
  two resolver lines, and the bridge gains `ConfigureWithoutCarrier=yes`.
- A bridge's address exists from the moment networkd creates the bridge. dnsmasq, which
  binds addresses as they appear, and the prefix-delegation assignment no longer wait for
  the first client.
