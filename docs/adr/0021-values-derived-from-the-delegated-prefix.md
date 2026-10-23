# ADR 0021: Resolve values derived from the delegated prefix on every turn

- Status: Accepted (2026-10-23)
- Extends [ADR 0015](0015-uplink-addresses-in-sets.md) and
  [ADR 0016](0016-converging-on-the-accepted-declaration.md)
- First of a series: [ADR 0022](0022-dslite-local-address-follows-the-delegation.md),
  [ADR 0023](0023-downstream-hosts-derive-their-subnet.md),
  [ADR 0024](0024-dns-records-inside-the-delegation.md),
  [ADR 0025](0025-resolicit-after-the-pppoe-session-returns.md),
  [ADR 0026](0026-no-uplink-failover-inside-regied.md)

## Context

### What a prefix change breaks

A delegated prefix is borrowed. It has a lifetime, and it changes when the provider wants
it to. Keeping the DUID and the IAID makes the same provider router hand back the same
prefix ([ADR 0012](0012-networkd-rendering.md)), but it promises nothing once the
provider moves the line to another router: the DUID and the IAID are unchanged, and a
different prefix comes back.

When that happens, everything in the declaration that was written as a *derivation* —
`fromDelegatedPrefix` on a downstream interface, `fromRouterAdvertisement` on a host
behind it — follows on its own, because networkd recomputes it. Everything that was
written as a *literal* inside the prefix goes on naming the old one. On a router shaped
like the worked example, that is:

| What breaks | How it is written today | Why it cannot be written another way |
|---|---|---|
| The DS-Lite tunnel's local address | `DSLiteTunnel.localAddress`, a literal | `localAddressFrom` renders as the underlay's own address on systemd 257 ([ADR 0011](0011-target-platform.md)), and an upstream interface that only receives a delegation has no global address of its own |
| A static route to a subnet handed to a host behind the router, and its next hop | `Interface.routes[].destination` and `via`, literals | No derived form exists |
| An address set naming a published host, or a subnet behind the router | `IPAddressSet.addresses` and `networks`, literals | No derived form exists |
| The subnet a downstream host gives its own bridge, and advertises | A literal on the other host's `Interface` | The other host does not know the delegated prefix ([ADR 0023](0023-downstream-hosts-derive-their-subnet.md)) |
| The AAAA record of a published host | Not expressible; `DNSRecordSet` holds A records only | Left to a later record by [ADR 0019](0019-uplink-address-in-dns-records.md) ([ADR 0024](0024-dns-records-inside-the-delegation.md)) |

The tunnel is the worst of them. It is what carries the router's IPv4, and a local
address the host no longer holds leaves the link up and every IPv4 packet dropped:
nothing reports an error, and every convergence check says the host is at its
declaration.

### What the delegated prefix is, to regied

It is a runtime value, in exactly the sense [ADR 0016](0016-converging-on-the-accepted-declaration.md)
uses the word: something the declaration cannot hold, that only this host can answer, and
that may be missing now and present on the next turn. It sits beside the three values a
turn already collects — the address an AFTR name resolves to, the contents of a DUID file,
the addresses each uplink link is holding.

It is closest to the third. [ADR 0015](0015-uplink-addresses-in-sets.md) found that an
uplink's address is runtime state the ruleset was treating as configuration, and moved it
into a set that a turn fills from the kernel. The delegated prefix is the same kind of
fact one level up: not an address a link holds, but the block every derived address is
cut from.

### What regied must not become

[ADR 0011](0011-target-platform.md) said that if deriving the tunnel's local address from
the delegation turned out to be necessary, that would be "a reason to revisit the
platform, not a reason to build the address lifecycle ourselves". The incident above makes
it necessary, and the platform has not moved. So what this record decides has to stay on
the near side of that line: **regied computes values from the prefix; networkd keeps the
lease, the renewals, the expiry and the addresses it assigns.** No address appears on a
link because regied counted a lifetime down.

[ADR 0009](0009-ownership-boundary.md), and the argument the `aftrHost` field makes from
it, rule out the other way this could go wrong: regied watching what it has rendered and
rebuilding from what it sees.

## Decision

### The delegated prefix is a runtime value, read from networkd on every turn

**For every `Interface` that runs a prefix-delegation client, a turn reads the prefix that
client currently holds, from networkd's own description of the link.** networkd publishes
it in the description `networkctl status --json` prints and its D-Bus `Describe` call
returns: in systemd 257 the link's `DHCPv6Client` object carries `Prefixes`, each with the
prefix, its length and its preferred and valid lifetimes. What it reports on a real line is
the first thing the first implementation unit confirms.

Three sources were weighed.

