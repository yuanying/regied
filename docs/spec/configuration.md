# Configuration format

The schema of regied's configuration file. The reasoning behind its shape is in
[ADR 0002](../adr/0002-configuration-schema.md); the per-kind field tables are in
[`kinds.md`](kinds.md); a worked example is [`config/example.yaml`](../../config/example.yaml).

日本語版は [docs/ja/spec/configuration.md](../ja/spec/configuration.md) にあります。

> **Status: the schema is settled in shape, not in detail.** The kinds and the division
> of responsibility below are decided. Individual field names may still move before the
> first release, and `v1alpha1` says so.

## The document

One YAML document per host.

| Field | Required | Value |
|---|---|---|
| `apiVersion` | yes | `net.unstable.cloud/v1alpha1` |
| `kind` | yes | `NetworkConfig` |
| `metadata.name` | yes | A name for this host's configuration. Not the hostname |
| `spec.global` | no | Host-wide switches. See below |
| `spec.resources` | yes | The list of resources |

The apiGroup does not contain the project's name on purpose. Renaming the binary must not
invalidate a configuration file.

The document kind carries no role either. A host that only has a firewall writes the same
`NetworkConfig` as a host terminating two uplinks; what a host does is expressed by which
resources it lists, not by the name of the document. No resource kind is mandatory, and
`router` and `firewall` are not part of the type system
([ADR 0009](../adr/0009-ownership-boundary.md)).

Each entry of `spec.resources` has:

| Field | Required | Value |
|---|---|---|
| `kind` | yes | One of the eleven kinds in [`kinds.md`](kinds.md) |
| `metadata.name` | yes | Unique within the kind. Referred to by other resources |
| `spec` | yes | Kind-specific |

Order in the list does not matter. Where evaluation order matters — policy routing, and
rules within a firewall policy — it is expressed by an explicit `priority` field or by
the order of the inner list, never by the order of `spec.resources`.

## References between resources

A resource refers to another by a field named `<kind in lower camel case>Ref` whose value
is the other resource's `metadata.name`. The kind is fixed by the field, so it is not
repeated in the value.

| Field | Points at |
|---|---|
| `interfaceRef`, `underlayRef` | an `Interface` |
| `egressRef` | an uplink: a `PPPoESession` or a `DSLiteTunnel` |
| `linkRefs` | a list of link resources: `Interface`, `PPPoESession`, `DSLiteTunnel` |
| `from`, `to` | a `FirewallZone`, or the reserved name `self` |
| `addressSetRefs` | a list of `IPAddressSet` |

Two collective terms are used throughout. A **link resource** is any kind that puts a
link on the host — `Interface`, `PPPoESession`, `DSLiteTunnel`. An **uplink** is a
`PPPoESession` or a `DSLiteTunnel`: a link resource that leads outward, and therefore one
that NAT and policy routing can point at.

A reference to a resource that does not exist is a validation error. So is a reference
cycle.

## Host-wide switches: `spec.global`

These are kernel settings and one firewall-wide behaviour. They are not resources: there
is exactly one of each per host, and nothing refers to them.

| Field | Default | Effect | Backend |
|---|---|---|---|
| `ipForwarding` | `false` | Forward IPv4 and IPv6 between interfaces | kernel |
| `synCookies` | `true` | SYN flood mitigation | kernel |
| `logMartians` | `false` | Log packets with impossible source addresses | kernel |
| `sendRedirects` | `false` | Send ICMP redirects | kernel |
| `receiveRedirects` | `false` | Accept ICMP redirects | kernel |
| `sourceValidation` | `false` | Reverse path filtering | kernel |
| `mssClamp` | `auto` | Clamp TCP MSS on paths whose MTU is below the local segment's | nftables |

Two of these deserve a note.

**`sourceValidation` defaults to off.** Policy routing makes return paths asymmetric by
design: a reply arrives on the interface the routing table would not have chosen. Strict
reverse path filtering drops exactly that traffic. Turning it on is only safe on a host
with no `EgressRoutePolicy`, and regied rejects the combination.

**`mssClamp: auto`** clamps on every path whose MTU is lower than the segment the traffic
came from, rather than on one named interface type. A tunnel needs it as much as a PPPoE
link does. `off` disables it; a number sets a fixed value on every such path.

Kernel settings are applied at apply time and are recorded so that a failed apply can put
them back. regied does **not** write them into `/etc/sysctl.d/`. A value there would be
applied at boot, which would enable forwarding before the firewall exists.

## What lands where

