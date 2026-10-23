# Resource kinds

Twelve kinds. The document that contains them is described in
[`configuration.md`](configuration.md); why these and not others is in
[ADR 0002](../adr/0002-configuration-schema.md), which settled the first eleven, and in
[ADR 0019](../adr/0019-uplink-address-in-dns-records.md) for the twelfth.

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
| [`DNSRecordSet`](#dnsrecordset) | the provider's HTTP API | — |

---

## Interface

A link regied owns both ends of: a physical NIC, or a bridge over several of them. It
carries everything that is a property of that link — its addresses, its MTU, the static
routes that leave by it, whether it runs a prefix-delegation client, and whether it
advertises to the segment below it.

Backend: a `.network` file, for a bridge a `.netdev` file, and for a link with
`wakeOnLan` a `.link` file, under regied's prefix in `/etc/systemd/network/`.

| Field | Required | Value |
|---|---|---|
| `ifname` | yes | The kernel interface name. For a bridge, the name to create |
| `bridge.members` | no | Kernel interface names to enslave. `bridge` present, even empty, means this is a bridge |
| `mtu` | no | Bytes. Defaults to the kernel's |
| `wakeOnLan` | no | The NIC's Wake-on-LAN mode. See below |
| `addresses` | no | List of addresses. See below |
| `routes` | no | List of static routes. See below |
| `dhcpv6` | no | DHCPv6 client settings. See below |
| `ipv6.advertise` | no | Router advertisement settings. See below |
| `ipv6.advertisedDelegation` | no | On a host behind a router: the delegation the prefix advertised to this link belongs to. See below |

`bridge.members` holds kernel interface names, not resource names. A member does not
need an `Interface` resource of its own — regied writes the file that enslaves it.
Declare a member as an `Interface` only to give it a property that is its own, such as an
MTU or `wakeOnLan`, and then it must not carry addresses. The addresses belong to the bridge, and so
does the name a `FirewallZone` names.

A bridge may name no member at all: `bridge: {}`, or `bridge.members` left empty. That is
a segment whose ports something else attaches — a container runtime plugging a veth in
for each container and removing it when the container goes. There is no port to name, but
the segment's addresses, the advertisement on it, and the zone it belongs to are still
this host's to declare, and declaring them here keeps its prefix in one place rather than
in the runtime's configuration as well. Such a bridge is the same bridge: regied writes
its `.netdev` and its `.network`, and only the files that would enslave members are
absent.

`ifname`, and every name in `bridge.members`, is a name the kernel has to hold: at most
15 characters, because `IFNAMSIZ` is 16 and the last byte is the terminator. A longer one
is a validation error rather than something the kernel refuses partway through an apply.

The bridge is created with spanning tree disabled, VLAN filtering disabled, and the
kernel's defaults for the remaining timers. There is no field for any of them, and the
omission is deliberate: joining several ports into one segment is all the schema claims
here. A bridge that has to run a spanning tree or carry tagged VLANs is a different
requirement, and it gets fields when there is a configuration that needs them rather than
in anticipation.

A bridge is configured whether or not a member has link. It holds its addresses, and
everything that hangs off them, with no client plugged in, and keeps them when the last
one is unplugged. networkd's default of waiting for carrier is right for a physical port
and wrong for a bridge, where no carrier means no client yet
([ADR 0018](../adr/0018-loopback-forwarder-is-the-host-resolver.md)).

### `wakeOnLan`

The mode the NIC wakes the host in: one of the values below, or a list of them.

| Value | Wakes on |
|---|---|
| `off` | Nothing. Wake-on-LAN is turned off |
| `phy` | PHY activity |
| `unicast` | Unicast frames |
| `multicast` | Multicast frames |
| `broadcast` | Broadcast frames |
| `arp` | ARP |
| `magic` | The magic packet |
| `secureon` | The magic packet with the SecureOn password |

**Omitted means regied leaves the NIC as it is.** It is not the same as `off`, and it does not
turn off what another tool, or the firmware, set. A list holds each value at most once, and
`off` stands alone: turning Wake-on-LAN off and waking on something are not one mode. An empty
list is an error; to turn it off, write `off`. There is no field for the SecureOn password, so
`secureon` leaves the NIC with whatever password it already holds, and is warned about.

It is a property of a physical link. A bridge has no NIC to wake, and setting it there is a
validation error. When the link is a bridge over a NIC, the setting belongs to the member:
declare the member as an `Interface` of its own, with `ifname` and `wakeOnLan` and no
addresses. regied writes the member's `.link` file beside the `.network` that enslaves it, and
being enslaved does not change how udev applies it.

The value goes in a `.link` file whose `[Match]` names `ifname`, and udev, not networkd,
applies it — when the device appears at boot, and on an apply to a link that is already up.
Removing the field takes the file away but does not turn Wake-on-LAN off: the NIC keeps its
mode until the device is added again, typically at the next boot. What the file matches on and
why is in [ADR 0020](../adr/0020-link-settings-applied-by-udev.md).

### `addresses[]`

Each entry is a literal address with prefix length, an address derived from a delegated
prefix, or an address derived from the prefix a router advertises on this link. A
mapping names exactly one of `fromDelegatedPrefix` and `fromRouterAdvertisement`.

| Field | Required | Value |
|---|---|---|
| *(string)* | — | A literal, e.g. `192.0.2.1/24` or `2001:db8::1/64` |
| `fromDelegatedPrefix.interfaceRef` | yes | The interface running the prefix-delegation client, or, on a host behind a router, the one declaring [`ipv6.advertisedDelegation`](#ipv6advertiseddelegation) |
| `fromDelegatedPrefix.subnetID` | yes | Which subnet of the delegated prefix to take |
| `fromDelegatedPrefix.token` | no | The host part, e.g. `::1`. Defaults to the interface identifier. Required when `interfaceRef` declares `ipv6.advertisedDelegation` |
| `fromRouterAdvertisement.token` | yes | The host part, e.g. `::153`. The upper 64 bits must be zero, and it must not be `::` |

A derived address changes when the delegation changes, and everything built on it —
the tunnel's local address, the advertised prefix, the addresses DNS listens on — changes
with it. That propagation is the reason the derivation is declared rather than the result
being written down.

`fromRouterAdvertisement` is the same idea for a host that is not the router. It takes
the prefix from the advertisement received on the link and keeps only the part the host
chose, the token, so the prefix the router advertises is not written down a second time.
The link accepts router advertisements, and so also takes the default route the
advertisement carries and, when its other-configuration flag is set, asks DHCPv6 for the
rest, as networkd does by default. There are no fields for either until a configuration
needs them.

An Interface holds at most one `fromRouterAdvertisement`; a second is a validation error.
networkd takes one token per link, and the address's source is meant to be readable in
one place. It may sit next to a literal address, typically an IPv4 one. The address it
yields is not advertised onward by `ipv6.advertise`, which advertises only literal
prefixes and the delegated one.

### `routes[]`

| Field | Required | Value |
|---|---|---|
| `destination` | yes | CIDR, or `fromDelegatedPrefix` without a `token`, which is a /64 of the delegation. Family is inferred |
| `via` | no | Next hop address, or `fromDelegatedPrefix` with a `token`. Omitted means on-link through this interface |
| `metric` | no | Lower wins |

`fromDelegatedPrefix` takes the same `interfaceRef` and `subnetID` as in `addresses[]`, and
both are required. Where a subnet is wanted — `destination` — the `token` is refused; where an
address is wanted — `via` — it is required, and is the host part. A route to a subnet handed
to a host behind the router, through that host's address, is then written once and follows a
prefix change ([ADR 0021](../adr/0021-values-derived-from-the-delegated-prefix.md)).

Every turn resolves the derived ends from the prefix the delegation client holds now, and
writes them into the `.network` file as literals. When the provider delegates another prefix,
the turn that follows rewrites the file and reloads networkd. While no delegation is held, or
the `subnetID` does not fit in the one delivered, the subnet the route names does not exist:
the route is left out of the file, the rest of the file is written as declared, and the turn
reports that it is waiting, naming the interface. `regied render` and `--dry-run` show each
route resolved, with the delegation it was cut from.

Only an `Interface`'s routes take the derived form. A route on an uplink leads outward, and
nothing outward is inside the prefix delegated to this host.

There is no table field. The only additional routing tables regied creates are the ones
an `EgressRoutePolicy` needs and the one per uplink a `PortForward` sends its replies
back by, and it fills them itself.

### `dhcpv6`

Set on the upstream interface, the one facing the provider. With `prefixDelegation` set, the client
asks for the prefix whatever the provider's router advertisement says: whether one arrives
at all, and whether it carries the managed or other-configuration flag. It asks for an
address (IA_NA) together with the prefix (IA_PD): some providers never answer a request
that carries the prefix alone. The usual reply holds no address, only the prefix. If the
provider does hand one out, it lands on that upstream interface.

| Field | Required | Value |
|---|---|---|
| `prefixDelegation.duidFile` | no | Path to a file holding the DUID to send. Some providers bind the delegation to it |
| `prefixDelegation.iaid` | no | The IAID to send with the prefix request. Some providers bind the delegation to the DUID and the IAID as a pair and never answer a request carrying a different IAID; carry the value over from the configuration of the router being replaced. Left out, networkd sends a value derived from the interface |
| `prefixDelegation.prefixLength` | yes | Prefix length to request, e.g. `56` |
| `prefixDelegation.rapidCommit` | no | Default `true` |
| `prefixDelegation.sendRelease` | no | Default `false`. Whether to give the delegation back to the provider when the client stops, as on a restart of networkd or a reboot |
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

The same holds for `iaid`, with one difference in how the loss shows. A provider that
binds the delegation to the DUID and the IAID together does not delegate a different
prefix to a request with the wrong IAID: it stays silent, and the client keeps asking. The
router being replaced usually sent `0`, and `0` is a declared value, distinct from leaving
the field out. Omitting it hands the choice to networkd, which derives the IAID from the
interface, so it never matches the replaced router's by accident.

`sendRelease` defaults to `false`, the opposite of networkd's own default. A client that
gives the delegation back each time it stops does so on every planned restart, and the
provider is then free to delegate a different prefix on the next request. Holding on to
it does not promise the same prefix — a provider can still change it — but it stops the
host from inviting the change itself. An undeclared field and a declared `false` render
the same `SendRelease=no`. Declare `true` for a line that is being handed over or shut
down for good and should free its delegation now rather than when it expires.

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
| `validLifetime` | no | Default `24h` for a literal prefix, `1h` for one cut from an advertised delegation. Not for a delegated prefix. See below |
| `preferredLifetime` | no | Default half of `validLifetime`. Not for a delegated prefix |

The prefix advertised is the one this interface holds. It is not written here, so it
cannot drift from the address that is actually configured.

**The two lifetimes apply only to a prefix regied writes into the advertisement**: one
written as a literal address, and one cut from an
[advertised delegation](#ipv6advertiseddelegation) on a host behind a router. A prefix
that comes from this host's own delegation is announced by the delegation itself, and
networkd takes its lifetimes from the delegation — how long the provider says the prefix
is good for is not something a downstream advertisement may extend, and there is no
directive that overrides it. An interface whose IPv6 address is entirely delegated and
that sets either field is warned about at render time: what was written goes nowhere.

A prefix cut from an advertised delegation is advertised for an hour, and preferred for
half of that, unless the fields say otherwise. networkd re-sends the advertisement every
few minutes while the prefix is current, so the short lifetime costs nothing while it is.
It matters after a change: networkd stops advertising a prefix it is no longer told to,
but does not announce it once more with a lifetime of zero, so the hosts below keep the
old address until the lifetime they last heard runs out
([ADR 0023](../adr/0023-downstream-hosts-derive-their-subnet.md)).

### `ipv6.advertisedDelegation`

Set on the LAN interface of a host behind a router, when that host hands subnets of the
router's delegation to a segment of its own — a container bridge, typically. It states
that the prefix advertised to this link is a subnet of a delegation of the given length.
The host then knows the delegated prefix as well as the router does, and
`fromDelegatedPrefix` naming this interface means here what it means on the router
([ADR 0023](../adr/0023-downstream-hosts-derive-their-subnet.md)).

| Field | Required | Value |
|---|---|---|
| `prefixLength` | yes | The length of the delegation, e.g. `56`. Between 1 and 64 |

The interface must also hold a `fromRouterAdvertisement` address, and must not run a
prefix-delegation client of its own. Every turn reads, from networkd's description of the
link, the address networkd configured from an advertisement with that token, and cuts its
/64 back to `prefixLength`: that is the delegated prefix on this host. Only a global
address counts. A unique local address advertised beside it is not the delegation, and a
temporary address does not carry the token. When the link holds the token in more than
one delegation, which is ordinary for a while after a prefix change, the one networkd
prefers for longest is taken, and the turn says which one it took. Two preferred equally
long cannot be told apart: everything cut from them waits.

`fromDelegatedPrefix` naming this interface is accepted wherever it is accepted on a
router — an interface's address, a route's destination and next hop, an address set's
entry — and resolves the same way, with the same waiting while nothing has been advertised.
The difference is how an interface's address reaches the host. networkd has no delegation
here to derive from, so the address is written into the `.network` file as the value it
resolves to (and so needs a `token`), and `ipv6.advertise` on that interface advertises it
as a literal prefix. When the router moves to another prefix and advertises a subnet of it,
the address taken from the advertisement changes, that wakes a turn, and the turn rewrites
the files and reloads networkd.

The length is the one value here a provider can still break: a provider that changes the
length it delegates moves every derived subnet on this host to the wrong place, and
nothing on this host can tell.

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
| `routes` | no | As on `Interface`, with literals only |

The link is named after `metadata.name`, so other resources and the firewall see a stable
name across redials. That makes the name a kernel interface name, and it is held to the
same 15 characters an `ifname` is.

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
| `localAddressFrom.interfaceRef` | one of | Take the tunnel's local address from this interface's IPv6 address. See below |
| `localAddress` | one of | A literal IPv6 address, for a statically addressed deployment |
| `aftrHost` | one of | The provider's AFTR by name. Resolved at apply time |
| `aftrAddress` | one of | The provider's AFTR as a literal IPv6 address |
| `mtu` | no | Default `1454` |
| `ttl` | no | Default `64` |
| `defaultRoute.install` | no | Default `true` |
| `defaultRoute.metric` | no | Default `0` |
| `routes` | no | As on `Interface`, with literals only |

The tunnel link is named after `metadata.name`, which makes it a kernel interface name
and holds it to the same 15 characters an `ifname` is.

Exactly one of `localAddressFrom` and `localAddress` is required. `localAddressFrom` is
the one to use where the prefix comes from delegation: the tunnel then follows a prefix
change instead of going dark until somebody edits the file.

`localAddressFrom.interfaceRef` names either the underlay or any other `Interface` that
declares exactly one IPv6 address of its own ([ADR 0022](../adr/0022-dslite-local-address-follows-the-delegation.md)):

| The named interface | What the tunnel sends from |
|---|---|
| The underlay itself | networkd's own global address of that link (`Local=slaac`) |
| Another interface whose IPv6 address is `fromDelegatedPrefix` | That address, resolved from the prefix the delegation client holds now |
| Another interface whose IPv6 address is a literal | That literal |

Naming an interface with no IPv6 address, with more than one, with one taken
`fromRouterAdvertisement`, or with a `fromDelegatedPrefix` that has no `token`, is a
validation error. The first two leave the address unstated; the third is a host behind a
router, which has no use for a tunnel of its own; the last is an address networkd makes up
from the link's hardware address, which regied cannot know.

On the platform regied targets, `Local=` in networkd's `[Tunnel]` section cannot take an
address out of a delegated prefix: the value that would, `dhcp_pd`, arrived in systemd 258
and is not on Debian 13 ([ADR 0011](../adr/0011-target-platform.md)). So regied computes
the address itself and writes it into `Local=` as a literal. Every turn reads the prefix
from networkd's description of the link that runs the delegation client
([ADR 0021](../adr/0021-values-derived-from-the-delegated-prefix.md)); when the provider
delegates another prefix, the turn that follows rewrites the `.netdev` and reloads
networkd, and networkd 257 changes the local address of the tunnel that is already up. No
submission is needed, and no link is taken down.

While no delegation is held — after boot, while the client is still soliciting, after a
lease has expired — or while the client holds more than one, or the `subnetID` does not fit
in the prefix delivered, the tunnel's files are left exactly as an earlier turn wrote them,
and the turn ends *waiting*, naming the interface whose delegation it waits for. `regied
render` and `--dry-run` show the prefix that was read and the local address resolved from
it; `regied render` takes the prefix as `-delegated-prefix <Interface>=<prefix>`.

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
It resolves at apply time, and a name that does not resolve — or resolves to no IPv6
address — leaves the tunnel out of that turn rather than failing it. The turn says it is
waiting for the name, and a later one builds the tunnel once the name answers
([ADR 0016](../adr/0016-converging-on-the-accepted-declaration.md)). On a host that has
just booted, the resolver is often the thing that is not up yet.

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
| `addresses` | no | Individual addresses, or `fromDelegatedPrefix` with a `token` |
| `networks` | no | Prefixes, or `fromDelegatedPrefix` without a `token`, which is a /64 of the delegation |

At least one of the two lists must be non-empty. A set holding prefixes becomes an
interval set, whether they are written or derived.

`fromDelegatedPrefix` here is the same as on a route: `interfaceRef` and `subnetID` are
required, the `token` is required in `addresses` and refused in `networks`, and the set must
be `ipv6`. It is how a host behind the router, or a subnet handed to one, is named in a rule
without writing the delegated prefix down
([ADR 0021](../adr/0021-values-derived-from-the-delegated-prefix.md)).

**A set with at least one derived entry has no elements in the ruleset.** The ruleset
declares it empty, and every turn writes all its elements — the literal ones too, so that one
set is never filled from two places — the way it writes an uplink's set
([ADR 0015](../adr/0015-uplink-addresses-in-sets.md)). The ruleset text stays a function of
the declaration, and a prefix change replaces the set's elements without replacing the table.
While no delegation is held, the derived entries are left out and the literal ones are
written; the turn reports that it is waiting, naming the interface. `regied render` and
`--dry-run` show the elements a turn would write beside the ruleset, and each derived entry
with the delegation it was cut from.

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

Backend: a destination-NAT rule, a source-NAT rule for the hairpin case, the connection
mark that puts the reply back on the uplink, with a routing table and a routing policy
rule for it in systemd-networkd, and — unless switched off — the firewall opening that
lets the translated traffic through.

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

**Two forwards may not listen on the same port.** What a forward matches is the uplink in
`egressRef`, the `protocol`, the family of `target.address`, and the port or range it
listens on — the listening address is the uplink's, so there is nothing else. Two
forwards that agree on the uplink, the protocol and the family, and whose ports share at
least one port, are a validation error, reported on the later of the two with the ports
they share. Otherwise the destination-NAT rule evaluated first would take every packet
and the other would never match, so which host is reached would be decided by rule
order, which the file does not say. The same port is fine for `tcp` and `udp`, for an
IPv4 target and an IPv6 one, and on two different uplinks; ranges that only meet end to
end, such as `60000-60004` and `60005-60010`, do not overlap.

A `PortForward` whose `egressRef` names a `DSLiteTunnel` is a validation error: nothing
can be published through it.

**The reply leaves by the uplink the connection arrived on.** regied marks every
connection a forward readdresses with the uplink it arrived on and restores the mark on
the reply before any `EgressRoutePolicy` looks at it, so the target can be any host
inside, whatever uplink its own traffic leaves by. Only connections that arrived on the
uplink are pinned to it: a connection the target starts itself follows the policies as
before ([ADR 0020](../adr/0020-port-forward-reply-on-arrival-uplink.md)).

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

`loopback` means the host itself, and it means it in both directions. dnsmasq answers on
the loopback addresses, and regied points the host's own resolver at it: every `.network`
it writes for an `Interface` that is not a bridge port names `127.0.0.1` as that link's
DNS server and as the route for names no other link claims, which systemd-resolved takes
from networkd. A host whose uplinks refuse the provider's resolvers (`useDNS: false`) is
then not left without one. It rides on every addressed link rather than on the links
`listenOn` names, because resolved uses a link's servers only while that link is up with
carrier and an address, and a bridge nobody is plugged into has no carrier. A declaration
that lists `loopback` but declares no `Interface` that could carry the entry gets a render
warning: dnsmasq will answer on the loopback, and the host will not be told to ask it
([ADR 0018](../adr/0018-loopback-forwarder-is-the-host-resolver.md)).

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

---

## DNSRecordSet

The records regied keeps in one DNS zone, so that a name published through a
`PortForward` resolves to the address the uplink is holding now. On a dynamically
addressed uplink that address changes, and a record still holding the old one makes the
service unreachable from outside until somebody edits it by hand. An IPv6 record holds an
address inside the prefix this host was delegated instead, so that a name published over
IPv6 follows a change of the delegation in the same way
([ADR 0024](../adr/0024-dns-records-inside-the-delegation.md)).

Backend: the provider's HTTP API. It is the only thing regied talks to that is not on
this host, and the only reason the host needs CA certificates
([ADR 0011](../adr/0011-target-platform.md)).

| Field | Required | Value |
|---|---|---|
| `provider` | yes | `cloudflare` |
| `zone` | yes | The zone these records are in, e.g. `example.com` |
| `apiTokenFile` | yes | Path to a file holding the API token to write them with |
| `records` | yes | What regied keeps in this zone. At least one entry. See below |

**This is a set of records *in* a zone, not the zone.** regied writes the names listed
here and knows nothing about the rest of the zone: a record that leaves this list stops
being followed and is not deleted, and no record is marked as regied's
([ADR 0009](../adr/0009-ownership-boundary.md),
[ADR 0019](../adr/0019-uplink-address-in-dns-records.md)).

**One resource per zone is the shape to reach for**, because the token is what binds to a
zone and a token scoped to one zone is the smaller credential. Splitting a shared token
into per-zone tokens is then changing one path in each resource. Two resources naming the
same zone are accepted — they are two sets of records, each with its own token — but two
entries asking for the same name and type are a validation error whether they are in one
resource or two: which of them the record would end up holding is written nowhere.

**`provider` is required, and `cloudflare` is its only value.** There is one
implementation and no interface admitting a second, and writing the name down is what
keeps a declaration valid on the day there is one: the file says what it was talking to,
so a second provider is a new value rather than an edit to every file that already exists.

**`apiTokenFile` names a credential.** Its content never appears in `--dry-run` output, in
a diff, in a log line, or in the report of a turn
([ADR 0003](../adr/0003-secrets-out-of-configuration.md)). It is read on the turn that
writes a record and dropped; a dry run does not read it at all, because a dry run writes
nothing. A file that is missing, unreadable or empty is a validation error.

### `records[]`

| Field | Required | Value |
|---|---|---|
| `names` | yes | Fully qualified names, each inside `zone`. At least one |
| `type` | no | `A`, which is the default, or `AAAA` |
| `egressRef` | `A` only | The uplink these names follow. The address is taken from it. Refused on `AAAA` |
| `address` | `AAAA` only | Where the address is derived from. See below. Refused on `A` |
| `proxied` | no | Default `false`. Cloudflare's proxy flag |
| `ttl` | no | Default `automatic`, which is the provider's own choice. Otherwise a duration |

One entry is one set of properties over several names, which is how a zone's apex and its
wildcard are written: they hold the same address and want the same flags. Each name
becomes its own record at the provider.

**Every name has to be inside the zone** — equal to it, or ending in a dot and it. A name
outside is a validation error rather than a request the provider refuses on every turn.

**An entry is either an `A` record following an uplink, or an `AAAA` record holding a
derived address.** An `A` entry names the uplink in `egressRef`; an `AAAA` entry carries
`address` instead, and the other field is a validation error on each. A name with both an
`A` and an `AAAA` entry is two records, and is fine. Any other type is refused.

**`egressRef` names an uplink, and a `DSLiteTunnel` is a validation error.** The tunnel's
IPv4 is translated by the AFTR, so nothing can be published through it and there is no
address for a record to hold — the same refusal a `PortForward` naming the tunnel gets.

**`proxied` is Cloudflare's**, and it is a plain field rather than something nested in a
per-provider container: one provider does not need a container, and `provider` is what
makes it checkable when there is a second. A proxied name resolves to the provider's own
addresses, and the address regied writes is what the provider sends the traffic on to.

**`ttl` is `automatic` or a duration.** `automatic` hands the choice to the provider,
which is what leaving the field out does as well; writing it says so rather than leaving
a reader to know the default. A duration below one minute or above twenty-four hours is a
validation error, because it is outside what the provider accepts. So is a duration on a record with `proxied: true`: a proxied record's
TTL belongs to the provider, and a declaration setting both asks for something that is
refused on every turn.

### `address`

What an `AAAA` record holds: an address inside the prefix this host was delegated, or
derived for itself from the advertisement it receives, or the address this host takes from
that advertisement ([ADR 0024](../adr/0024-dns-records-inside-the-delegation.md)). It is a
mapping with exactly one of `fromDelegatedPrefix` and `fromRouterAdvertisement` in it.

| Field | Required | Value |
|---|---|---|
| `fromDelegatedPrefix.interfaceRef` | yes | The interface running the prefix-delegation client, or, on a host behind a router, the one declaring [`ipv6.advertisedDelegation`](#ipv6advertiseddelegation) |
| `fromDelegatedPrefix.subnetID` | yes | Which /64 of the delegated prefix |
| `fromDelegatedPrefix.token` | yes | The host part, e.g. `::200` |
| `fromRouterAdvertisement.interfaceRef` | yes | The interface whose address taken from the router advertisement the record holds |

`fromDelegatedPrefix` is the one an interface's address, a route and an address set take
([ADR 0021](../adr/0021-values-derived-from-the-delegated-prefix.md)), with the `token`
required because a record holds an address rather than a subnet. Its `interfaceRef` has to
name an interface with a `dhcpv6.prefixDelegation` client or one declaring
`ipv6.advertisedDelegation`. On a host behind a router, the second is how the names of the
containers behind its bridge are kept: the subnet ID and the token are the ones the
bridge and the address sets already use.

`fromRouterAdvertisement` is how a host behind a router publishes its own address on the
LAN. The record holds the address the named interface takes from the advertisement it
receives, with the token that interface's own
[`fromRouterAdvertisement`](#addresses) address declares; the token is not written again.
The named interface has to hold such an address, and one that does not is a validation
error. The LAN's subnet ID is the router's choice and appears nowhere in this host's
declaration, which is why this is not written as `fromDelegatedPrefix`. While the
interface holds addresses from two advertised prefixes, as it does for a while after a
change, the one preferred longest is taken — the new one — as it is for
`ipv6.advertisedDelegation`.

**There is no literal form.** An `AAAA` record with an address written out is one regied
would have nothing to keep up to date, and the provider's own console is where it belongs.
An `AAAA` following an uplink's own IPv6 address is refused too: the address the outside
world has to reach over IPv6 is inside the delegation, not on the uplink
([ADR 0019](../adr/0019-uplink-address-in-dns-records.md)).

**Which host keeps a name is the host whose declaration decides its address.** A router
keeps the records for itself and for what on its segments does not run regied — a cluster's
load-balancer address, a host configured by something else. A host behind the router that
runs regied keeps the records for itself and for the containers behind its bridge, since
its declaration is where their subnet ID and host parts are written. **One name is written by at
most one host**: two hosts writing the same name would each put back what the other wrote.
Within one declaration that is checked; across hosts it cannot be, and keeping the names
apart is for the repository that holds both declarations.

**A change of the delegation rewrites every `AAAA` record at once, and resolvers keep the
old answer for the TTL.** The default, `automatic`, is short at the provider. A long TTL on
an `AAAA` record is a choice to be unreachable for that long after the prefix changes.

### What a turn does with a record

| When | What happens |
|---|---|
| The uplink holds no address | Every record following it is left exactly as it is, and the turn says what it waits for |
| No delegation is held, or the one held has no room for the subnet ID | Every `AAAA` record cut from it is left exactly as it is, and the turn says what it waits for, naming the interface |
| On a host behind a router, nothing has been taken from the advertisement yet | Every `AAAA` record cut from the advertised delegation or holding the advertised address is left exactly as it is, and the turn says what it waits for, naming the interface |
| The address is not what this process last wrote | The record is written |
| The record is not at the provider | It is created |
| The record is at the provider | Its address and declared properties are updated, and its other fields — a comment somebody wrote on it — are left alone |
| The record was written earlier in this process and the address has not moved | Nothing is asked of the provider |
| The write fails | That record is *failing*, under a backoff of its own. The rest of the turn finishes |

**What decides is what this process last wrote, not what the provider holds.** The
provider is not read on every turn; it is asked once per record, for the identifier that
says whether to create or to update. That memory is in the process and not on disk, so it
is empty after any start — and empty is *not known*, never *the same*, so the first turn
after a start writes every record once. The cost of that is one write per record per
start; the cost of the alternative is one read per record per resync, for an address that
changes a few times a year.

The trade-off is that a record edited at the provider by hand is not put back until the
address changes or regied restarts.

**`--dry-run` names every record it would write and the address it would write, and asks
the provider nothing.** It does not read the token either: what a dry run has to show is
decided by the memory and the kernel, both of which are here
([ADR 0006](../adr/0006-dry-run-and-rendering.md)). In a fresh process that means every
record is shown as one it would write, which is what the turn would do. An `AAAA` record's
address is shown beside the delegation it was cut from, or beside the interface whose
advertisement it was taken from, among the values derived from the delegated prefix.

**Stopping regied stops the updates.** A host whose daemon is stopped keeps whatever its
records last held, which is what the stop lever means.