| Source | Verdict |
|---|---|
| networkd's description of the link | **Chosen.** It is the party holding the lease, it describes the delegation itself rather than something derived from it, and it is what networkd will act on |
| The kernel address of a link whose address is `fromDelegatedPrefix`, masked back to the delegation's length | Rejected as the primary source. It reads back an address networkd placed *because of a file regied wrote*, which is the shape ADR 0009 avoids; it needs a derived address to exist somewhere; and it guesses the delegation's length from the request, while a provider may delegate a different length from the one asked for |
| networkd's files under `/run` | Rejected. They are networkd's private state, not an interface, and building on them is building on something it never promised |

This is not ADR 0009's avoided structure. What regied reads is not an artifact it
rendered, and not an address networkd derived from one: it is what the provider
delegated, which no file regied writes can change. It is an input, the same as an AFTR
name's answer.

**What wakes the turn is already there.** A changed delegation makes networkd reassign
every address it derives from it, and an address event wakes a turn
([ADR 0016](0016-converging-on-the-accepted-declaration.md)). The subscription is widened
from the uplinks' links to every link, so that a reassigned downstream address wakes a
turn as an uplink's address does. The periodic resync remains the floor; nothing else is
watched.

### One vocabulary: `fromDelegatedPrefix` wherever an address inside the prefix is written

**The derivation the schema already has for an interface's address becomes the way to
write an address or a subnet inside the delegated prefix anywhere the deployment needs
one.** It is the same three fields with the same meaning:

| Field | Meaning |
|---|---|
| `interfaceRef` | The interface through which this host knows the delegated prefix — on a router, the one running the prefix-delegation client |
| `subnetID` | Which subnet of the delegation |
| `token` | The host part. Present: an address. Absent where a subnet is written: the subnet as a /64 |

The places it is accepted, beside the literal they replace:

| Field | Today | Proposed |
|---|---|---|
| `Interface.addresses[]` | Literal, `fromDelegatedPrefix`, `fromRouterAdvertisement` | Unchanged |
| `Interface.routes[].destination` | CIDR | CIDR, or `fromDelegatedPrefix` without a token, which is the /64 |
| `Interface.routes[].via` | Address | Address, or `fromDelegatedPrefix` with a token |
| `IPAddressSet.addresses[]` | Address | Address, or `fromDelegatedPrefix` with a token |
| `IPAddressSet.networks[]` | CIDR | CIDR, or `fromDelegatedPrefix` without a token |
| `DSLiteTunnel.localAddressFrom` | Interface reference | [ADR 0022](0022-dslite-local-address-follows-the-delegation.md) |
| `DNSRecordSet.records[]` | `egressRef` | [ADR 0024](0024-dns-records-inside-the-delegation.md) |

A token is required in `via` and in `addresses[]` and refused in `destination` and
`networks[]`, so a field that wants an address cannot be given a subnet. A subnet is always
a /64: that is what a delegated subnet is handed out as, and a field for another length
waits for a configuration that needs one ([ADR 0002](0002-configuration-schema.md)). A
`subnetID` that does not fit in the delegation the provider actually handed out is not a
validation error — the length is not known until the turn — and resolves to nothing, with
the reason, as below.

**Nothing else gains the form now.** `FirewallPolicy.rules[].destinationCIDRs`,
`DNSForwarder.staticHosts[].address`, the resolvers in `ipv6.advertise.dnsServers` and in
`DHCPServer.ipv6`, and an IPv6 `EgressRoutePolicy`'s source ranges keep their literals. A
rule that has to name a host inside the prefix names an `IPAddressSet`, which is what the
set is for; the others have not been asked for.

### How a derived value reaches the host

A turn resolves every `fromDelegatedPrefix` it can, and each consumer takes the result the
way it already takes its runtime values.

| Consumer | Where the resolved value goes |
|---|---|
| An address on the router's own interface | Unchanged: networkd derives it from `[DHCPPrefixDelegation]`, and regied renders the derivation, not the address |
| A route's destination or next hop | Rendered into the `.network` file as a literal. The file changes when the prefix does, and the turn writes it and reloads networkd, as for any changed file |
| An `IPAddressSet` element | **Not rendered into the ruleset.** The set is declared in the table, and its elements are written by the turn in the way [ADR 0015](0015-uplink-addresses-in-sets.md) writes an uplink's set |
| The tunnel's local address | [ADR 0022](0022-dslite-local-address-follows-the-delegation.md) |
| A DNS record | [ADR 0024](0024-dns-records-inside-the-delegation.md) |

The split between the files and the ruleset follows what each already is. The networkd
files are a function of the declaration *and* of runtime values — an AFTR address, a DUID
— and always were ([ADR 0012](0012-networkd-rendering.md)). The ruleset is a function of
the declaration alone, and ADR 0015 is what made it so; putting a prefix into its text
would undo that for the same reasons that record gives. **A set with at least one derived
entry has all its elements written by the turn**, literal ones included, so that one set is
never filled from two places.

`regied render` and `--dry-run` show the prefix that was read, and every value resolved from
it, beside the files and the set elements a turn would write. Neither is a secret, and the
prefix in effect is the first thing a diagnosis needs — the treatment the DUID and the
AFTR address already get ([ADR 0003](0003-secrets-out-of-configuration.md)).

