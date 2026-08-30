# ADR 0002: The shape of the configuration schema

- Status: Accepted (2026-08-30)
- Supersedes the provisional kind list drawn up before ADR 0008

## Context

The first list of resource kinds was drawn up on the assumption that regied would
implement every layer itself. ADR 0008 changed that assumption: interfaces, addresses,
routes, routing policy rules, the ip6tnl tunnel, prefix delegation and router
advertisement are all things systemd-networkd already models declaratively, and regied
hands them over rather than implementing them.

That leaves a question the old list cannot answer. If networkd owns those layers, what
does the operator write?

Two answers are wrong in opposite directions.

- **Too thick.** Keep one kind per networkd concept. The configuration becomes networkd's
  vocabulary with different punctuation, and the operator has to know both.
- **Too thin.** Drop those layers from the schema and tell the operator to write
  `.network` files alongside. Then there is no single declaration, which was the point.

The schema has to be regied's own vocabulary, and it has to fall out into networkd,
nftables, dnsmasq and pppd without the operator seeing the seam.

## Decision

### The document

One YAML document. `apiVersion: net.unstable.cloud/v1alpha1`, `kind: NetworkConfig`,
`spec.global` for host-wide switches, and `spec.resources[]` listing resources that each
carry `kind`, `metadata.name` and `spec`. The apiGroup is deliberately not derived from
the project name, so renaming the binary never invalidates a configuration file.

`NetworkConfig` names what the document holds, not what the host is. `Router` was the
provisional name and it was wrong in two directions. It contradicts ADR 0009, which keeps
role names such as `wan` and `lan` out of the type system — the outermost type is the last
place to break that rule. And it does not describe a host that carries only a firewall,
which the schema already accepts: no resource kind is mandatory, so a document holding
zones and policies and nothing else is valid.

`NodeConfig` was considered and rejected for claiming more than regied owns. Hostname,
timezone, accounts and packages are all a node's configuration and all deliberately out of
scope ([`docs/scope.md`](../scope.md)). What this document holds is network configuration
and nothing else, down to the seven kernel switches in `spec.global`.

Resources refer to each other by `<kind in lower camel case>Ref` holding another
resource's `metadata.name`. A reference is resolved against the kind the field expects,
so the kind is not repeated in the value.

### What becomes a kind

A resource kind exists when at least one of these is true.

1. **Something else refers to it by name.** An address set is named by firewall rules; a
   zone is named by policies; an uplink is named by NAT and by policy routing.
2. **It has a lifecycle of its own** — a process to supervise, a link that appears and
   disappears, a lease database.
3. **It repeats, and the entries are meaningful on their own** rather than being
   properties of exactly one parent.

Everything else is a field on the resource that owns it. The test that settles most cases
is the first: a thing nobody names does not need a name.

### The kinds

| # | Kind | What it is | Backend |
|---|---|---|---|
| 1 | `Interface` | A link regied owns both ends of: physical NIC or bridge, with its addresses, MTU, static routes, prefix-delegation client, and router advertisement | systemd-networkd |
| 2 | `PPPoESession` | A PPPoE uplink | pppd |
| 3 | `DSLiteTunnel` | The IPv4-over-IPv6 uplink (RFC 6333 B4) | systemd-networkd |
| 4 | `EgressRoutePolicy` | Which uplink a class of traffic leaves by | nftables + systemd-networkd |
| 5 | `IPAddressSet` | A named set of addresses or prefixes | nftables |
| 6 | `FirewallZone` | A named set of interfaces | nftables |
| 7 | `FirewallPolicy` | The rules that apply between two zones | nftables |
| 8 | `SourceNAT` | Source address translation on the way out | nftables |
| 9 | `PortForward` | Destination translation on the way in, with hairpin | nftables |
| 10 | `DHCPServer` | Address handout on a downstream link | dnsmasq |
| 11 | `DNSForwarder` | Recursive resolution, conditional forwarding, name overrides | dnsmasq |

Eleven, from the fourteen of the earlier list. The field-level detail is in
[`docs/spec/`](../spec/).

### What was folded away, and why

**`StaticRoute` → `Interface.spec.routes[]`.** networkd has no global route file; a route
lives in the `.network` of the link it leaves by. Making the operator name that link is
honest, and a route is never referred to by name.

**`DHCPv6PrefixDelegation` → `Interface`.** The client is a property of the upstream link,
and each derived address is a property of the downstream link that carries it. Splitting
it out would create a resource whose only purpose is to be pointed at from both sides.

**`IPv6RouterAdvertisement` → `Interface.spec.ipv6.advertise`.** One link advertises or it
does not, and what it advertises is derived from the addresses that link already has.
Keeping it on the interface also makes a second advertiser structurally impossible: there
is nowhere else in the schema to ask for one.

**`SystemSettings` — dropped.** Hostname, timezone and NTP are the operating system's, and
[`docs/scope.md`](../scope.md) already said so. A kind that exists to restate what
another tool owns is worse than no kind.

**`spec.global.allPing` — dropped.** Accepting an echo request is a firewall rule,
and the firewall is regied's own model. A global switch that secretly writes a rule gives
the firewall two entry points.

### What was added and renamed

**`FirewallZone` split out of `FirewallPolicy`.** ADR 0009 requires that `wan` and `lan`
not be proper nouns in the type system. As long as zones were a map inside the policy,
the policy's default actions had to be spelled `wanToLan`, `lanToSelf`, and so on — the
names were in the schema. With zones as their own kind, a policy names a `from` and a
`to`, and the names are data. `self` is the one reserved value: it denotes the host, and
the netfilter hook follows from the pair — `to: self` is input, zone to zone is forward.
There is no way to write a policy for output, because nothing in the target configuration
filters what the host itself sends.

**`NAT44Rule` → `SourceNAT`.** The address family belongs in a field, not in the name.

### What the operator does not write

Two things the earlier list would have exposed are derived instead.

**Routing table numbers and firewall marks.** An `EgressRoutePolicy` names a class of
traffic and an uplink. regied allocates the table and the mark, installs the default route
into that table, emits the `[RoutingPolicyRule]` that maps mark to table, and emits the
nftables rule that sets the mark. The numbers are visible in `regied render` and can be
pinned, but nothing outside regied should depend on them.

**The place where marks are set.** The chain that sets policy-routing marks runs **after**
nat prerouting, so it matches on the destination address as already rewritten by DNAT.
Hairpin traffic — a LAN host reaching a published service through the uplink's own global
address — is rewritten to the internal address before the mark is considered, falls into
the policy's own local-destination exclusion, and is routed locally. This is why the
schema has no way to write the uplink's global address anywhere: `PortForward` and
`SourceNAT` take a reference to an uplink resource, never a literal address. A schema that
allowed the address would invite a configuration that breaks when the address changes.

## Consequences

- The networkd-facing part of the schema is three kinds, not seven, and none of them
  requires knowing networkd's syntax.
- Adding a networkd capability later is usually a field on `Interface`, not a new kind.
- Two kinds (`DHCPServer`, `DNSForwarder`) render into one dnsmasq process. The apply
  model has to merge them, and a validation error in either fails both.
- Because table numbers and marks are derived, a policy-routing change is one resource
  edit rather than a coordinated edit across three.
- Router advertisement is networkd's and DHCPv6 option serving is dnsmasq's. Both are
  driven from the same declaration, but they are two backends and the split must be kept
  visible in the spec, or an operator will not know which one to look at when it breaks.
