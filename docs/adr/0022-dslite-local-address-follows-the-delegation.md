# ADR 0022: The DS-Lite tunnel's local address follows the delegation

- Status: Accepted (2026-10-23)
- Builds on [ADR 0021](0021-values-derived-from-the-delegated-prefix.md)
- Amends [ADR 0011](0011-target-platform.md) and [ADR 0012](0012-networkd-rendering.md)

## Context

A DS-Lite tunnel sends every IPv4 packet inside IPv6, from a local address of this host.
On a line where the provider delegates a prefix and the upstream interface gets no global
address of its own, the only global addresses the router holds are the ones it cut from
the delegation — typically the `::1` of a downstream subnet. The tunnel has to send from
one of those.

[`DSLiteTunnel.localAddressFrom`](../spec/kinds.md#dslitetunnel) was meant for exactly this:
"take the tunnel's local address from this interface's IPv6 address", so that the tunnel
follows a prefix change. On systemd 257 it cannot. `Local=` in networkd's `[Tunnel]`
section takes either a literal or an address of the link the tunnel is stacked on
(`dhcp4`, `dhcp6`, `slaac`, the two link-local values); the value that takes one out of a
delegated prefix, `dhcp_pd`, arrived in systemd 258 ([ADR 0011](0011-target-platform.md)).
regied therefore renders `localAddressFrom` as the underlay's own address and warns when it
names another interface ([ADR 0012](0012-networkd-rendering.md)). On a line where the
underlay has no global address, the warning is the whole outcome, so the deployment writes
`localAddress` as a literal — and the literal is what stopped the router's IPv4 when the
prefix changed.

The failure is silent in a way worth stating. The kernel keeps an `ip6tnl` link up whose
local address no link holds; every packet is dropped with a kernel log line and nothing
else. The link is up, the route is there, the declaration and the host agree, and every
turn reports converged.

## Decision

### The field keeps its meaning, and regied supplies the address on 257

**`localAddressFrom.interfaceRef` may name any `Interface` with a single IPv6 address of
its own declared, and on systemd 257 regied writes that address into `Local=` as a
literal.** The address is the one the interface's declaration yields:

| The named interface's IPv6 address | What `Local=` gets |
|---|---|
| The underlay itself, as today | `slaac` or `dhcp6`, as today. Unchanged |
| `fromDelegatedPrefix` | The address resolved from the current delegation ([ADR 0021](0021-values-derived-from-the-delegated-prefix.md)) |
| A literal | That literal |

Naming an interface with no IPv6 address, with more than one, or with
`fromRouterAdvertisement`, is a validation error: the first two leave the choice unstated,
and the third is a host that is not the router, which has no use for a DS-Lite tunnel of
its own. The render warning ADR 0012 lists for this field goes away, because naming another
interface now does what it says.

**No field is added.** The schema already says the right thing — *this interface's
address* — and ADR 0011 said that which of networkd's values it becomes belongs to the
backend. What changes is that on this platform the backend's answer is a value regied
computes instead of one networkd names. A worked declaration names the downstream
interface that holds the `::1` of its subnet, and drops `localAddress`.

A `fromDelegatedPrefix` written on the tunnel itself was the other shape. It would state
the subnet and the token a second time, beside the interface that already holds that
address, and two statements of one address are the drift this record exists to remove.

### When the address changes, the tunnel is made to carry it

The resolved address is in the `.netdev`, so a prefix change rewrites that file and the
turn reloads networkd. Whether networkd 257 applies a changed `Local=` to an `ip6tnl` link
that already exists, rather than only to one it creates, is to be confirmed in the testbed
by the first implementation unit; its reload replaces its own record of the netdev, and
that alone does not say the kernel link is changed.

*Confirmed by the first implementation unit on systemd 257.13 (Debian 13): after `Local=`
in an `ip6tnl` netdev is changed and networkd is reloaded, the link already up carries the
new local address, under the same interface index — networkd changes the link in place
rather than creating another. `networkctl reconfigure` is not needed. The recreation below
is therefore not built, and the tier table of [ADR 0016](0016-converging-on-the-accepted-declaration.md)
is unchanged.*

**If it does not, regied removes the tunnel link and lets networkd create it again from
the new file.** That is a link being taken down, and an unattended turn may not take down
something that is up ([ADR 0016](0016-converging-on-the-accepted-declaration.md)). The rule
is narrowed, not broken: **an unattended turn may recreate a tunnel whose current local
address no link on the host holds.** Such a tunnel carries nothing — the kernel drops every
packet it is given — so recreating it takes down nothing that was up. A tunnel whose local
address is still held is left alone by an unattended turn and recreated by a submitted one,
as before.

### While there is no delegation

The tunnel is an artifact that cannot exist without the value
([ADR 0021](0021-values-derived-from-the-delegated-prefix.md)). With no delegation, the
turn leaves the tunnel's files exactly as they are and waits, naming the interface. On a
host that has just booted, the previous tunnel is there with the previous address until the
delegation arrives; if the prefix came back the same, it was never wrong.

### On systemd 258

Whether `Local=dhcp_pd` can express *an address of another link* — the downstream
interface's — or only an address from the delegation assigned to the link the tunnel is
stacked on, is to be checked when 258 reaches the platform. In the second case the literal
regied writes stays the right rendering, not a stopgap: it is the address the declaration
names, and no lifecycle is built around it.

## Alternatives considered

**Assigning a delegated address to the upstream interface too**, so that the existing
`slaac` / `dhcp6` rendering finds one. networkd 257 can assign a delegated subnet to the
upstream link, but the address it takes is the subnet regied would then have to reserve,
and the underlay's own address is chosen by networkd among several. It moves the problem
rather than naming the address.

**A pppd-style hook, or a networkd dispatcher script, that rewrites the tunnel when the
prefix changes.** Rejected for the reason ADR 0015 gives against a second writer: it races
the turn, and it would be the only place the address is computed outside the renderer.

**Watching the tunnel and rebuilding it when traffic stops.** That is health, not drift
([ADR 0016](0016-converging-on-the-accepted-declaration.md)); see
[ADR 0026](0026-no-uplink-failover-inside-regied.md).

## Consequences

- **A prefix change no longer takes the router's IPv4 down** for longer than the turn
  that follows the address event, plus networkd's reload or the recreation.
- **`localAddress` remains for a statically addressed line**, and a deployment with a
  delegation has no reason to use it.
- **An unattended turn gains one narrowly defined recreation**: a tunnel whose local
  address the host no longer holds. It is stated in the tier table when the code lands.
- `--dry-run` shows the resolved local address, as it already shows the AFTR address.

## Implementation units

Units 1 and 2 of [ADR 0021](0021-values-derived-from-the-delegated-prefix.md), as one pull
request: reading the delegation, resolving it, and this field as its first consumer, with
the spec's `DSLiteTunnel` section and ADR 0011's and ADR 0012's notes changed in the same
pull request. The testbed check of whether a reload changes `Local=` on an existing link
decides whether the recreation is built.