### While there is no delegation

On a host that has just booted, on a line whose lease has expired, or while the client is
still soliciting, there is no prefix to resolve against. The rule splits by what kind of
thing holds the value.

**An entry is absent while there is no delegation.** A route whose destination is a
delegated subnet, a set element inside the prefix: each is rendered without that entry,
in the way networkd itself has no derived address while there is no delegation. A file or a
set without the entry is not a degraded version of the declaration — it is exactly what the
declaration means at that moment, because the subnet it names does not exist. This is not
the case [ADR 0016](0016-converging-on-the-accepted-declaration.md) warns about, where a
prefix delegation written without its DUID is *a different configuration*: a route to a
subnet that is not there is no configuration at all.

**An artifact that cannot exist without the value is left exactly as it is.** The tunnel,
whose local address is part of what the link is, and a record at a DNS provider, which is
not on this host, are both left alone — the previous tunnel stays as it was, the record
keeps what it held — and the turn says what it is waiting for. That is the AFTR rule and
ADR 0019's rule, unchanged.

Either way the turn ends **waiting**, naming the interface whose delegation it is waiting
for. More than one delegated prefix at once, or a `subnetID` that does not fit in the one
delivered, is reported in the same way, as what stops the value from resolving.

## Alternatives considered

**Teaching each consumer to read the prefix itself.** The tunnel renderer, the route
renderer, the set writer and the DNS writer would each look the prefix up. Rejected: four
readings of one fact, possibly taken at four moments, can disagree within one turn. A turn
reads it once, and everything it renders agrees.

**Rendering the prefix into the ruleset text.** Simpler for the sets than the runtime
write. Rejected for ADR 0015's reasons, which apply word for word: the text would stop
being a function of the declaration, and the same declaration would render differently
before and after a lease.

**A new kind holding the delegation, for every derivation to refer to.** It would give
"the delegated prefix" a name. Rejected by ADR 0002's tests: the client is already a
property of the upstream interface, and an `interfaceRef` already names it. A kind whose
only content is a pointer to another resource is the shape ADR 0002 folds away.

**Waiting for systemd 258.** It covers the tunnel's local address on the router and
nothing else in the table above, and it is not on the platform
([ADR 0011](0011-target-platform.md)).

**Leaving it to the configuration repository's tooling** — substituting the prefix into
the declaration before submitting it. It needs somebody to notice the change, run the
tooling and submit, which is the manual step this record is about removing; and every
submission is then a different declaration for the same intent.

## Consequences

- **The delegated prefix joins the runtime values a turn collects.** Its absence is
  never a failure of the turn, only a reason to wait, and the report names the interface it
  is waiting on.
- **The files networkd reads change when the prefix does**, and a turn writes them and
  reloads, as it would for any other runtime value that changed.
- **An `IPAddressSet` with a derived entry has its elements written at runtime.** The
  ruleset text stays a function of the declaration; the set's contents become what the
  dry-run shows beside it, as an uplink's set already is.
- **Address events on every link wake a turn**, not only on the uplinks'. A turn that
  finds nothing to do runs no command, so the cost is the comparison.
- **The schema gains no kind.** It gains one form, already known, in four more places.
- **regied still owns no address lifecycle.** networkd keeps the lease and the addresses;
  regied computes values from what networkd says it holds. The one place regied writes an
  address that networkd would not otherwise have is the downstream host of
  [ADR 0023](0023-downstream-hosts-derive-their-subnet.md), and that record argues for it.

## Implementation units

The series lands in this order. Each unit is one pull request, with tests first, and the
spec in `docs/spec/` changed in the same pull request as the code.

| # | Unit | Records |
|---|---|---|
| 1 | Read the delegated prefix from networkd, resolve `fromDelegatedPrefix` in a turn, show both in `render` and `--dry-run`, widen the address subscription. Lands together with unit 2, which is its first consumer | 0021, 0022 |
| 2 | The tunnel's local address follows the delegation | 0022 |
| 3 | Derived route destinations and next hops; derived `IPAddressSet` elements written at runtime | 0021 |
| 4 | AAAA records kept by the router, inside its delegation | 0024 |
| 5 | A downstream host derives its subnet from the advertised prefix | 0023 |
| 6 | AAAA records kept by a downstream host, for itself and its containers | 0024 |
| 7 | The netns testbed gains a DHCPv6 server that delegates prefixes, so that a prefix change can be reproduced end to end. Precedes unit 8 | 0021, 0025 |
| 8 | Re-soliciting after the PPPoE session returns from a different upstream | 0025 |

Units 1 and 2 are first because the tunnel is what takes the router's IPv4 down. Unit 5
comes after 3 because a downstream host's subnet is only worth deriving once the router's
route to it follows the prefix too; deriving one side alone is two places again. Unit 6
comes after 5 because the downstream host keeps its own records
([ADR 0024](0024-dns-records-inside-the-delegation.md)), and it needs its prefix to do so.