| Area | Kinds | Backend |
|---|---|---|
| Links, addresses, MTU, bridges | `Interface` | systemd-networkd |
| Static routes | `Interface` | systemd-networkd |
| Prefix delegation, router advertisement | `Interface` | systemd-networkd |
| IPv4-over-IPv6 tunnel | `DSLiteTunnel` | systemd-networkd |
| Routing policy rule and its table | `EgressRoutePolicy` | systemd-networkd |
| PPPoE session | `PPPoESession` | pppd |
| Address handout, DNS | `DHCPServer`, `DNSForwarder` | dnsmasq |
| Firewall, NAT, policy-routing match | `FirewallZone`, `FirewallPolicy`, `IPAddressSet`, `SourceNAT`, `PortForward`, `EgressRoutePolicy` | nftables |
| Kernel switches | `spec.global` | kernel |

regied writes networkd files into `/etc/systemd/network/` under its own prefix, builds
one dnsmasq configuration out of every `DHCPServer` and `DNSForwarder`, and rebuilds only
its own nftables table. It never flushes the ruleset and never rewrites a file it did not
create ([ADR 0009](../adr/0009-ownership-boundary.md)).

## Secrets

No field in this schema holds a secret. Credentials are named by the path of a file that
contains them, and that file lives outside the configuration
([ADR 0003](../adr/0003-secrets-out-of-configuration.md)). The PPPoE user ID counts as a
credential. A referenced file that is missing or unreadable is a validation error.

## Derived values

Some things the operator would otherwise have to keep consistent by hand are derived.

| Derived | From | Can be pinned |
|---|---|---|
| Routing table number | an `EgressRoutePolicy` | yes, `spec.table` |
| Firewall mark | an `EgressRoutePolicy` | yes, `spec.mark` |
| The default route inside a policy's table | the policy's `egressRef` | no |
| The routing policy rule mapping mark to table | the policy | no |
| The firewall opening for a port forward | `PortForward` | yes, `spec.openFirewall: false` |
| The stateful accept and invalid drop at the top of a policy | `FirewallPolicy` | yes, `spec.stateful: false` |

Derived numbers are visible in `regied render`. Nothing outside regied should depend on
them; they are an implementation detail of how a match becomes a route.

Table numbers are allocated from 100 upward and marks from 0x100 upward. Allocation runs
in the order the policies are evaluated in — by family, then by priority — and not in the
order the document happens to list them, so moving a resource within the file changes no
number and an apply that changed nothing changes nothing. A pinned value is left where it
was put and the allocation works around it. The tables the kernel keeps for itself, `main`,
`local` and `default`, are refused as pins.

Those two starting points are where the allocation begins, not a promise about what any
particular policy gets. The sentence above still holds.

## How the pieces fit for a two-uplink host

The configuration expresses a decision about traffic, and the decision reaches the kernel
in two halves.

1. `EgressRoutePolicy` describes *which traffic* — source ranges, and destinations that
   must be excluded. That half is a match, and it becomes an nftables rule that sets a
   mark.
2. The same resource names *which uplink*. That half becomes a routing table containing a
   default route to that uplink, and a routing policy rule selecting the table by mark.

The chain that sets the mark runs **after** nat prerouting. That ordering is what makes
hairpin work: a LAN host connecting to a published service through the uplink's global
address has already had its destination rewritten to the internal address by the time the
mark is considered, so it matches the policy's local-destination exclusion and is routed
locally. The uplink's global address never has to be written down, which is why no field
in this schema accepts one — `PortForward` and `SourceNAT` take an `egressRef`.

## Validation

Beyond references resolving and required fields being present, regied rejects:

- `sourceValidation: true` together with any `EgressRoutePolicy`
- two `EgressRoutePolicy` resources with the same `priority` and the same family
- two `FirewallPolicy` resources with the same `from` and `to`
- an `Interface` whose `ifname` appears in another interface's `bridge.members` and
  that also carries an address
- an address derived from a delegated prefix whose upstream interface has no
  prefix-delegation client
- a `PortForward` or `SourceNAT` whose `egressRef` names a `DSLiteTunnel`, which
  translates at the far end and cannot publish anything inbound
- a `PortForward` whose target covers a different number of ports from the range it
  listens on
- a `PortForward` whose `protocol` is anything other than `tcp` or `udp` written by name
- an `EgressRoutePolicy` whose `sourceRanges` holds a bare address rather than a CIDR or
  an inclusive range
- a `DSLiteTunnel` carrying both `aftrHost` and `aftrAddress`
- a `DSLiteTunnel` carrying neither
- a second `DNSForwarder`. One dnsmasq serves the host, and it has one cache and one set
  of upstreams
- a `FirewallZone` named `self`
- a `FirewallPolicy` whose `from` is `self`
- a secret file that is missing, unreadable, or empty

It warns, and continues, about:

- an `Interface` carrying `dhcpv6.prefixDelegation` with no `duidFile`. A line being
  brought up for the first time has no DUID to carry over, which is a legitimate
  configuration. A host replacing one that already holds a delegation and omitting it
  will quietly be delegated a different prefix.
