# ADR 0011: Build on Debian 13

- Status: Accepted (2026-08-31)

## Context

[ADR 0008](0008-delegate-to-existing-implementations.md) decided which layers to hand to
systemd-networkd, and the seven areas were measured on Ubuntu 26.04. The router is being
built on Debian 13 (trixie) instead, so the measurements need to be restated against the
platform that will actually run them.

Everything regied delegates to — systemd-networkd, dnsmasq, pppd, nftables — is in
trixie's main, so the delegation decision survives the change of distribution. Three
differences are worth writing down.

**There is no netplan.** ADR 0008 argues at length for not going through netplan, and
[ADR 0009](0009-ownership-boundary.md) names it as the other writer that keeps the base
network on a general-purpose host. Neither applies on Debian: `/etc/systemd/network/` has
a single writer from the start, and the ownership boundary gets simpler rather than
harder.

**networkd is not enabled by default.** A Debian installation leaves the network to
ifupdown, or to NetworkManager on a desktop install. Where Ubuntu server arrives with
networkd already running, Debian has to be told.

**One directive we counted on is not there yet.** ADR 0008's table records that `Local=`
in a `[Tunnel]` section accepts `dhcp_pd`, which is how a DS-Lite tunnel would take its
local address out of a delegated prefix. That value arrived in systemd 258. Trixie ships
257, and there is no systemd in trixie-backports, so on this platform `Local=` takes an
address of the underlay: `dhcp6` or `slaac`.

## Decision

**Debian 13 (trixie) is the platform regied is built, tested and deployed against.**

What is assumed present, from the distribution's own packages:

| Assumed | Why |
|---|---|
| systemd 257 or later, with `systemd-networkd` enabled | Links, addresses, routes, policy rules, the ip6tnl tunnel, prefix delegation, router advertisement |
| `dnsmasq` | DHCP server and conditional DNS forwarding |
| `ppp` | The PPPoE uplink |
| `nftables` | Firewall, NAT, and the marking half of policy routing |

Nothing else may own the router's links: `/etc/network/interfaces` is left empty and
NetworkManager is not installed. This is an operator's prerequisite, not something regied
does — see [scope](../scope.md).

The networkd directives regied generates, and therefore the floor under "systemd 257":
`Kind=ip6tnl` with `Mode=ipip6`, `[DHCPPrefixDelegation]` with `SubnetId=` and `Token=`,
`IPv6SendRA=`, and `FirewallMark=` on a routing policy rule. All are in 257.

`DSLiteTunnel.localAddressFrom` therefore renders as the underlay's own global address —
`slaac` or `dhcp6` — and not as an address taken from the delegated prefix. The schema
does not change: it already says *this interface's IPv6 address*, and which of networkd's
special values that becomes belongs to the backend.

## Consequences

- The testbed container and the deployment target become the same distribution. ADR 0010
  records results that differed between a bookworm container and an Ubuntu 26.04 virtual
  machine; the fewer axes along which they differ, the less a green testbed can lie.
  Moving it surfaced one difference at once: with ppp 2.5.2 and pppoe 4.0 the testbed's
  PPPoE server no longer completes LCP in kernel mode, so it runs in user space. All seven
  checks pass on Debian 13.
- Where ADR 0008 and ADR 0009 say "netplan", read "whatever owns the base network on that
  host". The argument never depended on which tool that is, and on a Debian router it is
  nothing.
- A deployment that must derive the tunnel's local address from the delegated prefix is
  not expressible until systemd 258 reaches the platform. If that turns out to be
  necessary rather than merely tidy, it is a reason to revisit the platform, not a reason
  to build the address lifecycle ourselves — that is exactly what ADR 0008 declined.
- Naming a platform says what is assumed present. It does not make regied install,
  upgrade, or provision any of it; that stays out of scope.
