# Resource kinds

Eleven kinds. The document that contains them is described in
[`configuration.md`](configuration.md); why these eleven and not others is in
[ADR 0002](../adr/0002-configuration-schema.md).

日本語版は [docs/ja/spec/kinds.md](../ja/spec/kinds.md) にあります。

Throughout, a **link resource** means an `Interface`, a `PPPoESession` or a
`DSLiteTunnel` — the three kinds that put a link on the host. An **uplink** means a
`PPPoESession` or a `DSLiteTunnel`.

| Kind | Backend | Referred to by |
|---|---|---|
| [`Interface`](#interface) | systemd-networkd | `interfaceRef`, `underlayRef`, `linkRefs` |
| [`PPPoESession`](#pppoesession) | pppd | `egressRef`, `linkRefs` |
| [`DSLiteTunnel`](#dslitetunnel) | systemd-networkd | `egressRef`, `linkRefs` |
| [`EgressRoutePolicy`](#egressroutepolicy) | nftables + systemd-networkd | — |
| [`IPAddressSet`](#ipaddressset) | nftables | `addressSetRefs` |
| [`FirewallZone`](#firewallzone) | nftables | `from`, `to` |
| [`FirewallPolicy`](#firewallpolicy) | nftables | — |
| [`SourceNAT`](#sourcenat) | nftables | — |
| [`PortForward`](#portforward) | nftables | — |
| [`DHCPServer`](#dhcpserver) | dnsmasq | — |
| [`DNSForwarder`](#dnsforwarder) | dnsmasq | — |

---

## Interface

A link regied owns both ends of: a physical NIC, or a bridge over several of them. It
carries everything that is a property of that link — its addresses, its MTU, the static
routes that leave by it, whether it runs a prefix-delegation client, and whether it
advertises to the segment below it.

Backend: a `.network` file, and for a bridge a `.netdev` file, under regied's prefix in
`/etc/systemd/network/`.

| Field | Required | Value |
|---|---|---|
| `ifname` | yes | The kernel interface name. For a bridge, the name to create |
| `bridge.members` | no | Kernel interface names to enslave. Present means this is a bridge |
| `mtu` | no | Bytes. Defaults to the kernel's |
| `addresses` | no | List of addresses. See below |
| `routes` | no | List of static routes. See below |
| `dhcpv6` | no | DHCPv6 client settings. See below |
| `ipv6.advertise` | no | Router advertisement settings. See below |

`bridge.members` holds kernel interface names, not resource names. A member does not
need an `Interface` resource of its own — regied writes the file that enslaves it.
Declare a member as an `Interface` only to give it a property that is its own, such as an
MTU, and then it must not carry addresses. The addresses belong to the bridge, and so
does the name a `FirewallZone` names.

The bridge is created with spanning tree disabled, VLAN filtering disabled, and the
kernel's defaults for the remaining timers. There is no field for any of them, and the
omission is deliberate: joining several ports into one segment is all the schema claims
here. A bridge that has to run a spanning tree or carry tagged VLANs is a different
requirement, and it gets fields when there is a configuration that needs them rather than
in anticipation.

### `addresses[]`

Each entry is either a literal address with prefix length, or an address derived from a
delegated prefix.

| Field | Required | Value |
|---|---|---|
| *(string)* | — | A literal, e.g. `192.0.2.1/24` or `2001:db8::1/64` |
| `fromDelegatedPrefix.interfaceRef` | yes | The interface running the prefix-delegation client |
| `fromDelegatedPrefix.subnetID` | yes | Which subnet of the delegated prefix to take |
| `fromDelegatedPrefix.token` | no | The host part, e.g. `::1`. Defaults to the interface identifier |

A derived address changes when the delegation changes, and everything built on it —
the tunnel's local address, the advertised prefix, the addresses DNS listens on — changes
with it. That propagation is the reason the derivation is declared rather than the result
being written down.

### `routes[]`

| Field | Required | Value |
|---|---|---|
| `destination` | yes | CIDR. Family is inferred |
| `via` | no | Next hop address. Omitted means on-link through this interface |
| `metric` | no | Lower wins |

There is no table field. The only additional routing tables regied creates are the ones
an `EgressRoutePolicy` needs, and it fills them itself.

### `dhcpv6`

Set on the upstream interface, the one facing the provider.

| Field | Required | Value |
|---|---|---|
| `prefixDelegation.duidFile` | no | Path to a file holding the DUID to send. Some providers bind the delegation to it |
| `prefixDelegation.prefixLength` | yes | Prefix length to request, e.g. `56` |
| `prefixDelegation.rapidCommit` | no | Default `true` |
| `useDNS` | no | Default `false`. Whether to take resolvers from the provider |

The DUID file holds the DUID as colon-separated hex, type prefix included, exactly as it
appears in the configuration it was copied from. A DUID-LL is `00:03:00:01:` followed by
six octets — twenty-nine characters, for instance `00:03:00:01:00:00:5e:00:53:01`. regied
takes the leading two bytes as the DUID type and hands the two halves to networkd, which
wants them apart.

There is deliberately no `duidType` field. The value is normally copied whole out of the
configuration of the router being replaced, and a format that asks for it to be split by
hand creates a path where dropping one byte is accepted without complaint. What is lost
then is the delegated prefix, and the loss shows only after every IPv6 address on the
segment below has already changed.

Omitting `duidFile` is allowed and is not an error. networkd then sends a DUID of its own,
derived from the machine ID. For a line being brought up for the first time that is
right; for a host replacing one that already holds a delegation, it is the thing that
changes the delegated prefix, and nothing about it fails loudly.

The DUID is not a credential, and unlike one it is shown in `--dry-run` output, in a diff
and in the state API — the single exception in
[ADR 0003](../adr/0003-secrets-out-of-configuration.md), because the DUID in effect is
what answers the question of why a delegated prefix changed.

### `ipv6.advertise`

Set on a downstream interface. Router advertisement is systemd-networkd's, not dnsmasq's:
the prefix comes from delegation and networkd is already tracking it. Because this lives
on the interface, there is nowhere in the schema to ask for a second advertiser on the
same link.

| Field | Required | Value |
|---|---|---|
| `mode` | yes | `slaac` — advertise the prefix for stateless autoconfiguration |
| `otherInformation` | no | Default `false`. Set the O flag, telling clients to ask DHCPv6 for the rest |
| `dnsServers` | no | Advertised as RDNSS |
| `validLifetime` | no | Default `24h` |
| `preferredLifetime` | no | Default half of `validLifetime` |

The prefix advertised is the one this interface holds. It is not written here, so it
cannot drift from the address that is actually configured.

---

## PPPoESession

A PPPoE uplink. systemd-networkd has no PPPoE, so regied generates pppd's configuration
and supervises the process.

Backend: a pppd peer file, a secrets file with restrictive permissions, and a supervised
`pppd` process.

| Field | Required | Value |
|---|---|---|
| `interfaceRef` | yes | The `Interface` the session runs over |
| `userIDFile` | yes | Path to a file holding the account name. Treated as a credential |
| `passwordFile` | yes | Path to a file holding the password |
| `mtu` | no | Default `1492`, the most PPPoE over Ethernet allows |
| `persist` | no | Default `true`. Redial when the session drops |
| `holdoff` | no | Default `5s`. Wait before redialling |
| `useDNS` | no | Default `false`. Whether to take resolvers from the peer |
| `defaultRoute.install` | no | Default `true`. Install a default route in the main table |
| `defaultRoute.metric` | no | Default `0`. Raise it to make this uplink a standby |
| `routes` | no | As on `Interface` |

The link is named after `metadata.name`, so other resources and the firewall see a stable
name across redials.

`defaultRoute.metric` is how a host with two uplinks says which one its own traffic uses.
Traffic originating on the router is not subject to policy routing, so the metric is the
only thing that decides it.

Neither credential file appears in `--dry-run` output, in a diff, or in the state API
([ADR 0003](../adr/0003-secrets-out-of-configuration.md)).

---

## DSLiteTunnel

The IPv4-over-IPv6 uplink: the B4 side of RFC 6333. IPv4 packets are encapsulated in IPv6
and handed to the provider's AFTR, which performs the address translation.

Backend: a `.netdev` of kind `ip6tnl`, mode `ipip6`, and its `.network`.

| Field | Required | Value |
|---|---|---|
| `underlayRef` | yes | The `Interface` carrying the IPv6 that the tunnel runs over |
| `localAddressFrom.interfaceRef` | one of | Take the tunnel's local address from this interface's IPv6 address |
| `localAddress` | one of | A literal IPv6 address, for a statically addressed deployment |
| `aftrHost` | one of | The provider's AFTR by name. Resolved at apply time |
| `aftrAddress` | one of | The provider's AFTR as a literal IPv6 address |
| `mtu` | no | Default `1454` |
| `ttl` | no | Default `64` |
| `defaultRoute.install` | no | Default `true` |
| `defaultRoute.metric` | no | Default `0` |
| `routes` | no | As on `Interface` |

Exactly one of `localAddressFrom` and `localAddress` is required. `localAddressFrom` is
the one to use where the prefix comes from delegation: the tunnel then follows a prefix
change instead of going dark until somebody edits the file.

Exactly one of `aftrHost` and `aftrAddress` is required, in the same way as the pair
above, and `aftrHost` is the one to reach for. Providers publish a stable, well-known name
for their AFTR and treat the addresses behind it as theirs to change, so the name is the
official identifier and the address is an implementation detail. Writing the detail down
is the failure `PortForward` avoids by never accepting a listening address: it works until
the far side changes something it never promised to hold still, and then it stops without
saying so.

`aftrHost` is resolved once, at apply time, and the tunnel is created with the address
that comes back. regied does not watch the name and does not re-resolve it. An `ip6tnl`
link takes its remote at creation and networkd will not rebuild a tunnel on a DHCP event,
so following the name would mean regied watching the state it has already rendered and
rebuilding from what it sees — the structure [ADR 0009](../adr/0009-ownership-boundary.md)
avoids deliberately. When the address changes, the answer is another apply: chosen rather
than triggered, visible in `--dry-run` before it happens, and able to roll back.

`--dry-run` reports the address the name resolved to. It is not a secret, and what the
host is about to build a tunnel to is exactly what a diagnosis needs — the treatment the
DUID gets in [ADR 0003](../adr/0003-secrets-out-of-configuration.md).

**`aftrHost` has to resolve over IPv6.** The tunnel is what carries IPv4, so a resolver
reachable only over IPv4 cannot be asked anything until the tunnel is up, and a host with
no other resolver would need the tunnel in order to look up the name the tunnel is built
from. Whether a resolver is reachable over IPv6 is a property of the deployment and not
something the configuration states, so regied does not try to reject that loop statically.
It resolves at apply time, and the failure it reports names an IPv6-reachable resolver as
the thing to check.

There is no field for learning the AFTR from DHCPv6, and that is a decision rather than an
oversight. RFC 6334's option 64 carries an AFTR name, so what arrives is the same FQDN
`aftrHost` already takes and not an address: the resolution step, and everything above
about it, stays exactly where it is. Following the option as it changes runs into the
`ip6tnl` constraint again — the remote is fixed when the link is created, so regied would
have to watch and rebuild. And nothing here spends the option: a deployment that meets a
provider sending it gains one more source for `aftrHost`, which is an addition rather than
a change to what these fields mean.

**No `SourceNAT` belongs on this uplink.** The AFTR translates, so the inner source
address is left as it is. A masquerade here would translate twice. It also follows that
nothing can be published inbound through this uplink, and a `PortForward` naming it is a
validation error.

---

## EgressRoutePolicy

Which uplink a class of traffic leaves by. One resource covers both halves: the match
that identifies the traffic, and the routing that follows from it.

Backend: an nftables rule that sets a mark, plus a `[RoutingPolicyRule]` and a default
route in a dedicated table, both in systemd-networkd.

| Field | Required | Value |
|---|---|---|
| `family` | no | Default `ipv4` |
| `priority` | yes | Lower is evaluated first. Unique within a family |
| `egressRef` | yes | The uplink this traffic leaves by |
| `sourceRanges` | no | CIDRs, or inclusive ranges such as `192.0.2.130-192.0.2.255` |
| `sourceAddressSetRefs` | no | `IPAddressSet` names, as an alternative to writing ranges here |
| `excludeDestinations` | no | CIDRs that must not be sent to this uplink |
| `table` | no | Pin the routing table number instead of letting regied choose |
| `mark` | no | Pin the firewall mark |

At least one of `sourceRanges` and `sourceAddressSetRefs` must be present.

`sourceRanges` takes the two forms in the table above and no third one. A bare address is
a validation error: `192.0.2.1/32` says the same thing in one of the two forms already
there, and two spellings for one meaning is one more thing to be inconsistent about.

`excludeDestinations` is what keeps local traffic local. A policy that says "these hosts
go out by the PPPoE uplink" has to exclude the LAN itself, or traffic between two LAN
hosts would be sent to the uplink. It is also what makes hairpin work, though indirectly:
the mark is set after destination translation, so a LAN host reaching a published service
through the uplink's global address is already addressed to the internal host by then,
matches the exclusion, and stays local. Nothing has to know the global address.

`table` and `mark` exist for a host that shares its routing tables with something else.
Left out, regied assigns them and reports the assignment in `regied render`.

Declaring any `EgressRoutePolicy` requires `spec.global.sourceValidation` to be off.
Policy routing makes return paths asymmetric, and reverse path filtering drops exactly
that traffic.

---

## IPAddressSet

A named set of addresses or prefixes, so that a group of hosts can be written once and
referred to from several rules.

Backend: an nftables set inside regied's table.

| Field | Required | Value |
|---|---|---|
| `family` | yes | `ipv4` or `ipv6` |
| `addresses` | no | Individual addresses |
| `networks` | no | Prefixes |

At least one of the two lists must be non-empty. A set holding prefixes becomes an
interval set.

---

## FirewallZone

A named set of links. Zones are what firewall policies are written between.

Backend: an nftables set of interface names.

| Field | Required | Value |
|---|---|---|
| `linkRefs` | yes | Names of link resources — `Interface`, `PPPoESession`, `DSLiteTunnel` |

`wan` and `lan` are ordinary names for ordinary zones, not concepts the schema knows
([ADR 0009](../adr/0009-ownership-boundary.md)). A host that is not a router names its
zones after whatever it actually has.

The name `self` is reserved and cannot be used for a zone. It denotes the host itself.

---

## FirewallPolicy

The rules that apply to traffic travelling from one zone to another, or from a zone to
the host.

Backend: a chain in regied's nftables table, dispatched to from the hook chain by
interface set.

| Field | Required | Value |
|---|---|---|
| `from` | yes | A `FirewallZone` name |
| `to` | yes | A `FirewallZone` name, or `self` |
| `defaultAction` | yes | `accept`, `drop` or `reject` — for traffic no rule matched |
| `logDefault` | no | Defaults to `true` unless `defaultAction` is `accept` |
| `stateful` | no | Default `true`. See below |
| `rules` | no | Evaluated in order. First match wins |

The netfilter hook follows from the pair: `to: self` is input, zone to zone is forward.
There is no policy for output — nothing here filters what the host itself sends. `self` is
therefore a `to` and never a `from`, and a policy naming it as its `from` is a validation
error rather than a policy that quietly matches nothing.

**Traffic between a pair of zones with no policy is dropped.** A pair is not implicitly
open because nobody wrote it down.

**`stateful: true` puts the two rules every chain needs at the top**: accept established
and related, drop invalid. Writing them by hand in every policy is how one gets forgotten.

Only one policy may exist for a given `from` and `to`.

### `rules[]`

| Field | Required | Value |
|---|---|---|
| `name` | yes | For diagnosis. Appears in counters and log prefixes |
| `action` | yes | `accept`, `drop` or `reject` |
| `family` | no | `ipv4`, `ipv6`, or both when omitted |
| `protocol` | no | `tcp`, `udp`, `icmp`, `icmpv6`, `ipip`, `esp`, or a protocol number |
| `sourceCIDRs` | no | |
| `sourceAddressSetRefs` | no | `IPAddressSet` names |
| `sourcePorts` | no | Numbers, or ranges such as `60000-60010` |
| `destinationCIDRs` | no | |
| `destinationAddressSetRefs` | no | |
| `destinationPorts` | no | |
| `log` | no | Default `false` |

An address set whose family does not match the rule's is a validation error.

Two rules are worth knowing about because leaving them out breaks something silently.

- **`protocol: ipip` from the upstream zone to `self`** is what lets a DS-Lite tunnel
  come up at all: the encapsulated packets arrive addressed to this host.
- **`udp` destination port 546 from the upstream zone to `self`** is what lets the
  DHCPv6 client receive its replies, and therefore the delegated prefix.

---

## SourceNAT

Rewriting the source address of traffic on its way out.

Backend: a rule in regied's nftables postrouting chain.

| Field | Required | Value |
|---|---|---|
| `type` | no | `masquerade` is the only value, and the default |
| `egressRef` | yes | The uplink this applies to. The address follows that uplink |
| `sourceRanges` | no | CIDRs. Omitted means every source |
| `excludeDestinations` | no | CIDRs that must be left untranslated |

`masquerade` takes the address from the outgoing link at the moment the packet leaves, so
a dynamically addressed uplink needs nothing written down and nothing re-applied when the
address changes.

A `SourceNAT` on a `DSLiteTunnel` is a validation error: the AFTR already translates.

---

## PortForward

Rewriting the destination of traffic arriving on an uplink, so that a host inside can be
reached from outside.

Backend: a destination-NAT rule, a source-NAT rule for the hairpin case, and — unless
switched off — the firewall opening that lets the translated traffic through.

| Field | Required | Value |
|---|---|---|
| `egressRef` | yes | The uplink the traffic arrives on. The listening address follows it |
| `protocol` | yes | `tcp` or `udp` |
| `port` | one of | A single port |
| `portRange` | one of | An inclusive range, e.g. `60000-60010`, as one rule |
| `target.address` | yes | The host inside |
| `target.port` / `target.portRange` | no | Defaults to the same port or range |
| `hairpin` | no | Default `true` |
| `openFirewall` | no | Default `true` |

There is no field for the address to listen on. It is the uplink's, and on a dynamically
addressed uplink writing it down produces a configuration that works until the address
changes and then fails in a way that looks like something else. `egressRef` is the whole
of it.

**`hairpin`** covers a host on the inside reaching the service through the uplink's global
address — which is what happens when an internal client resolves the same public name as
an external one. Without it the reply is sent directly from target to client, bypassing
the translation, and the client discards it.

**`openFirewall`** adds the accept that matches this forward in the path the translated
traffic takes. It is on by default because a port forward with default-drop firewalling
and no opening is a configuration that is wrong in every case. Turn it off only to write
a narrower rule by hand.

**`protocol` takes those two names and nothing else.** A firewall rule accepts a protocol
number and this field does not, which is not an oversight: a forward translates a
transport port, so a protocol with no ports has nothing to forward. Writing `6` where
`tcp` was meant is refused rather than quietly accepted as the same thing.

**The target has to cover as many ports as the forward listens on.** Eleven ports onto
eleven is a forward; eleven ports onto two is a validation error. Which outside port would
land on which inside one is written nowhere in the second form — the kernel decides — so
the file would stop describing what the host does, which is the failure the rest of this
schema is arranged to avoid. Leaving `target.port` and `target.portRange` out keeps the
width by construction, and is the usual way to write it.

A `PortForward` whose `egressRef` names a `DSLiteTunnel` is a validation error: nothing
can be published through it.

---

## DHCPServer

Address handout on a downstream link.

Backend: dnsmasq. Every `DHCPServer` and `DNSForwarder` renders into one dnsmasq
configuration and one supervised process.

| Field | Required | Value |
|---|---|---|
| `interfaceRef` | yes | The link to serve |
| `subnet` | yes | CIDR the pool and the mappings sit in |
| `pool.start`, `pool.end` | yes | Inclusive. May cover only part of the subnet |
| `leaseTime` | no | Default `24h` |
| `gateway` | no | Defaults to the interface's own address |
| `dnsServers` | no | Defaults to the interface's own address |
| `domain` | no | Search domain handed to clients |
| `staticMappings` | no | See below |
| `ipv6` | no | Stateless DHCPv6. See below |

A pool narrower than the subnet is the usual arrangement: the rest of the subnet is left
for static mappings and for addresses assigned by hand.

### `staticMappings[]`

| Field | Required | Value |
|---|---|---|
| `name` | yes | Hostname handed out and registered |
| `macAddress` | yes | |
| `address` | yes | Must be inside `subnet`, and outside `pool` |

Where policy routing selects an uplink by source range, which side of that boundary a
static mapping falls on decides which uplink that host uses. Moving a mapping across the
boundary is a routing change, not a cosmetic one.

### `ipv6`

For the stateless half of IPv6 configuration. The prefix and the router advertisement
come from the interface ([`Interface.ipv6.advertise`](#ipv6advertise)); what is here is
what a client asks DHCPv6 for after the advertisement told it to.

| Field | Required | Value |
|---|---|---|
| `mode` | yes | `stateless` — answer information requests, hand out no addresses |
| `dnsServers` | no | |
| `informationRefreshTime` | no | Default `6h` |

Setting this without `otherInformation` on the interface's advertisement is a validation
warning: nothing would ever ask.

---

## DNSForwarder

Recursive resolution for the segments below, with conditional forwarding and name
overrides.

Backend: dnsmasq, merged with every `DHCPServer` into one configuration.

| Field | Required | Value |
|---|---|---|
| `listenOn` | yes | Link resource names, plus the reserved name `loopback` |
| `cacheSize` | no | Default `150` entries |
| `upstreams` | yes | Resolver addresses, IPv4 or IPv6 |
| `conditional` | no | See below |
| `staticHosts` | no | See below |

`listenOn` names links, not addresses, so an interface whose address comes from a
delegated prefix keeps being listened on after the prefix changes.

**A host has at most one `DNSForwarder`, and a second one is a validation error.**
dnsmasq is one process with one cache and one set of upstreams, so two resources could
only be merged into them, and what the merge decided — whose cache size won, which
upstreams the other's clients ended up asking — would be written nowhere. One resource
holds everything the kind can express: `listenOn`, `upstreams`, `conditional` and
`staticHosts` are all lists, so a host resolving for several segments names them all
here.

### `conditional[]`

| Field | Required | Value |
|---|---|---|
| `domain` | yes | The zone to divert |
| `servers` | yes | Where to ask instead of the upstreams |

This is how an internal zone — a cluster's service domain, for instance — is resolved by
the resolver that knows about it, while everything else goes upstream.

### `staticHosts[]`

| Field | Required | Value |
|---|---|---|
| `name` | yes | A single fully qualified name |
| `address` | yes | What to answer with |

An override for one name. The usual reason is a public name that should resolve to an
internal address for clients inside, so that they reach the service directly rather than
through the uplink and back.
